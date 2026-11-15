// monofs-loadgen generates artificial MonoFS gRPC load and reports client-side
// latency/throughput. It is intentionally dependency-light: it talks to the
// same router and node RPCs the userspace client uses.
//
// Subcommands:
//
//	read     read-only file workload (small or large reads)
//	meta     lookup/getattr/readdir workload
//	ingest   synthetic metadata ingest via IngestFileBatch
//	mixed    read + meta mix
//	cleanup  delete a synthetic repository from every node
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

var (
	loadgenDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "loadgen_request_duration_seconds",
		Help:    "Client-side request latency by scenario, op and status.",
		Buckets: []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"scenario", "op", "status"})
	loadgenOps = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "loadgen_ops_total",
		Help: "Completed operations by scenario, op and status.",
	}, []string{"scenario", "op", "status"})
	loadgenBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "loadgen_bytes_total",
		Help: "Bytes transferred by scenario and op.",
	}, []string{"scenario", "op"})
	loadgenInflight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "loadgen_inflight",
		Help: "In-flight requests by scenario.",
	}, []string{"scenario"})
)

type options struct {
	router      string
	external    bool
	nodeMap     string
	nodeFilter  string
	concurrency int
	duration    time.Duration
	warmup      time.Duration
	rate        float64
	timeout     time.Duration
	repos       string
	exclude     string
	maxFiles    int
	readSize    int64
	large       bool
	largeSize   int64
	metaOp      string
	seed        uint64
	storageID   string
	displayPath string
	batch       int
	files       int
	cleanup     bool
	jsonOut     string
	metricsAddr string
	label       string
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var err error
	switch os.Args[1] {
	case "read":
		err = runRead(ctx, os.Args[2:], false)
	case "meta":
		err = runRead(ctx, os.Args[2:], true)
	case "mixed":
		err = runMixed(ctx, os.Args[2:])
	case "ingest":
		err = runIngest(ctx, os.Args[2:])
	case "cleanup":
		err = runCleanup(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `monofs-loadgen <read|meta|mixed|ingest|cleanup> [flags]

Common flags:
  -router string          router gRPC address (default "router-a.monofs.svc.cluster.local:9090")
  -external               rewrite *.svc.cluster.local node addresses to 127.0.0.1 (host smoke tests)
  -node-map string        comma-separated node overrides, e.g. node-a=127.0.0.1:9006
  -concurrency int        concurrent workers (default 16)
  -duration duration      measurement window (default 1m)
  -warmup duration        warm-up window before measurement (default 10s)
  -rate float             target ops/s across workers; 0 = closed loop
  -timeout duration       per-op timeout (default 5s)
  -repos string           comma-separated display-path prefixes to read (default all)
  -max-files int          max files discovered per node (default 20000)
  -read-size int          bytes requested per read (default 4096)
  -large                  restrict discovery to _blobs/ paths
  -large-size int         bytes requested per large read (default 1MiB)
  -meta-op string         lookup|getattr|readdir|mix (default mix)
  -seed int               random seed
  -label string           scenario label for metrics/results
  -json string            write the result JSON to this path
  -metrics-addr string    Prometheus listen address (default ":9100"; empty disables)
`)
}

func parseCommon(name string, args []string) (*options, *flag.FlagSet) {
	o := &options{}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&o.router, "router", envOr("MONOFS_ROUTER", "router-a.monofs.svc.cluster.local:9090"), "router gRPC address")
	fs.BoolVar(&o.external, "external", false, "rewrite cluster DNS node addresses to 127.0.0.1")
	fs.StringVar(&o.nodeMap, "node-map", "", "node overrides: node-a=host:port,...")
	fs.StringVar(&o.nodeFilter, "node", "", "comma-separated node IDs to include")
	fs.IntVar(&o.concurrency, "concurrency", 16, "concurrent workers")
	fs.DurationVar(&o.duration, "duration", time.Minute, "measurement duration")
	fs.DurationVar(&o.warmup, "warmup", 10*time.Second, "warm-up duration")
	fs.Float64Var(&o.rate, "rate", 0, "target ops/s (0 = closed loop)")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Second, "per-op timeout")
	fs.StringVar(&o.repos, "repos", "", "comma-separated display-path prefixes")
	fs.StringVar(&o.exclude, "exclude", "", "comma-separated path substrings to skip")
	fs.IntVar(&o.maxFiles, "max-files", 20000, "max discovered files per node")
	fs.Int64Var(&o.readSize, "read-size", 4096, "bytes requested per read")
	fs.BoolVar(&o.large, "large", false, "restrict discovery to _blobs/ paths")
	fs.Int64Var(&o.largeSize, "large-size", 1<<20, "bytes requested per large read")
	fs.StringVar(&o.metaOp, "meta-op", "mix", "lookup|getattr|readdir|mix")
	fs.Uint64Var(&o.seed, "seed", uint64(time.Now().UnixNano()), "random seed")
	fs.StringVar(&o.label, "label", name, "scenario label")
	fs.StringVar(&o.jsonOut, "json", "", "result JSON output path")
	fs.StringVar(&o.metricsAddr, "metrics-addr", ":9100", "Prometheus listen address")
	fs.StringVar(&o.storageID, "storage-id", "", "synthetic repository storage id")
	fs.StringVar(&o.displayPath, "display-path", "", "synthetic repository display path")
	fs.IntVar(&o.batch, "batch", 100, "files per ingest batch")
	fs.IntVar(&o.files, "files", 0, "total files to ingest (0 = unlimited until duration)")
	fs.BoolVar(&o.cleanup, "cleanup", false, "delete the synthetic repository after the run")
	return o, fs
}

func runRead(ctx context.Context, args []string, metaOnly bool) error {
	o, fs := parseCommon("read", args)
	fs.Parse(args)
	if metaOnly {
		o.label = "meta"
	}
	if err := o.validate(); err != nil {
		return err
	}
	startMetrics(o.metricsAddr)

	cluster, err := discover(ctx, o)
	if err != nil {
		return err
	}
	defer cluster.close()

	var refs []fileRef
	for _, n := range cluster.nodes {
		for _, r := range n.repos {
			if !repoMatches(o.repos, r.displayPath) {
				continue
			}
			if len(n.files) >= o.maxFiles {
				break
			}
			resp, err := n.client.GetRepositoryFiles(ctx, &pb.GetRepositoryFilesRequest{StorageId: r.storageID})
			if err != nil {
				fmt.Fprintf(os.Stderr, "discover %s/%s: %v\n", n.id, r.displayPath, err)
				continue
			}
			for _, f := range resp.GetFiles() {
				if o.large && !strings.Contains(f, "_blobs/") {
					continue
				}
				fullPath := joinPath(r.displayPath, f)
				if pathExcluded(o.exclude, fullPath) {
					continue
				}
				refs = append(refs, fileRef{
					nodeID: n.id,
					node:   n,
					path:   fullPath,
				})
				if len(n.files) >= o.maxFiles {
					break
				}
				n.files = append(n.files, f)
			}
		}
	}
	if len(refs) == 0 {
		return errors.New("no files discovered; check -repos/-large filters")
	}
	fmt.Printf("discovered %d files across %d nodes\n", len(refs), len(cluster.nodes))

	scenario := o.label
	if scenario == "" {
		scenario = "read"
	}
	if metaOnly {
		scenario = "meta"
	}

	result := newResult(scenario, o)
	measure(ctx, o, scenario, func(ctx context.Context, w *workerState) error {
		ref := refs[w.rng.IntN(len(refs))]
		if metaOnly {
			return doMeta(ctx, w, ref, o)
		}
		return doRead(ctx, w, ref, o)
	}, result)

	return finish(o, result)
}

func runMixed(ctx context.Context, args []string) error {
	o, fs := parseCommon("mixed", args)
	fs.Parse(args)
	if err := o.validate(); err != nil {
		return err
	}
	startMetrics(o.metricsAddr)

	cluster, err := discover(ctx, o)
	if err != nil {
		return err
	}
	defer cluster.close()

	var refs []fileRef
	for _, n := range cluster.nodes {
		for _, r := range n.repos {
			if !repoMatches(o.repos, r.displayPath) {
				continue
			}
			resp, err := n.client.GetRepositoryFiles(ctx, &pb.GetRepositoryFilesRequest{StorageId: r.storageID})
			if err != nil {
				continue
			}
			for _, f := range resp.GetFiles() {
				fullPath := joinPath(r.displayPath, f)
				if pathExcluded(o.exclude, fullPath) {
					continue
				}
				refs = append(refs, fileRef{nodeID: n.id, node: n, path: fullPath})
				if len(n.files) >= o.maxFiles {
					break
				}
				n.files = append(n.files, f)
			}
		}
	}
	if len(refs) == 0 {
		return errors.New("no files discovered")
	}
	fmt.Printf("discovered %d files across %d nodes\n", len(refs), len(cluster.nodes))

	result := newResult("mixed", o)
	measure(ctx, o, "mixed", func(ctx context.Context, w *workerState) error {
		ref := refs[w.rng.IntN(len(refs))]
		if w.rng.IntN(100) < 80 {
			return doRead(ctx, w, ref, o)
		}
		return doMeta(ctx, w, ref, o)
	}, result)
	return finish(o, result)
}

func runIngest(ctx context.Context, args []string) error {
	o, fs := parseCommon("ingest", args)
	fs.Parse(args)
	if err := o.validate(); err != nil {
		return err
	}
	startMetrics(o.metricsAddr)
	cluster, err := discover(ctx, o)
	if err != nil {
		return err
	}
	defer cluster.close()

	runID := time.Now().UTC().Format("20060102-150405")
	if o.storageID == "" {
		o.storageID = "perf-loadgen-" + runID
	}
	if o.displayPath == "" {
		o.displayPath = "perf/loadgen-" + runID
	}
	for _, n := range cluster.nodes {
		_, err := n.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
			StorageId:     o.storageID,
			DisplayPath:   o.displayPath,
			Source:        "perf://loadgen",
			IngestionType: pb.IngestionType_INGESTION_FILE,
			FetchType:     pb.SourceType_SOURCE_TYPE_UNKNOWN,
			CommitMessage: "synthetic perf ingest",
			CommitTime:    time.Now().Unix(),
		})
		if err != nil {
			return fmt.Errorf("register repository on %s: %w", n.id, err)
		}
	}

	var seq atomic.Int64
	result := newResult("ingest", o)
	measure(ctx, o, "ingest", func(ctx context.Context, w *workerState) error {
		if o.files > 0 && seq.Load() >= int64(o.files) {
			return errStop
		}
		batch := o.batch
		if batch <= 0 {
			batch = 1
		}
		files := make([]*pb.FileMetadata, 0, batch)
		for i := 0; i < batch; i++ {
			n := seq.Add(1)
			if o.files > 0 && n > int64(o.files) {
				break
			}
			path := fmt.Sprintf("dir-%02d/file-%08d.bin", n/1000, n)
			files = append(files, &pb.FileMetadata{
				Path:        path,
				Ref:         "main",
				Size:        uint64(o.readSize),
				Mode:        0644,
				Mtime:       time.Now().Unix(),
				BlobHash:    fmt.Sprintf("perf-blob-%d", n),
				Source:      "perf://loadgen",
				StorageId:   o.storageID,
				DisplayPath: o.displayPath,
				SourceType:  pb.IngestionType_INGESTION_FILE,
				FetchType:   pb.SourceType_SOURCE_TYPE_UNKNOWN,
			})
		}
		if len(files) == 0 {
			return errStop
		}
		target := cluster.nodes[w.id%len(cluster.nodes)]
		start := time.Now()
		_, err := target.client.IngestFileBatch(ctx, &pb.IngestFileBatchRequest{
			Files:       files,
			StorageId:   o.storageID,
			DisplayPath: o.displayPath,
			Source:      "perf://loadgen",
			Ref:         "main",
		})
		w.observe("ingest_batch", start, err, int64(len(files))*o.readSize)
		return err
	}, result)

	if o.cleanup {
		if err := cleanupRepo(ctx, cluster, o.storageID); err != nil {
			fmt.Fprintf(os.Stderr, "cleanup: %v\n", err)
		}
	}
	return finish(o, result)
}

func runCleanup(ctx context.Context, args []string) error {
	o, fs := parseCommon("cleanup", args)
	fs.Parse(args)
	if o.storageID == "" {
		return errors.New("-storage-id is required")
	}
	cluster, err := discover(ctx, o)
	if err != nil {
		return err
	}
	defer cluster.close()
	return cleanupRepo(ctx, cluster, o.storageID)
}

func cleanupRepo(ctx context.Context, cluster *clusterState, storageID string) error {
	var wg sync.WaitGroup
	errs := make([]error, len(cluster.nodes))
	for i, n := range cluster.nodes {
		wg.Add(1)
		go func(i int, n *nodeTarget) {
			defer wg.Done()
			_, err := n.client.DeleteRepository(ctx, &pb.DeleteRepositoryOnNodeRequest{StorageId: storageID})
			errs[i] = err
		}(i, n)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// discovery

type repoRef struct {
	storageID   string
	displayPath string
}

type nodeTarget struct {
	id     string
	addr   string
	conn   *grpc.ClientConn
	client pb.MonoFSClient
	repos  []repoRef
	files  []string
}

type clusterState struct {
	nodes []*nodeTarget
}

func (c *clusterState) close() {
	for _, n := range c.nodes {
		_ = n.conn.Close()
	}
}

func discover(ctx context.Context, o *options) (*clusterState, error) {
	routerConn, err := dial(ctx, o.router)
	if err != nil {
		return nil, fmt.Errorf("dial router %s: %w", o.router, err)
	}
	router := pb.NewMonoFSRouterClient(routerConn)
	info, err := router.GetClusterInfo(ctx, &pb.ClusterInfoRequest{UseExternalAddresses: o.external})
	if err != nil {
		_ = routerConn.Close()
		return nil, fmt.Errorf("GetClusterInfo: %w", err)
	}

	overrides := parseNodeMap(o.nodeMap)
	cluster := &clusterState{}
	for _, ni := range info.GetNodes() {
		if o.nodeFilter != "" && !containsCSV(o.nodeFilter, ni.GetNodeId()) {
			continue
		}
		addr := ni.GetAddress()
		if o.external {
			addr = rewriteExternal(addr)
		}
		if v, ok := overrides[ni.GetNodeId()]; ok {
			addr = v
		}
		conn, err := dial(ctx, addr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dial node %s (%s): %v\n", ni.GetNodeId(), addr, err)
			continue
		}
		n := &nodeTarget{id: ni.GetNodeId(), addr: addr, conn: conn, client: pb.NewMonoFSClient(conn)}
		cluster.nodes = append(cluster.nodes, n)
		if err := discoverRepos(ctx, n); err != nil {
			fmt.Fprintf(os.Stderr, "list repos on %s: %v\n", n.id, err)
		}
	}
	_ = routerConn.Close()
	if len(cluster.nodes) == 0 {
		return nil, errors.New("no reachable nodes")
	}
	return cluster, nil
}

func discoverRepos(ctx context.Context, n *nodeTarget) error {
	var token string
	for {
		resp, err := n.client.ListRepositories(ctx, &pb.ListRepositoriesRequest{
			PageSize:       500,
			PageToken:      token,
			IncludeDetails: true,
		})
		if err != nil {
			return err
		}
		for _, r := range resp.GetRepositories() {
			if r.GetDisplayPath() == "" {
				continue
			}
			n.repos = append(n.repos, repoRef{storageID: r.GetStorageId(), displayPath: r.GetDisplayPath()})
		}
		if resp.GetNextPageToken() == "" {
			return nil
		}
		token = resp.GetNextPageToken()
	}
}

func dial(ctx context.Context, addr string) (*grpc.ClientConn, error) {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return grpc.DialContext(dctx, addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(256<<20)),
	)
}

func containsCSV(csv, want string) bool {
	for _, v := range strings.Split(csv, ",") {
		if strings.TrimSpace(v) == want {
			return true
		}
	}
	return false
}

func parseNodeMap(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

func rewriteExternal(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if strings.HasSuffix(host, ".svc.cluster.local") || strings.Contains(host, ".monofs.svc") {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return addr
}

func pathExcluded(exclude, p string) bool {
	for _, ex := range strings.Split(exclude, ",") {
		ex = strings.TrimSpace(ex)
		if ex != "" && strings.Contains(p, ex) {
			return true
		}
	}
	return false
}

func repoMatches(filter, displayPath string) bool {
	if strings.TrimSpace(filter) == "" {
		return true
	}
	for _, p := range strings.Split(filter, ",") {
		p = strings.TrimSpace(p)
		if p != "" && strings.HasPrefix(displayPath, p) {
			return true
		}
	}
	return false
}

func joinPath(display, file string) string {
	display = strings.Trim(display, "/")
	file = strings.TrimPrefix(file, "/")
	if display == "" {
		return file
	}
	return display + "/" + file
}

// ---------------------------------------------------------------------------
// workload

var errStop = errors.New("loadgen: stop")

type fileRef struct {
	nodeID string
	node   *nodeTarget
	path   string
}

type workerState struct {
	id            int
	rng           *rand.Rand
	samples       []float64
	ops           int64
	errs          int64
	bytes         int64
	measured      int64
	measuredErrs  int64
	measuredBytes int64
	mu            sync.Mutex
}

func (w *workerState) observe(op string, start time.Time, err error, nbytes int64) {
	d := time.Since(start).Seconds()
	code := "ok"
	if err != nil {
		code = status.Code(err).String()
		if code == "OK" {
			code = "error"
		}
	}
	scenario := scenarioLabel(w)
	scenarioCounter(&scenarioOps, scenario).Add(1)
	scenarioCounter(&scenarioBytes, scenario).Add(nbytes)
	if err != nil {
		scenarioCounter(&scenarioErrors, scenario).Add(1)
		scenarioCounter(&errorCodes, scenario+"|"+code).Add(1)
	}
	loadgenDuration.WithLabelValues(scenario, op, code).Observe(d)
	loadgenOps.WithLabelValues(scenario, op, code).Inc()
	if nbytes > 0 {
		loadgenBytes.WithLabelValues(scenario, op).Add(float64(nbytes))
	}
	w.mu.Lock()
	w.ops++
	if err != nil {
		w.errs++
	}
	w.bytes += nbytes
	measuring := measurementStart.Load()
	if measuring > 0 && time.Now().UnixNano() >= measuring {
		w.measured++
		w.measuredBytes += nbytes
		if err != nil {
			w.measuredErrs++
		}
		if len(w.samples) < 200000 {
			w.samples = append(w.samples, d)
		}
	}
	w.mu.Unlock()
}

var measurementStart atomic.Int64

var (
	currentScenario atomic.Value
	scenarioOps     sync.Map // scenario -> *atomic.Int64
	scenarioErrors  sync.Map // scenario -> *atomic.Int64
	errorCodes      sync.Map // scenario|code -> *atomic.Int64
	scenarioBytes   sync.Map // scenario -> *atomic.Int64
)

func scenarioLabel(_ *workerState) string {
	if v := currentScenario.Load(); v != nil {
		return v.(string)
	}
	return "unknown"
}

func scenarioCounter(m *sync.Map, scenario string) *atomic.Int64 {
	if v, ok := m.Load(scenario); ok {
		return v.(*atomic.Int64)
	}
	c := &atomic.Int64{}
	actual, _ := m.LoadOrStore(scenario, c)
	return actual.(*atomic.Int64)
}

func doRead(ctx context.Context, w *workerState, ref fileRef, o *options) error {
	size := o.readSize
	op := "read"
	if o.large {
		size = o.largeSize
		op = "read_large"
	}
	cctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	start := time.Now()
	stream, err := ref.node.client.Read(cctx, &pb.ReadRequest{
		Path:   ref.path,
		Offset: 0,
		Size:   size,
	})
	if err != nil {
		if ctx.Err() != nil {
			return errStop
		}
		w.observe(op, start, err, 0)
		return err
	}
	var total int64
	for {
		chunk, rerr := stream.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return errStop
			}
			w.observe(op, start, rerr, total)
			return rerr
		}
		total += int64(len(chunk.GetData()))
	}
	w.observe(op, start, nil, total)
	return nil
}

func doMeta(ctx context.Context, w *workerState, ref fileRef, o *options) error {
	parent, name := splitPath(ref.path)
	mode := o.metaOp
	if mode == "mix" {
		switch w.rng.IntN(3) {
		case 0:
			mode = "lookup"
		case 1:
			mode = "getattr"
		default:
			mode = "readdir"
		}
	}
	cctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	start := time.Now()
	switch mode {
	case "lookup":
		_, err := ref.node.client.Lookup(cctx, &pb.LookupRequest{ParentPath: parent, Name: name})
		if ctx.Err() != nil {
			return errStop
		}
		w.observe("lookup", start, err, 0)
		return err
	case "getattr":
		_, err := ref.node.client.GetAttr(cctx, &pb.GetAttrRequest{Path: ref.path})
		if ctx.Err() != nil {
			return errStop
		}
		w.observe("getattr", start, err, 0)
		return err
	default:
		stream, err := ref.node.client.ReadDir(cctx, &pb.ReadDirRequest{Path: parent, Limit: 100})
		if err != nil {
			if ctx.Err() != nil {
				return errStop
			}
			w.observe("readdir", start, err, 0)
			return err
		}
		for {
			_, rerr := stream.Recv()
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				if ctx.Err() != nil {
					return errStop
				}
				w.observe("readdir", start, rerr, 0)
				return rerr
			}
		}
		w.observe("readdir", start, nil, 0)
		return nil
	}
}

func splitPath(p string) (string, string) {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

type opFunc func(context.Context, *workerState) error

func measure(ctx context.Context, o *options, scenario string, op opFunc, result *result) {
	currentScenario.Store(scenario)
	runCtx, cancel := context.WithTimeout(ctx, o.warmup+o.duration)
	defer cancel()

	workers := make([]*workerState, o.concurrency)
	for i := range workers {
		workers[i] = &workerState{id: i, rng: rand.New(rand.NewPCG(o.seed, uint64(i+1)))}
	}

	var wg sync.WaitGroup
	start := time.Now()
	measureStart := start.Add(o.warmup)
	measurementStart.Store(measureStart.UnixNano())
	for _, w := range workers {
		wg.Add(1)
		go func(w *workerState) {
			defer wg.Done()
			interval := time.Duration(0)
			if o.rate > 0 {
				interval = time.Duration(float64(time.Second) * float64(o.concurrency) / o.rate)
			}
			var next time.Time
			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}
				now := time.Now()
				loadgenInflight.WithLabelValues(scenario).Inc()
				_ = op(runCtx, w)
				loadgenInflight.WithLabelValues(scenario).Dec()
				if interval > 0 {
					next = next.Add(interval)
					if next.IsZero() || next.Before(now) {
						next = now.Add(interval)
					}
					select {
					case <-runCtx.Done():
						return
					case <-time.After(time.Until(next)):
					}
				}
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(measureStart)
	if elapsed <= 0 {
		elapsed = time.Since(start)
	}
	result.DurationSeconds = elapsed.Seconds()
	var samples []float64
	for _, w := range workers {
		w.mu.Lock()
		samples = append(samples, w.samples...)
		w.mu.Unlock()
	}
	sort.Float64s(samples)
	result.Samples = len(samples)
	result.P50MS = pct(samples, 0.50)
	result.P95MS = pct(samples, 0.95)
	result.P99MS = pct(samples, 0.99)
	result.MaxMS = pct(samples, 1.0)
	for _, w := range workers {
		w.mu.Lock()
		result.Ops += w.measured
		result.Errors += w.measuredErrs
		result.Bytes += w.measuredBytes
		w.mu.Unlock()
	}
}

func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)-1))
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i] * 1000
}

// ---------------------------------------------------------------------------
// result + helpers

type result struct {
	Scenario        string           `json:"scenario"`
	StartedAt       string           `json:"started_at"`
	DurationSeconds float64          `json:"duration_seconds"`
	Ops             int64            `json:"ops"`
	OpsPerSec       float64          `json:"ops_per_sec"`
	Bytes           int64            `json:"bytes"`
	MBPerSec        float64          `json:"mb_per_sec"`
	Errors          int64            `json:"errors"`
	ErrorRate       float64          `json:"error_rate"`
	ErrorBreakdown  map[string]int64 `json:"error_breakdown,omitempty"`
	Samples         int              `json:"samples"`
	P50MS           float64          `json:"p50_ms"`
	P95MS           float64          `json:"p95_ms"`
	P99MS           float64          `json:"p99_ms"`
	MaxMS           float64          `json:"max_ms"`
	Config          map[string]any   `json:"config"`
}

func newResult(scenario string, o *options) *result {
	return &result{
		Scenario:  scenario,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Config: map[string]any{
			"concurrency": o.concurrency,
			"duration":    o.duration.String(),
			"warmup":      o.warmup.String(),
			"rate":        o.rate,
			"read_size":   o.readSize,
			"large":       o.large,
			"repos":       o.repos,
		},
	}
}

func finish(o *options, r *result) error {
	if r.DurationSeconds > 0 {
		r.OpsPerSec = float64(r.Ops) / r.DurationSeconds
		r.MBPerSec = float64(r.Bytes) / r.DurationSeconds / (1 << 20)
	}
	if r.Ops > 0 {
		r.ErrorRate = float64(r.Errors) / float64(r.Ops)
	}
	r.ErrorBreakdown = map[string]int64{}
	errorCodes.Range(func(k, v any) bool {
		key := k.(string)
		prefix := r.Scenario + "|"
		if strings.HasPrefix(key, prefix) {
			r.ErrorBreakdown[strings.TrimPrefix(key, prefix)] = v.(*atomic.Int64).Load()
		}
		return true
	})
	if o.jsonOut != "" {
		b, _ := json.MarshalIndent(r, "", "  ")
		if err := os.WriteFile(o.jsonOut, b, 0644); err != nil {
			return err
		}
	}
	fmt.Printf("\n%s: %d ops, %.2f ops/s, p50=%.2fms p95=%.2fms p99=%.2fms, errors=%d (%.3f%%)\n",
		r.Scenario, r.Ops, r.OpsPerSec, r.P50MS, r.P95MS, r.P99MS, r.Errors, r.ErrorRate*100)
	return nil
}

func startMetrics(addr string) {
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	go func() {
		_ = http.ListenAndServe(addr, mux)
	}()
}

func (o *options) validate() error {
	if o.concurrency <= 0 {
		return errors.New("-concurrency must be > 0")
	}
	if o.duration <= 0 {
		return errors.New("-duration must be > 0")
	}
	if o.rate < 0 {
		return errors.New("-rate must be >= 0")
	}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

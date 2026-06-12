// MonoFS Server - gRPC backend for MonoFS filesystem
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/diagnostics"
	"github.com/radryc/monofs/internal/fetcher"
	"github.com/radryc/monofs/internal/server"
	"github.com/radryc/monofs/internal/storage/logengine"
	"github.com/radryc/monofs/internal/telemetry"
	kvsgrpc "github.com/rydzu/ainfra/kvs/pkg/grpcserver"
	"github.com/rydzu/ainfra/kvs/pkg/kvsapi"
	"github.com/rydzu/ainfra/kvs/pkg/raftstore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
)

var (
	// Version information (injected at build time)
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// stringSlice is a flag that can be specified multiple times
type stringSlice []string

func (s *stringSlice) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// kvsOffloaderAdapter implements kvsapi.FetcherOffloader using a fetcher.Client,
// routing KVS archive blobs through the fetcher tier (and on to MinIO).
type kvsOffloaderAdapter struct {
	client *fetcher.Client
}

func (a *kvsOffloaderAdapter) StoreBlob(ctx context.Context, blobHash string, content []byte) error {
	_, failed, err := a.client.StoreBlobBatch(ctx, map[string][]byte{blobHash: content})
	if err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("fetcher rejected blob upload")
	}
	return nil
}

func (a *kvsOffloaderAdapter) FetchBlob(ctx context.Context, blobHash string) ([]byte, error) {
	req := &fetcher.FetchRequest{
		ContentID: blobHash,
		SourceKey: "kvs-archive",
		Priority:  3,
	}
	return a.client.FetchBlob(ctx, req, fetcher.SourceTypeBlob)
}

var _ kvsapi.FetcherOffloader = (*kvsOffloaderAdapter)(nil)

func main() {
	addr := flag.String("addr", ":9000", "Server listen address")
	nodeID := flag.String("node-id", "", "Unique node identifier (required)")
	routerAddr := flag.String("router", "", "Router address for failover coordination (optional)")
	dbPath := flag.String("db-path", "/tmp/monofs-db", "NutsDB database path")
	gitCache := flag.String("git-cache", "/tmp/monofs-git-cache", "Git repository cache directory")
	debug := flag.Bool("debug", false, "Enable debug logging (shorthand for --log-level=debug)")
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")

	// Fetcher configuration
	var fetcherAddrs stringSlice
	flag.Var(&fetcherAddrs, "fetcher", "Fetcher service address (can be specified multiple times)")
	enablePrediction := flag.Bool("enable-prediction", false, "Enable access pattern prediction and prefetching")

	// Embedded KVS configuration
	kvsDataDir := flag.String("kvs-data-dir", "", "Embedded KVS data directory (empty disables KVS-backed repositories)")
	kvsAPIAddr := flag.String("kvs-api-addr", "", "Dialable gRPC address for this node's embedded KVS service (defaults to --addr)")
	kvsRaftAddr := flag.String("kvs-raft-addr", "", "Raft address for the embedded KVS store (empty keeps the embedded KVS store local-only)")
	kvsRaftAdvertiseAddr := flag.String("kvs-raft-advertise-addr", "", "Advertised Raft address for the embedded KVS store (defaults to --kvs-raft-addr)")
	kvsBootstrap := flag.Bool("kvs-bootstrap", false, "Bootstrap this node as the initial embedded KVS raft cluster member")
	kvsMaxHotVersions := flag.Int("kvs-max-hot-versions", 5, "Maximum number of hot versions retained in the embedded KVS store")
	var kvsPeers stringSlice
	flag.Var(&kvsPeers, "kvs-peer", "Embedded KVS peer in the form nodeID,apiAddress,raftAddress (repeatable)")

	// Doctor telemetry log engine
	logengineDir := flag.String("logengine-dir", "", "Directory for the doctor telemetry log engine (empty disables)")

	// Metrics
	metricsAddr := flag.String("metrics-addr", ":9100", "Listen address for Prometheus /metrics endpoint (empty disables)")

	flag.Parse()

	if *nodeID == "" {
		fmt.Fprintln(os.Stderr, "Error: --node-id is required")
		flag.Usage()
		os.Exit(1)
	}
	telemetryCfg, err := telemetry.LoadConfig("monofs-server")
	if err != nil {
		fmt.Fprintf(os.Stderr, "load telemetry config: %v\n", err)
		os.Exit(1)
	}
	telemetryHandle, err := telemetry.Setup(context.Background(), telemetryCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup telemetry: %v\n", err)
		os.Exit(1)
	}
	if telemetryHandle.Enabled() {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := telemetryHandle.Shutdown(shutdownCtx); err != nil {
				fmt.Fprintf(os.Stderr, "shutdown telemetry: %v\n", err)
			}
		}()
	}

	// Setup logger
	level := slog.LevelInfo
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	if *debug {
		level = slog.LevelDebug
	}
	var handler slog.Handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	if telemetryHandle.Enabled() {
		handler = telemetry.WrapSlogHandler(handler, "monofs/server")
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	if telemetryHandle.Enabled() {
		telemetry.EmitInfo(context.Background(), "monofs/server", "monofs server telemetry enabled")
	}

	logger.Info("starting monofs-server",
		"version", Version,
		"commit", Commit,
		"build_time", BuildTime,
		"addr", *addr,
		"node_id", *nodeID)

	// Create listener
	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Error("failed to listen", "error", err)
		os.Exit(1)
	}

	// Create gRPC server with raised message limits for inline dep blobs
	grpcServer := grpc.NewServer(
		grpc.StatsHandler(telemetry.NewGRPCServerStatsHandler()),
		grpc.MaxRecvMsgSize(256*1024*1024), // 256 MB
		grpc.MaxSendMsgSize(256*1024*1024),
	)

	// Create server with NutsDB backend
	srv, err := server.NewServer(*nodeID, *addr, *dbPath, *gitCache, logger)
	if err != nil {
		logger.Error("failed to create server", "error", err)
		os.Exit(1)
	}

	if *kvsBootstrap && *kvsRaftAddr == "" {
		logger.Error("invalid kvs configuration", "error", "--kvs-bootstrap requires --kvs-raft-addr")
		os.Exit(1)
	}
	if strings.TrimSpace(*kvsRaftAdvertiseAddr) != "" && *kvsRaftAddr == "" {
		logger.Error("invalid kvs configuration", "error", "--kvs-raft-advertise-addr requires --kvs-raft-addr")
		os.Exit(1)
	}

	// Create fetcher client early so KVS can use it as a blob offloader.
	var kvsOffloader kvsapi.FetcherOffloader
	if len(fetcherAddrs) > 0 {
		kvsClientCfg := fetcher.DefaultClientConfig()
		kvsClientCfg.FetcherAddresses = []string(fetcherAddrs)
		if fc, fcErr := fetcher.NewClient(kvsClientCfg, logger); fcErr != nil {
			logger.Warn("failed to create fetcher client for KVS offloading, archived blobs will stay local",
				"error", fcErr)
		} else {
			kvsOffloader = &kvsOffloaderAdapter{client: fc}
			logger.Info("KVS fetcher offloader configured",
				"fetcher_count", len(fetcherAddrs))
		}
	}

	if strings.TrimSpace(*kvsDataDir) != "" {
		peerDefs, err := parseKVSPeers(kvsPeers)
		if err != nil {
			logger.Error("failed to parse kvs peers", "error", err)
			os.Exit(1)
		}
		apiAddr := strings.TrimSpace(*kvsAPIAddr)
		if apiAddr == "" {
			apiAddr = *addr
		}
		kvsStore, err := raftstore.Open(raftstore.Config{
			NodeID:         *nodeID,
			DataDir:        *kvsDataDir,
			RaftAddress:    strings.TrimSpace(*kvsRaftAddr),
			RaftAdvertise:  strings.TrimSpace(*kvsRaftAdvertiseAddr),
			APIAddress:     apiAddr,
			Peers:          peerDefs,
			Bootstrap:      *kvsBootstrap,
			MaxHotVersions: *kvsMaxHotVersions,
			LogOutput:      os.Stdout,
			Offloader:      kvsOffloader,
		})
		if err != nil {
			logger.Error("failed to create embedded kvs store", "error", err)
			os.Exit(1)
		}
		srv.SetKVSStore(kvsStore)
		kvsgrpc.Register(grpcServer, kvsStore, kvsgrpc.Config{})
		kvsStore.StartPurge(context.Background(), 5*time.Minute)
		logger.Info("embedded kvs enabled",
			"data_dir", *kvsDataDir,
			"api_addr", apiAddr,
			"raft_addr", strings.TrimSpace(*kvsRaftAddr),
			"raft_advertise_addr", strings.TrimSpace(*kvsRaftAdvertiseAddr),
			"peer_count", len(peerDefs),
			"bootstrap", *kvsBootstrap,
			"offloader", kvsOffloader != nil)
	}

	// Configure fetcher client and prediction if enabled
	if len(fetcherAddrs) > 0 && *enablePrediction {
		if err := srv.ConfigureFetcher([]string(fetcherAddrs)); err != nil {
			logger.Warn("failed to configure fetcher client, continuing without prediction",
				"error", err)
		} else {
			logger.Info("prediction and prefetching enabled",
				"fetcher_count", len(fetcherAddrs),
				"fetcher_addrs", fetcherAddrs)
		}
	} else if len(fetcherAddrs) > 0 {
		logger.Info("fetcher addresses provided but prediction not enabled, use --enable-prediction to enable")
	}

	// Enable server-side request forwarding if router is configured
	if *routerAddr != "" {
		if err := srv.EnableForwarding(*routerAddr, 30*time.Second); err != nil {
			logger.Warn("failed to enable forwarding, continuing without it",
				"error", err,
				"router", *routerAddr)
		} else {
			logger.Info("server-side request forwarding enabled",
				"router", *routerAddr)
		}
	}

	// Initialize doctor telemetry log engine if configured.
	if dir := strings.TrimSpace(*logengineDir); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			logger.Error("failed to create logengine directory", "error", err)
			os.Exit(1)
		}
		le := logengine.New(logengine.NewMockS3Store(dir), logengine.Config{
			LocalCacheDir: dir,
			ChunkDuration: 5 * time.Minute,
		})
		srv.SetDoctorBackend(le)
		logger.Info("doctor logengine enabled", "dir", dir)
	}

	srv.Register(grpcServer)

	// Enable reflection for debugging with grpcurl
	reflection.Register(grpcServer)

	// Start diagnostics HTTP server (metrics + pprof), separate from gRPC.
	diagServer := diagnostics.StartServer(logger, "monofs-server", strings.TrimSpace(*metricsAddr))
	defer diagnostics.ShutdownServer(logger, "monofs-server", diagServer)

	// Start serving in background
	go func() {
		logger.Info("server listening", "addr", *addr)
		if err := grpcServer.Serve(lis); err != nil {
			logger.Error("server error", "error", err)
		}
	}()

	// Handle shutdown with graceful failover
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	sig := <-sigCh
	logger.Info("received signal, initiating graceful shutdown", "signal", sig)

	// Attempt graceful failover if router address is configured
	if *routerAddr != "" {
		if err := requestFailover(*routerAddr, *nodeID, logger); err != nil {
			logger.Warn("failover request failed", "error", err)
		} else {
			logger.Info("failover completed successfully")
		}
	}

	// Close server resources
	srv.Close()

	// Graceful stop with timeout
	stopCh := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopCh)
	}()

	select {
	case <-stopCh:
		logger.Info("server stopped gracefully")
	case <-time.After(30 * time.Second):
		logger.Warn("graceful shutdown timeout, forcing stop")
		grpcServer.Stop()
	}

	logger.Info("server stopped")
}

func newMetricsHandler() http.Handler {
	return diagnostics.NewHandler()
}

func requestFailover(routerAddr, nodeID string, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	logger.Info("connecting to router for failover", "router", routerAddr)

	conn, err := grpc.DialContext(ctx, routerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock())
	if err != nil {
		return fmt.Errorf("failed to connect to router: %w", err)
	}
	defer conn.Close()

	client := pb.NewMonoFSRouterClient(conn)

	resp, err := client.RequestFailover(ctx, &pb.FailoverRequest{
		SourceNodeId: nodeID,
		Timestamp:    time.Now().Unix(),
	})
	if err != nil {
		return fmt.Errorf("failover RPC failed: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("failover rejected: %s", resp.Message)
	}

	logger.Info("failover accepted",
		"target_node", resp.TargetNodeId,
		"message", resp.Message)

	return nil
}

func parseKVSPeers(values []string) ([]raftstore.Peer, error) {
	peers := make([]raftstore.Peer, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		parts := strings.Split(trimmed, ",")
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid kvs peer %q: expected nodeID,apiAddress,raftAddress", value)
		}
		peer := raftstore.Peer{
			NodeID:      strings.TrimSpace(parts[0]),
			APIAddress:  strings.TrimSpace(parts[1]),
			RaftAddress: strings.TrimSpace(parts[2]),
		}
		if peer.NodeID == "" || peer.APIAddress == "" || peer.RaftAddress == "" {
			return nil, fmt.Errorf("invalid kvs peer %q: nodeID, apiAddress, and raftAddress are required", value)
		}
		peers = append(peers, peer)
	}
	return peers, nil
}

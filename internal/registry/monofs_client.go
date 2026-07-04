package registry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	defaultClientHeartbeat = 30 * time.Second
	defaultTopologyRefresh = 10 * time.Second
	defaultRPCTimeout      = 30 * time.Second
)

type ClientConfig struct {
	RouterAddr string
	Token      string
	ClientID   string
	DataNS     string
	Logger     *slog.Logger
}

type Client struct {
	routerAddr string
	clientID   string
	token      string
	dataNS     string
	logger     *slog.Logger
	rpcTimeout time.Duration

	mu          sync.Mutex
	routerConn  *grpc.ClientConn
	router      pb.MonoFSRouterClient
	nodeConns   map[string]*grpc.ClientConn
	nodeClients map[string]pb.MonoFSClient
	nodeAddrs   map[string]string
	lastRefresh time.Time
	refreshTTL  time.Duration

	stopHeartbeat chan struct{}
	stopOnce      sync.Once
	heartbeatWG   sync.WaitGroup
}

func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	if cfg.RouterAddr == "" {
		return nil, fmt.Errorf("router address is required")
	}
	if cfg.ClientID == "" {
		hostname, _ := os.Hostname()
		cfg.ClientID = "registry-" + hostname + "-" + randomHex(6)
	}
	if cfg.DataNS == "" {
		cfg.DataNS = "docker-registry"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	conn, err := grpc.NewClient(
		cfg.RouterAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(1024*1024*1024),
			grpc.MaxCallSendMsgSize(1024*1024*1024),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to monofs router: %w", err)
	}

	c := &Client{
		routerAddr:    cfg.RouterAddr,
		clientID:      cfg.ClientID,
		token:         cfg.Token,
		dataNS:        cfg.DataNS,
		logger:        logger,
		rpcTimeout:    defaultRPCTimeout,
		routerConn:    conn,
		router:        pb.NewMonoFSRouterClient(conn),
		nodeConns:     map[string]*grpc.ClientConn{},
		nodeClients:   map[string]pb.MonoFSClient{},
		nodeAddrs:     map[string]string{},
		refreshTTL:    defaultTopologyRefresh,
		stopHeartbeat: make(chan struct{}),
	}

	if err := c.refreshNodes(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	heartbeatInterval, err := c.register(ctx)
	if err != nil {
		logger.Warn("failed to register with monofs router, continuing without registration", "error", err)
	} else {
		c.startHeartbeatLoop(heartbeatInterval)
	}
	return c, nil
}

func (c *Client) Close() error {
	c.stopHeartbeatLoop()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.router != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = c.router.UnregisterClient(ctx, &pb.UnregisterClientRequest{ClientId: c.clientID, Reason: "registry shutdown"})
		cancel()
	}
	for _, conn := range c.nodeConns {
		_ = conn.Close()
	}
	c.nodeConns = nil
	c.nodeClients = nil
	c.nodeAddrs = nil
	if c.routerConn != nil {
		return c.routerConn.Close()
	}
	return nil
}

func (c *Client) dataPath(elem ...string) string {
	return c.dataNS + "/" + strings.Join(elem, "/")
}

// Read reads the file at the given path within the data namespace.
func (c *Client) Read(ctx context.Context, path string) ([]byte, error) {
	nodes, err := c.healthyNodes(ctx)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, node := range nodes {
		callCtx, cancel := context.WithTimeout(ctx, c.rpcTimeout)
		stream, err := node.client.Read(callCtx, &pb.ReadRequest{Path: c.dataPath(path), Offset: 0, Size: 0})
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		var content []byte
		for {
			chunk, recvErr := stream.Recv()
			if recvErr == io.EOF {
				cancel()
				return content, nil
			}
			if recvErr != nil {
				cancel()
				lastErr = recvErr
				break
			}
			content = append(content, chunk.GetData()...)
		}
	}
	if lastErr != nil {
		if status.Code(lastErr) == codes.NotFound {
			return nil, os.ErrNotExist
		}
		return nil, lastErr
	}
	return nil, os.ErrNotExist
}

// ReadStream returns a streaming reader for the file at path.
// The caller must close the returned reader to release the underlying gRPC stream.
func (c *Client) ReadStream(ctx context.Context, path string) (io.ReadCloser, error) {
	nodes, err := c.healthyNodes(ctx)
	if err != nil {
		return nil, err
	}
	fullPath := c.dataPath(path)
	var lastErr error
	for _, node := range nodes {
		callCtx, cancel := context.WithTimeout(ctx, c.rpcTimeout)
		stream, err := node.client.Read(callCtx, &pb.ReadRequest{Path: fullPath, Offset: 0, Size: 0})
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		// Verify the stream has data by receiving the first chunk.
		// The node's Read handler may successfully open a stream but
		// return NotFound on the first Recv if metadata is missing.
		chunk, recvErr := stream.Recv()
		if recvErr != nil {
			cancel()
			lastErr = recvErr
			continue
		}
		return &grpcReadCloser{stream: stream, cancel: cancel, buf: chunk.GetData(), offset: 0}, nil
	}
	if lastErr != nil {
		if status.Code(lastErr) == codes.NotFound {
			return nil, os.ErrNotExist
		}
		return nil, lastErr
	}
	return nil, os.ErrNotExist
}

type grpcReadCloser struct {
	stream pb.MonoFS_ReadClient
	cancel context.CancelFunc
	buf    []byte
	offset int
	eof    bool
}

func (r *grpcReadCloser) Read(p []byte) (int, error) {
	if r.offset < len(r.buf) {
		n := copy(p, r.buf[r.offset:])
		r.offset += n
		return n, nil
	}
	if r.eof {
		return 0, io.EOF
	}
	chunk, err := r.stream.Recv()
	if err == io.EOF {
		r.eof = true
		r.cancel()
		return 0, io.EOF
	}
	if err != nil {
		r.cancel()
		return 0, err
	}
	r.buf = chunk.GetData()
	r.offset = 0
	if len(r.buf) == 0 {
		r.eof = true
		r.cancel()
		return 0, io.EOF
	}
	n := copy(p, r.buf)
	r.offset = n
	return n, nil
}

func (r *grpcReadCloser) Close() error {
	r.cancel()
	return nil
}

// Write creates or overwrites a file at the given path.
func (c *Client) Write(ctx context.Context, path string, data []byte) error {
	nodes, err := c.healthyNodes(ctx)
	if err != nil {
		return err
	}

	lastErr := fmt.Errorf("no nodes available")

	// Use IngestFileBatch to store file data, avoiding the unimplemented
	// Create/Write RPCs on the data node.
	blobHash := sha256Hex(data)
	meta := &pb.FileMetadata{
		Path:          path,
		StorageId:     storageIDFromNS(c.dataNS),
		DisplayPath:   c.dataNS,
		Size:          uint64(len(data)),
		Mtime:         time.Now().Unix(),
		Mode:          0644,
		BlobHash:      blobHash,
		Source:        "monofs-registry",
		InlineContent: data,
		SourceType:    pb.IngestionType_INGESTION_FILE,
		FetchType:     pb.SourceType_SOURCE_TYPE_BLOB,
	}

	for _, node := range nodes {
		callCtx, cancel := context.WithTimeout(ctx, c.rpcTimeout)
		resp, err := node.client.IngestFileBatch(callCtx, &pb.IngestFileBatchRequest{
			Files:       []*pb.FileMetadata{meta},
			StorageId:   meta.StorageId,
			DisplayPath: meta.DisplayPath,
		})
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if resp != nil && resp.GetSuccess() {
			return nil
		}
		if resp != nil && resp.GetErrorMessage() != "" {
			lastErr = fmt.Errorf("ingest %s: %s", path, resp.GetErrorMessage())
			continue
		}
		lastErr = fmt.Errorf("ingest %s: unknown error", path)
	}

	return fmt.Errorf("write %s: %w", path, lastErr)
}

// Exists checks if a path exists.
func (c *Client) Exists(ctx context.Context, path string) (bool, error) {
	nodes, err := c.healthyNodes(ctx)
	if err != nil {
		return false, err
	}
	fullPath := c.dataPath(path)
	for _, node := range nodes {
		callCtx, cancel := context.WithTimeout(ctx, c.rpcTimeout)
		_, err := node.client.GetAttr(callCtx, &pb.GetAttrRequest{Path: fullPath})
		cancel()
		if err == nil {
			return true, nil
		}
		if status.Code(err) == codes.NotFound {
			return false, nil
		}
	}
	return false, nil
}

// Stat returns file attributes.
func (c *Client) Stat(ctx context.Context, path string) (*pb.GetAttrResponse, error) {
	nodes, err := c.healthyNodes(ctx)
	if err != nil {
		return nil, err
	}
	fullPath := c.dataPath(path)
	for _, node := range nodes {
		callCtx, cancel := context.WithTimeout(ctx, c.rpcTimeout)
		resp, err := node.client.GetAttr(callCtx, &pb.GetAttrRequest{Path: fullPath})
		cancel()
		if err == nil && resp.GetFound() {
			return resp, nil
		}
	}
	return nil, os.ErrNotExist
}

// Delete removes a file.
func (c *Client) Delete(ctx context.Context, path string) error {
	nodes, err := c.healthyNodes(ctx)
	if err != nil {
		return err
	}
	fullPath := c.dataPath(path)
	for _, node := range nodes {
		callCtx, cancel := context.WithTimeout(ctx, c.rpcTimeout)
		_, err := node.client.DeleteFile(callCtx, &pb.DeleteFileRequest{StorageId: "", FilePath: fullPath})
		cancel()
		if err != nil {
			if status.Code(err) != codes.NotFound {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("delete %s: no nodes", path)
}

// ListDir lists directory entries.
func (c *Client) ListDir(ctx context.Context, path string) ([]string, error) {
	nodes, err := c.healthyNodes(ctx)
	if err != nil {
		return nil, err
	}
	fullPath := c.dataPath(path)
	for _, node := range nodes {
		callCtx, cancel := context.WithTimeout(ctx, c.rpcTimeout)
		stream, err := node.client.ReadDir(callCtx, &pb.ReadDirRequest{Path: fullPath})
		if err != nil {
			cancel()
			if status.Code(err) == codes.NotFound {
				return nil, nil
			}
			continue
		}
		var entries []string
		for {
			entry, recvErr := stream.Recv()
			if recvErr == io.EOF {
				cancel()
				return entries, nil
			}
			if recvErr != nil {
				cancel()
				return entries, nil
			}
			if name := entry.GetName(); name != "" {
				entries = append(entries, name)
			}
		}
	}
	return nil, fmt.Errorf("list %s: no nodes", path)
}

// CreateDir ensures a directory path exists.
func (c *Client) CreateDir(ctx context.Context, path string) error {
	// Directory creation is handled implicitly by IngestFileBatch during writes.
	// This remains for API compatibility but is a no-op.
	return nil
}

func (c *Client) register(ctx context.Context) (time.Duration, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.rpcTimeout)
	defer cancel()
	resp, err := c.router.RegisterClient(callCtx, &pb.RegisterClientRequest{
		ClientId: c.clientID,
		Hostname: "monofs-registry",
		Version:  "registry",
		GuardianConfig: &pb.GuardianConfig{
			AuthToken:   c.token,
			Role:        "registry",
			DisplayName: "monofs-registry",
		},
	})
	if err != nil {
		return 0, err
	}
	interval := time.Duration(resp.GetHeartbeatIntervalMs()) * time.Millisecond
	if interval <= 0 {
		interval = defaultClientHeartbeat
	}
	return interval, nil
}

func (c *Client) refreshNodes(ctx context.Context) error {
	callCtx, cancel := context.WithTimeout(ctx, c.rpcTimeout)
	defer cancel()
	resp, err := c.router.GetClusterInfo(callCtx, &pb.ClusterInfoRequest{ClientId: c.clientID})
	if err != nil {
		return fmt.Errorf("fetch cluster info: %w", err)
	}

	var healthy []*pb.NodeInfo
	for _, node := range resp.GetNodes() {
		if node != nil && node.GetHealthy() && node.GetNodeId() != "" && node.GetAddress() != "" {
			healthy = append(healthy, node)
		}
	}
	if len(healthy) == 0 {
		return fmt.Errorf("no healthy monofs nodes")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	seen := map[string]struct{}{}
	for _, node := range healthy {
		nodeID := node.GetNodeId()
		nodeAddr := node.GetAddress()
		seen[nodeID] = struct{}{}
		if conn := c.nodeConns[nodeID]; conn != nil && c.nodeAddrs[nodeID] == nodeAddr {
			continue
		}
		if conn := c.nodeConns[nodeID]; conn != nil {
			_ = conn.Close()
			delete(c.nodeConns, nodeID)
			delete(c.nodeClients, nodeID)
			delete(c.nodeAddrs, nodeID)
		}
		nodeConn, dialErr := grpc.NewClient(
			nodeAddr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(1024*1024*1024),
				grpc.MaxCallSendMsgSize(1024*1024*1024),
			),
		)
		if dialErr != nil {
			continue
		}
		c.nodeConns[nodeID] = nodeConn
		c.nodeClients[nodeID] = pb.NewMonoFSClient(nodeConn)
		c.nodeAddrs[nodeID] = nodeAddr
	}

	for nodeID, conn := range c.nodeConns {
		if _, ok := seen[nodeID]; !ok {
			_ = conn.Close()
			delete(c.nodeConns, nodeID)
			delete(c.nodeClients, nodeID)
			delete(c.nodeAddrs, nodeID)
		}
	}
	c.lastRefresh = time.Now()
	return nil
}

func (c *Client) startHeartbeatLoop(interval time.Duration) {
	if interval <= 0 {
		interval = defaultClientHeartbeat
	}
	c.heartbeatWG.Add(1)
	go func() {
		defer c.heartbeatWG.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-c.stopHeartbeat:
				return
			case <-ticker.C:
				callCtx, cancel := context.WithTimeout(context.Background(), c.rpcTimeout)
				resp, err := c.router.ClientHeartbeat(callCtx, &pb.ClientHeartbeatRequest{ClientId: c.clientID})
				cancel()
				if err == nil && resp.GetShouldRegister() {
					_, _ = c.register(context.Background())
				}
			}
		}
	}()
}

func (c *Client) stopHeartbeatLoop() {
	c.stopOnce.Do(func() {
		if c.stopHeartbeat != nil {
			close(c.stopHeartbeat)
		}
		c.heartbeatWG.Wait()
	})
}

func (c *Client) healthyNodes(ctx context.Context) ([]nodeTarget, error) {
	c.mu.Lock()
	fresh := !c.lastRefresh.IsZero() && time.Since(c.lastRefresh) <= c.refreshTTL && len(c.nodeClients) > 0
	c.mu.Unlock()
	if !fresh {
		if err := c.refreshNodes(ctx); err != nil {
			c.mu.Lock()
			nodes := c.snapshotNodeTargets()
			c.mu.Unlock()
			if len(nodes) == 0 {
				return nil, err
			}
			return nodes, nil
		}
	}
	c.mu.Lock()
	nodes := c.snapshotNodeTargets()
	c.mu.Unlock()
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no healthy monofs nodes")
	}
	return nodes, nil
}

func (c *Client) snapshotNodeTargets() []nodeTarget {
	ids := make([]string, 0, len(c.nodeClients))
	for id := range c.nodeClients {
		ids = append(ids, id)
	}
	out := make([]nodeTarget, 0, len(ids))
	for _, id := range ids {
		out = append(out, nodeTarget{id: id, client: c.nodeClients[id]})
	}
	return out
}

type nodeTarget struct {
	id     string
	client pb.MonoFSClient
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

func storageIDFromNS(ns string) string {
	return "registry-" + ns
}

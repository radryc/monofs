// Package client provides sharded gRPC client for distributed MonoFS operations.
package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/monopath"
	"github.com/radryc/monofs/internal/sharding"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// ShardedClient provides a multi-node client with HRW-based routing.
// It maintains connections to all backend nodes and routes requests
// based on Rendezvous hashing.
type ShardedClient struct {
	mu                   sync.RWMutex
	hrw                  *sharding.HRW
	conns                map[string]*grpc.ClientConn // nodeID -> connection
	clients              map[string]pb.MonoFSClient  // nodeID -> client
	routerAddr           string
	routerConn           *grpc.ClientConn
	routerClient         pb.MonoFSRouterClient
	clientID             string
	logger               *slog.Logger
	useExternalAddresses bool

	// Cluster topology cache
	clusterVersion int64
	refreshTicker  *time.Ticker
	stopRefresh    chan struct{}

	// Connection state
	connected bool
	lastError error

	// Timeout configuration
	rpcTimeout time.Duration

	// Client registration and metrics
	hostname          string
	mountPoint        string
	writable          bool
	version           string
	registered        bool
	heartbeatInterval time.Duration
	stopHeartbeat     chan struct{}
	operationsCount   int64 // atomic counter
	bytesRead         int64 // atomic counter
	errorsCount       int64 // atomic counter

	// Guardian visibility
	guardianVisible bool
}

// ShardedClientConfig holds configuration for ShardedClient.
type ShardedClientConfig struct {
	RouterAddr           string        // Router service address (host:port)
	ClientID             string        // Unique client identifier
	RefreshInterval      time.Duration // How often to refresh cluster topology
	RPCTimeout           time.Duration // Timeout for individual RPC calls (default: 3s)
	UseExternalAddresses bool          // Use external node addresses (for host-based clients, false for containerized)
	Logger               *slog.Logger  // Optional logger for debugging

	// Client registration info
	Hostname   string // Client hostname for identification
	MountPoint string // FUSE mount path
	Writable   bool   // Whether write mode is enabled
	Version    string // Client version
}

// NewShardedClient creates a new sharded client connected to the router.
func NewShardedClient(ctx context.Context, cfg ShardedClientConfig) (*ShardedClient, error) {
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = 30 * time.Second
	}
	if cfg.ClientID == "" {
		cfg.ClientID = fmt.Sprintf("client-%d", time.Now().UnixNano())
	}
	if cfg.RPCTimeout == 0 {
		cfg.RPCTimeout = 10 * time.Second
	}
	if cfg.Hostname == "" {
		cfg.Hostname, _ = os.Hostname()
	}
	if cfg.Version == "" {
		cfg.Version = "unknown"
	}

	sc := &ShardedClient{
		conns:                make(map[string]*grpc.ClientConn),
		clients:              make(map[string]pb.MonoFSClient),
		routerAddr:           cfg.RouterAddr,
		clientID:             cfg.ClientID,
		stopRefresh:          make(chan struct{}),
		stopHeartbeat:        make(chan struct{}),
		connected:            false,
		rpcTimeout:           cfg.RPCTimeout,
		useExternalAddresses: cfg.UseExternalAddresses,
		logger:               cfg.Logger,
		hostname:             cfg.Hostname,
		mountPoint:           cfg.MountPoint,
		writable:             cfg.Writable,
		version:              cfg.Version,
		heartbeatInterval:    30 * time.Second, // Default, may be overridden by router
	}

	// Connect to router
	routerConn, err := grpc.NewClient(cfg.RouterAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(256*1024*1024),
			grpc.MaxCallSendMsgSize(256*1024*1024),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to router: %w", err)
	}
	sc.routerConn = routerConn
	sc.routerClient = pb.NewMonoFSRouterClient(routerConn)

	// Fetch initial cluster topology
	if err := sc.refreshClusterInfo(ctx); err != nil {
		routerConn.Close()
		return nil, fmt.Errorf("fetch cluster info: %w", err)
	}

	sc.connected = true

	// Register with router
	if err := sc.registerWithRouter(ctx); err != nil {
		if sc.logger != nil {
			sc.logger.Warn("failed to register with router", "error", err)
		}
		// Don't fail - registration is optional for functionality
	}

	// Start background refresh
	sc.refreshTicker = time.NewTicker(cfg.RefreshInterval)
	go sc.refreshLoop()

	// Start heartbeat loop if registered
	if sc.registered {
		go sc.heartbeatLoop()
	}

	return sc, nil
}

// NewDisconnectedClient creates a client that starts in disconnected state
// and attempts to connect in the background.
func NewDisconnectedClient(cfg ShardedClientConfig) *ShardedClient {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = 30 * time.Second
	}
	if cfg.ClientID == "" {
		cfg.ClientID = fmt.Sprintf("client-%d", time.Now().UnixNano())
	}
	if cfg.Hostname == "" {
		cfg.Hostname, _ = os.Hostname()
	}
	if cfg.Version == "" {
		cfg.Version = "unknown"
	}

	sc := &ShardedClient{
		conns:                make(map[string]*grpc.ClientConn),
		clients:              make(map[string]pb.MonoFSClient),
		routerAddr:           cfg.RouterAddr,
		clientID:             cfg.ClientID,
		stopRefresh:          make(chan struct{}),
		stopHeartbeat:        make(chan struct{}),
		connected:            false,
		logger:               cfg.Logger,
		rpcTimeout:           3 * time.Second, // Default timeout
		lastError:            fmt.Errorf("not connected"),
		useExternalAddresses: cfg.UseExternalAddresses,
		hostname:             cfg.Hostname,
		mountPoint:           cfg.MountPoint,
		writable:             cfg.Writable,
		version:              cfg.Version,
		heartbeatInterval:    30 * time.Second,
	}

	// Start background connection retry
	sc.refreshTicker = time.NewTicker(cfg.RefreshInterval)
	go sc.reconnectLoop()

	return sc
}

// reconnectLoop attempts to establish connection in background
func (sc *ShardedClient) reconnectLoop() {
	// Try immediately
	sc.attemptConnection()

	// Then periodically
	for {
		select {
		case <-sc.refreshTicker.C:
			if !sc.isConnected() {
				sc.attemptConnection()
			} else {
				// Once connected, try to refresh topology
				ctx, cancel := context.WithTimeout(context.Background(), sc.rpcTimeout)
				_ = sc.refreshClusterInfo(ctx)
				cancel()
			}
		case <-sc.stopRefresh:
			return
		}
	}
}

// attemptConnection tries to establish router connection
func (sc *ShardedClient) attemptConnection() {
	sc.mu.Lock()
	if sc.connected {
		sc.mu.Unlock()
		return
	}
	sc.mu.Unlock()

	if sc.logger != nil {
		sc.logger.Debug("attempting to connect to router", "addr", sc.routerAddr)
	}

	// Try to connect to router
	routerConn, err := grpc.NewClient(sc.routerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(256*1024*1024),
			grpc.MaxCallSendMsgSize(256*1024*1024),
		),
	)
	if err != nil {
		sc.mu.Lock()
		sc.lastError = fmt.Errorf("connect to router: %w", err)
		sc.mu.Unlock()
		if sc.logger != nil {
			sc.logger.Debug("failed to connect to router", "error", err)
		}
		return
	}

	sc.mu.Lock()
	sc.routerConn = routerConn
	sc.routerClient = pb.NewMonoFSRouterClient(routerConn)
	sc.mu.Unlock()

	// Try to fetch cluster info
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := sc.refreshClusterInfo(ctx); err != nil {
		sc.mu.Lock()
		sc.lastError = fmt.Errorf("fetch cluster info: %w", err)
		sc.mu.Unlock()
		routerConn.Close()
		if sc.logger != nil {
			sc.logger.Debug("failed to fetch cluster info", "error", err)
		}
		return
	}

	sc.mu.Lock()
	sc.connected = true
	sc.lastError = nil
	sc.mu.Unlock()

	if sc.logger != nil {
		sc.logger.Info("successfully connected to cluster", "healthy_nodes", len(sc.GetHealthyNodes()))
	}

	// Register with router
	regCtx, regCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := sc.registerWithRouter(regCtx); err != nil {
		if sc.logger != nil {
			sc.logger.Warn("failed to register with router", "error", err)
		}
	} else {
		// Start heartbeat loop if not already running
		sc.mu.Lock()
		if sc.registered && sc.stopHeartbeat != nil {
			select {
			case <-sc.stopHeartbeat:
				// Channel was closed, create new one and start loop
				sc.stopHeartbeat = make(chan struct{})
				go sc.heartbeatLoop()
			default:
				// Already running
			}
		}
		sc.mu.Unlock()
	}
	regCancel()
}

// isConnected returns connection state
func (sc *ShardedClient) isConnected() bool {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.connected
}

// refreshLoop periodically refreshes cluster topology.
func (sc *ShardedClient) refreshLoop() {
	for {
		select {
		case <-sc.refreshTicker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = sc.refreshClusterInfo(ctx)
			cancel()
		case <-sc.stopRefresh:
			return
		}
	}
}

// refreshClusterInfo fetches the current cluster topology from router.
func (sc *ShardedClient) refreshClusterInfo(ctx context.Context) error {
	sc.mu.RLock()
	client := sc.routerClient
	sc.mu.RUnlock()

	if client == nil {
		return fmt.Errorf("no router connection")
	}

	resp, err := client.GetClusterInfo(ctx, &pb.ClusterInfoRequest{
		ClientId:             sc.clientID,
		UseExternalAddresses: sc.useExternalAddresses,
	})
	if err != nil {
		// Router temporarily unavailable - keep using cached topology
		// Do NOT set connected=false here, as individual node connections may still work
		if sc.logger != nil {
			sc.logger.Warn("failed to refresh cluster info from router, using cached topology", "error", err)
		}
		return err
	}

	if sc.logger != nil {
		sc.logger.Debug("received cluster info",
			"node_count", len(resp.Nodes),
			"version", resp.Version,
			"use_external", sc.useExternalAddresses)
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	// Mark connected since we successfully talked to router
	sc.connected = true
	sc.lastError = nil
	sc.guardianVisible = true

	// Node health state comes exclusively from the router via UpdateNodeHealthFromProto().
	// The router is the single source of truth for node health.
	if sc.hrw == nil {
		sc.hrw = sharding.NewHRWFromProto(resp.Nodes)
	} else {
		sc.hrw.UpdateNodeHealthFromProto(resp.Nodes)
	}

	// Check if topology changed (for connection management)
	topologyChanged := resp.Version > sc.clusterVersion
	if topologyChanged {
		sc.clusterVersion = resp.Version
	}

	// Log node health state after update
	if sc.logger != nil {
		healthyCount := 0
		nodeStates := make([]string, 0, len(resp.Nodes))
		for _, n := range resp.Nodes {
			status := "unhealthy"
			if n.Healthy {
				healthyCount++
				status = "healthy"
			}
			nodeStates = append(nodeStates, fmt.Sprintf("%s=%s", n.NodeId, status))
		}
		// Only log at INFO level when topology changes, DEBUG for health syncs
		if topologyChanged {
			sc.logger.Info("cluster topology updated",
				"version", resp.Version,
				"total_nodes", len(resp.Nodes),
				"healthy_nodes", healthyCount,
				"node_states", strings.Join(nodeStates, ","))
		} else {
			sc.logger.Debug("synced node health from router",
				"version", resp.Version,
				"healthy_nodes", healthyCount,
				"node_states", strings.Join(nodeStates, ","))
		}
	}

	// Only manage connections if topology changed (new/removed nodes)
	if !topologyChanged {
		return nil
	}

	// Connect to new nodes, disconnect from removed ones
	currentNodes := make(map[string]bool)
	for _, node := range resp.Nodes {
		currentNodes[node.NodeId] = true

		if _, exists := sc.conns[node.NodeId]; !exists {
			// New node - establish connection
			if sc.logger != nil {
				sc.logger.Debug("connecting to node",
					"node_id", node.NodeId,
					"address", node.Address,
					"healthy", node.Healthy)
			}
			conn, err := grpc.NewClient(node.Address,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithDefaultCallOptions(
					grpc.MaxCallRecvMsgSize(256*1024*1024),
					grpc.MaxCallSendMsgSize(256*1024*1024),
				),
			)
			if err != nil {
				// Log error but continue with other nodes
				if sc.logger != nil {
					sc.logger.Warn("failed to connect to node",
						"node_id", node.NodeId,
						"address", node.Address,
						"error", err)
				}
				continue
			}
			sc.conns[node.NodeId] = conn
			sc.clients[node.NodeId] = pb.NewMonoFSClient(conn)
			if sc.logger != nil {
				sc.logger.Info("connected to node",
					"node_id", node.NodeId,
					"address", node.Address)
			}
		}
	}

	// Remove connections to nodes no longer in cluster
	for nodeID, conn := range sc.conns {
		if !currentNodes[nodeID] {
			conn.Close()
			delete(sc.conns, nodeID)
			delete(sc.clients, nodeID)
		}
	}

	return nil
}

func splitDisplayPath(fullPath string) (displayPath, filePath string, ok bool) {
	return monopath.SplitDisplayPath(fullPath)
}

// buildShardKey builds the sharding key in the format "storageID:filePath"
// to match the router's sharding algorithm used during ingestion.
func buildShardKey(fullPath string) string {
	return monopath.BuildShardKey(fullPath)
}

// getNodeForFileFromRouter queries the router for the correct node to serve a file.
// This is used during failover scenarios when the HRW primary is unavailable.
// Returns the primary node ID and a list of fallback node IDs.
func (sc *ShardedClient) getNodeForFileFromRouter(ctx context.Context, fullPath string) (string, []string, error) {
	displayPath, filePath, ok := splitDisplayPath(fullPath)
	if !ok || filePath == "" {
		return "", nil, fmt.Errorf("not a file path: %s", fullPath)
	}

	storageID := sharding.GenerateStorageID(displayPath) // Use shared function to match router

	sc.mu.RLock()
	routerClient := sc.routerClient
	sc.mu.RUnlock()

	if routerClient == nil {
		return "", nil, fmt.Errorf("no router connection")
	}

	callCtx, cancel := context.WithTimeout(ctx, sc.rpcTimeout)
	defer cancel()

	resp, err := routerClient.GetNodeForFile(callCtx, &pb.GetNodeForFileRequest{
		StorageId: storageID,
		FilePath:  filePath,
	})
	if err != nil {
		return "", nil, fmt.Errorf("router GetNodeForFile failed: %w", err)
	}

	if sc.logger != nil {
		sc.logger.Debug("router GetNodeForFile response",
			"path", fullPath,
			"primary_node", resp.NodeId,
			"fallbacks", resp.FallbackNodeIds,
			"rebalance_state", resp.RebalanceState,
			"cache_ttl", resp.CacheTtlSeconds)
	}

	return resp.NodeId, resp.FallbackNodeIds, nil
}

// withClientID attaches the client ID as gRPC metadata to the context.
// This allows the server to identify the client for access pattern analysis.
func (sc *ShardedClient) withClientID(ctx context.Context) context.Context {
	md := metadata.Pairs("x-client-id", sc.clientID)
	return metadata.NewOutgoingContext(ctx, md)
}

// Lookup performs a lookup operation routed via HRW.
// The path is split into repo ID and file path, and combined as "storageID:filePath"
// to match the exact sharding key used during ingestion on the router.
func (sc *ShardedClient) Lookup(ctx context.Context, path string) (*pb.LookupResponse, error) {
	// Check if context is already canceled before starting
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Build shard key to match router's ingestion sharding
	// For files: "github.com/owner/repo/README.md" -> "github.com/owner/repo:README.md"
	// For repo dirs: "github.com/owner/repo" -> "github.com/owner/repo" (special case)
	key := buildShardKey(path)
	if key == "" {
		key = "/"
	}

	// Get all nodes ranked by HRW for this key
	sc.mu.RLock()
	var rankedNodes []sharding.Node
	if sc.hrw != nil {
		rankedNodes = sc.hrw.GetNodes(key, 3)
	}
	sc.mu.RUnlock()

	if len(rankedNodes) == 0 {
		return nil, fmt.Errorf("no healthy nodes available")
	}

	// Try up to 3 nodes via HRW ranking (primary + 2 fallbacks)
	var lastErr error
	maxAttempts := 3
	if maxAttempts > len(rankedNodes) {
		maxAttempts = len(rankedNodes)
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Check context cancellation between attempts
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		nodeID := rankedNodes[attempt].ID

		sc.mu.RLock()
		client := sc.clients[nodeID]
		sc.mu.RUnlock()

		if client == nil {
			continue
		}

		if sc.logger != nil {
			sc.logger.Debug("lookup routing",
				"full_path", path,
				"shard_key", key,
				"target_node", nodeID,
				"attempt", attempt+1)
		}

		// Add timeout to prevent hanging on dead nodes
		callCtx, cancel := context.WithTimeout(ctx, sc.rpcTimeout)
		resp, err := client.Lookup(callCtx, &pb.LookupRequest{
			ParentPath: path,
			Name:       "",
		})
		cancel()

		if err != nil {
			lastErr = err
			if sc.logger != nil {
				sc.logger.Debug("lookup RPC error, trying next node",
					"path", path,
					"node_id", nodeID,
					"error", err,
					"attempt", attempt+1)
			}
			continue // Try next node in HRW ranking
		}

		// If found, return immediately
		if resp.Found {
			return resp, nil
		}

		// Not found on primary - for directories, need to check other nodes
		// because files may be sharded to different nodes
		break
	}

	// If we got a "not found" (not an RPC error), check other healthy nodes
	// This handles directories that exist on multiple nodes
	sc.mu.RLock()
	var healthyNodes []sharding.Node
	if sc.hrw != nil {
		healthyNodes = sc.hrw.GetHealthyNodes()
	}
	primaryNodeID := ""
	if sc.hrw != nil {
		if node := sc.hrw.GetNode(key); node != nil {
			primaryNodeID = node.ID
		}
	}
	clients := make(map[string]pb.MonoFSClient)
	for _, node := range healthyNodes {
		if node.ID != primaryNodeID {
			if c, ok := sc.clients[node.ID]; ok {
				clients[node.ID] = c
			}
		}
	}
	sc.mu.RUnlock()

	if sc.logger != nil && len(clients) > 0 {
		sc.logger.Debug("primary lookup not found, trying other healthy nodes",
			"path", path,
			"fallback_nodes", len(clients))
	}

	// Try other nodes until we find it or exhaust all options
	for nodeID, client := range clients {
		// Check context cancellation between fallback attempts
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		callCtx, cancel := context.WithTimeout(ctx, sc.rpcTimeout)
		resp, err := client.Lookup(callCtx, &pb.LookupRequest{
			ParentPath: path,
			Name:       "",
		})
		cancel()

		if err != nil {
			continue
		}
		if resp.Found {
			if sc.logger != nil {
				sc.logger.Debug("lookup found on fallback node", "path", path, "node_id", nodeID)
			}
			return resp, nil
		}
	}

	// FAILOVER: If all HRW-based attempts failed, try router-based routing
	// This handles the case where the primary node is down and failover is active
	if lastErr != nil {
		// Check context before router-based routing
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		if sc.logger != nil {
			sc.logger.Debug("HRW-based lookup failed, trying router-based routing",
				"path", path,
				"last_error", lastErr)
		}

		primaryNode, fallbacks, routerErr := sc.getNodeForFileFromRouter(ctx, path)
		if routerErr == nil && primaryNode != "" {
			// Try the router-suggested nodes
			nodesToTry := append([]string{primaryNode}, fallbacks...)

			for _, nodeID := range nodesToTry {
				// Check context cancellation between router-based attempts
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}

				sc.mu.RLock()
				client := sc.clients[nodeID]
				sc.mu.RUnlock()

				if client == nil {
					continue
				}

				callCtx, cancel := context.WithTimeout(ctx, sc.rpcTimeout)
				resp, err := client.Lookup(callCtx, &pb.LookupRequest{
					ParentPath: path,
					Name:       "",
				})
				cancel()

				if err != nil {
					continue
				}
				if resp.Found {
					if sc.logger != nil {
						sc.logger.Info("lookup found via router-based failover",
							"path", path,
							"node_id", nodeID)
					}
					return resp, nil
				}
			}
		}
	}

	// Return not found (or last error if all nodes failed)
	if lastErr != nil {
		return nil, lastErr
	}
	return &pb.LookupResponse{Found: false}, nil
}

// GetAttr performs a getattr operation routed via HRW.
func (sc *ShardedClient) GetAttr(ctx context.Context, path string) (*pb.GetAttrResponse, error) {
	// Check if context is already canceled before starting
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Build shard key to match router's ingestion sharding
	key := buildShardKey(path)
	if key == "" {
		key = "/"
	}

	// Get all nodes ranked by HRW for this key
	sc.mu.RLock()
	var rankedNodes []sharding.Node
	if sc.hrw != nil {
		rankedNodes = sc.hrw.GetNodes(key, 3)
	}
	sc.mu.RUnlock()

	if len(rankedNodes) == 0 {
		return nil, fmt.Errorf("no healthy nodes available")
	}

	// Try up to 3 nodes via HRW ranking (primary + 2 fallbacks)
	var lastErr error
	maxAttempts := 3
	if maxAttempts > len(rankedNodes) {
		maxAttempts = len(rankedNodes)
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Check context cancellation between attempts
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		nodeID := rankedNodes[attempt].ID

		sc.mu.RLock()
		client := sc.clients[nodeID]
		sc.mu.RUnlock()

		if client == nil {
			continue
		}

		if sc.logger != nil {
			sc.logger.Debug("getattr routing",
				"full_path", path,
				"shard_key", key,
				"target_node", nodeID,
				"attempt", attempt+1)
		}

		// Add timeout to prevent hanging on dead nodes
		callCtx, cancel := context.WithTimeout(ctx, sc.rpcTimeout)
		resp, err := client.GetAttr(callCtx, &pb.GetAttrRequest{
			Path: path,
		})
		cancel()

		if err != nil {
			lastErr = err
			if sc.logger != nil {
				sc.logger.Debug("getattr RPC error, trying next node",
					"path", path,
					"node_id", nodeID,
					"error", err,
					"attempt", attempt+1)
			}
			continue // Try next node in HRW ranking
		}

		// If found, return immediately
		if resp.Found {
			return resp, nil
		}

		// Not found on primary - for directories, need to check other nodes
		break
	}

	// If we got a "not found" (not an RPC error), check other healthy nodes
	sc.mu.RLock()
	var healthyNodes []sharding.Node
	if sc.hrw != nil {
		healthyNodes = sc.hrw.GetHealthyNodes()
	}
	primaryNodeID := ""
	if sc.hrw != nil {
		if node := sc.hrw.GetNode(key); node != nil {
			primaryNodeID = node.ID
		}
	}
	clients := make(map[string]pb.MonoFSClient)
	for _, node := range healthyNodes {
		if node.ID != primaryNodeID {
			if c, ok := sc.clients[node.ID]; ok {
				clients[node.ID] = c
			}
		}
	}
	sc.mu.RUnlock()

	if sc.logger != nil && len(clients) > 0 {
		sc.logger.Debug("primary getattr not found, trying other healthy nodes",
			"path", path,
			"fallback_nodes", len(clients))
	}

	// Try other nodes until we find it or exhaust all options
	for nodeID, client := range clients {
		callCtx, cancel := context.WithTimeout(ctx, sc.rpcTimeout)
		resp, err := client.GetAttr(callCtx, &pb.GetAttrRequest{
			Path: path,
		})
		cancel()

		if err != nil {
			continue
		}
		if resp.Found {
			if sc.logger != nil {
				sc.logger.Debug("getattr found on fallback node", "path", path, "node_id", nodeID)
			}
			return resp, nil
		}
	}

	// Return not found (or last error if all nodes failed)
	if lastErr != nil {
		return nil, lastErr
	}
	return &pb.GetAttrResponse{Found: false}, nil
}

// ReadDir performs a readdir operation across all healthy nodes in parallel.
// Files are sharded across nodes, so every healthy node must be queried and
// results merged to produce a complete directory listing.
//
// CORRECTNESS: This function never returns partial results. If any node
// fails to respond fully, it retries that node once. If any node still
// fails after retry, it returns an error so that callers (e.g. FUSE
// readdir → filepath.Walk in go mod verify) see an error rather than
// computing a hash over an incomplete file list.
func (sc *ShardedClient) ReadDir(ctx context.Context, path string) ([]*pb.DirEntry, error) {
	// Check if context is already canceled before starting
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// For ReadDir, we need to query ALL HEALTHY nodes since files are sharded
	// Each node may have different files in the same directory

	sc.mu.RLock()
	// Get only healthy nodes from HRW
	var healthyNodes []sharding.Node
	if sc.hrw != nil {
		healthyNodes = sc.hrw.GetHealthyNodes()
	}

	clients := make([]pb.MonoFSClient, 0, len(healthyNodes))
	nodeIDs := make([]string, 0, len(healthyNodes))

	for _, node := range healthyNodes {
		if client, ok := sc.clients[node.ID]; ok {
			clients = append(clients, client)
			nodeIDs = append(nodeIDs, node.ID)
		}
	}
	sc.mu.RUnlock()

	if len(clients) == 0 {
		if sc.logger != nil {
			sc.logger.Debug("readdir: no healthy clients available", "path", path)
		}
		return nil, fmt.Errorf("no healthy nodes available")
	}

	if sc.logger != nil {
		sc.logger.Debug("readdir: querying healthy nodes in parallel", "path", path, "healthy_node_count", len(clients))
	}

	// Query all nodes in parallel. Each goroutine collects entries from
	// one node and reports success/failure. This avoids the previous
	// sequential approach where a slow node could starve later nodes of
	// remaining context time, leading to incomplete results.
	type nodeResult struct {
		nodeID  string
		entries []*pb.DirEntry
		err     error // non-nil means this node's listing is incomplete
	}

	results := make([]nodeResult, len(clients))
	var wg sync.WaitGroup

	for i, client := range clients {
		wg.Add(1)
		go func(idx int, c pb.MonoFSClient, nid string) {
			defer wg.Done()
			entries, err := sc.readDirFromNode(ctx, c, nid, path)
			results[idx] = nodeResult{nodeID: nid, entries: entries, err: err}
		}(i, client, nodeIDs[i])
	}
	wg.Wait()

	// Check parent context first — if it was cancelled, nothing we can do.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Collect results. If any node failed, retry it once before giving up.
	// A failed node means we may be missing files that are sharded to it.
	allEntries := make(map[string]*pb.DirEntry)
	var failedNodes []int

	for i, r := range results {
		if r.err != nil {
			failedNodes = append(failedNodes, i)
			continue
		}
		for _, entry := range r.entries {
			if _, exists := allEntries[entry.Name]; !exists {
				allEntries[entry.Name] = entry
			}
		}
	}

	// Retry failed nodes once (covers transient network blips).
	if len(failedNodes) > 0 && ctx.Err() == nil {
		if sc.logger != nil {
			sc.logger.Info("readdir: retrying failed nodes",
				"path", path,
				"failed_count", len(failedNodes))
		}

		var retryWg sync.WaitGroup
		for _, idx := range failedNodes {
			retryWg.Add(1)
			go func(i int) {
				defer retryWg.Done()
				entries, err := sc.readDirFromNode(ctx, clients[i], nodeIDs[i], path)
				results[i] = nodeResult{nodeID: nodeIDs[i], entries: entries, err: err}
			}(idx)
		}
		retryWg.Wait()

		// Merge retry results
		for _, idx := range failedNodes {
			r := results[idx]
			if r.err != nil {
				// Node still failing after retry — return error to prevent
				// callers from seeing a partial directory listing.
				if sc.logger != nil {
					sc.logger.Warn("readdir: node failed after retry, returning error to prevent partial results",
						"path", path,
						"node_id", r.nodeID,
						"error", r.err)
				}
				return nil, fmt.Errorf("readdir incomplete: node %s failed: %w", r.nodeID, r.err)
			}
			for _, entry := range r.entries {
				if _, exists := allEntries[entry.Name]; !exists {
					allEntries[entry.Name] = entry
				}
			}
		}
	}

	if sc.logger != nil {
		sc.logger.Debug("readdir: all healthy nodes queried",
			"path", path,
			"total_entries", len(allEntries),
			"total_nodes", len(clients))
	}

	// Convert map to slice
	entries := make([]*pb.DirEntry, 0, len(allEntries))
	for _, entry := range allEntries {
		entries = append(entries, entry)
	}

	// Sort by name for deterministic ordering required by go mod verify
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})

	return entries, nil
}

// readDirFromNode queries a single node for directory entries.
// Returns (entries, nil) on success, or (partial, error) on failure.
// A per-node timeout is applied so one slow node cannot block the caller.
func (sc *ShardedClient) readDirFromNode(ctx context.Context, client pb.MonoFSClient, nodeID, path string) ([]*pb.DirEntry, error) {
	nodeCtx, cancel := context.WithTimeout(ctx, sc.rpcTimeout)
	defer cancel()

	stream, err := client.ReadDir(nodeCtx, &pb.ReadDirRequest{
		Path: path,
	})
	if err != nil {
		if sc.logger != nil {
			sc.logger.Debug("readdir: node RPC error",
				"path", path,
				"node_id", nodeID,
				"error", err)
		}
		return nil, fmt.Errorf("readdir RPC to node %s: %w", nodeID, err)
	}

	var entries []*pb.DirEntry
	for {
		entry, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			if sc.logger != nil {
				sc.logger.Debug("readdir: node stream error",
					"path", path,
					"node_id", nodeID,
					"entries_received", len(entries),
					"error", err)
			}
			return entries, fmt.Errorf("readdir stream from node %s: %w", nodeID, err)
		}
		entries = append(entries, entry)
	}

	if sc.logger != nil {
		sc.logger.Debug("readdir: node completed",
			"path", path,
			"node_id", nodeID,
			"entries", len(entries))
	}
	return entries, nil
}

// Read performs a read operation routed via HRW.
func (sc *ShardedClient) Read(ctx context.Context, path string, offset, size int64) ([]byte, error) {
	// Check if context is already canceled before starting
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Build shard key to match router's ingestion sharding
	key := buildShardKey(path)
	if key == "" {
		key = "/"
	}

	// Get all nodes ranked by HRW for this key
	sc.mu.RLock()
	var rankedNodes []sharding.Node
	if sc.hrw != nil {
		rankedNodes = sc.hrw.GetNodes(key, 3)
	}
	sc.mu.RUnlock()

	if len(rankedNodes) == 0 {
		return nil, fmt.Errorf("no healthy nodes available")
	}

	// Try up to 3 nodes via HRW ranking (primary + 2 fallbacks)
	var lastErr error
	maxAttempts := 3
	if maxAttempts > len(rankedNodes) {
		maxAttempts = len(rankedNodes)
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Check context cancellation between attempts
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		nodeID := rankedNodes[attempt].ID

		sc.mu.RLock()
		client := sc.clients[nodeID]
		sc.mu.RUnlock()

		if client == nil {
			continue
		}

		if sc.logger != nil {
			sc.logger.Debug("read routing",
				"full_path", path,
				"shard_key", key,
				"target_node", nodeID,
				"attempt", attempt+1)
		}

		// Add timeout to prevent hanging on dead nodes
		callCtx, cancel := context.WithTimeout(sc.withClientID(ctx), sc.rpcTimeout)
		stream, err := client.Read(callCtx, &pb.ReadRequest{
			Path:   path,
			Offset: offset,
			Size:   size,
		})
		if err != nil {
			cancel()
			lastErr = err
			if sc.logger != nil {
				sc.logger.Debug("read RPC error, trying next node",
					"path", path,
					"node_id", nodeID,
					"error", err,
					"attempt", attempt+1)
			}
			continue // Try next node in HRW ranking
		}

		// Read stream.
		// Initialise as non-nil empty slice so callers can distinguish
		// "successfully read zero bytes" from "never loaded" (nil).
		data := make([]byte, 0)
		streamErr := false
		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				cancel()
				lastErr = err
				streamErr = true
				if sc.logger != nil {
					sc.logger.Debug("read stream error, trying next node",
						"path", path,
						"node_id", nodeID,
						"error", err,
						"attempt", attempt+1)
				}
				break
			}
			data = append(data, chunk.Data...)
		}
		cancel()

		// If we got data or reached EOF cleanly (no stream error), return
		if !streamErr {
			// Track stats for heartbeat
			atomic.AddInt64(&sc.operationsCount, 1)
			atomic.AddInt64(&sc.bytesRead, int64(len(data)))
			return data, nil
		}
	}

	// FAILOVER: If all HRW-based attempts failed, try router-based routing
	// This handles the case where the primary node is down and failover is active
	if lastErr != nil {
		if sc.logger != nil {
			sc.logger.Debug("HRW-based read failed, trying router-based routing",
				"path", path,
				"last_error", lastErr)
		}

		primaryNode, fallbacks, routerErr := sc.getNodeForFileFromRouter(ctx, path)
		if routerErr == nil && primaryNode != "" {
			// Try the router-suggested nodes
			nodesToTry := append([]string{primaryNode}, fallbacks...)

			for _, nodeID := range nodesToTry {
				sc.mu.RLock()
				client := sc.clients[nodeID]
				sc.mu.RUnlock()

				if client == nil {
					continue
				}

				if sc.logger != nil {
					sc.logger.Debug("trying router-suggested node for read",
						"path", path,
						"node_id", nodeID)
				}

				callCtx, cancel := context.WithTimeout(ctx, sc.rpcTimeout)
				stream, err := client.Read(callCtx, &pb.ReadRequest{
					Path:   path,
					Offset: offset,
					Size:   size,
				})
				if err != nil {
					cancel()
					continue
				}

				data := make([]byte, 0)
				streamErr := false
				for {
					chunk, err := stream.Recv()
					if err == io.EOF {
						break
					}
					if err != nil {
						cancel()
						streamErr = true
						break
					}
					data = append(data, chunk.Data...)
				}
				cancel()

				if !streamErr {
					if sc.logger != nil {
						sc.logger.Info("read succeeded via router-based failover",
							"path", path,
							"node_id", nodeID,
							"bytes", len(data))
					}
					// Track stats for heartbeat
					atomic.AddInt64(&sc.operationsCount, 1)
					atomic.AddInt64(&sc.bytesRead, int64(len(data)))
					return data, nil
				}
			}
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no healthy nodes available")
}

// Close shuts down the client and all connections.
func (sc *ShardedClient) Close() error {
	// Stop heartbeat loop
	if sc.stopHeartbeat != nil {
		select {
		case <-sc.stopHeartbeat:
			// Already closed
		default:
			close(sc.stopHeartbeat)
		}
	}

	// Unregister from router
	if sc.registered {
		sc.unregisterFromRouter("client shutdown")
	}

	// Stop refresh loop
	if sc.stopRefresh != nil {
		select {
		case <-sc.stopRefresh:
			// Already closed
		default:
			close(sc.stopRefresh)
		}
	}
	if sc.refreshTicker != nil {
		sc.refreshTicker.Stop()
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	// Close all backend connections
	for _, conn := range sc.conns {
		conn.Close()
	}
	sc.conns = nil
	sc.clients = nil

	// Close router connection
	if sc.routerConn != nil {
		sc.routerConn.Close()
	}

	return nil
}

// GetClusterInfo returns the current cluster topology.
func (sc *ShardedClient) GetClusterInfo() []sharding.Node {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	if sc.hrw == nil {
		return nil
	}
	return sc.hrw.GetAllNodes()
}

// GetHealthyNodes returns currently healthy nodes.
func (sc *ShardedClient) GetHealthyNodes() []sharding.Node {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	if sc.hrw == nil {
		return nil
	}
	return sc.hrw.GetHealthyNodes()
}

// ============================================================================
// Client Registration and Lifecycle
// ============================================================================

// registerWithRouter registers this client with the router for tracking
func (sc *ShardedClient) registerWithRouter(ctx context.Context) error {
	sc.mu.RLock()
	client := sc.routerClient
	sc.mu.RUnlock()

	if client == nil {
		return fmt.Errorf("no router connection")
	}

	resp, err := client.RegisterClient(ctx, &pb.RegisterClientRequest{
		ClientId:   sc.clientID,
		MountPoint: sc.mountPoint,
		Hostname:   sc.hostname,
		Writable:   sc.writable,
		Version:    sc.version,
	})
	if err != nil {
		return fmt.Errorf("register client RPC: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("registration failed: %s", resp.Message)
	}

	sc.mu.Lock()
	sc.registered = true
	if resp.HeartbeatIntervalMs > 0 {
		sc.heartbeatInterval = time.Duration(resp.HeartbeatIntervalMs) * time.Millisecond
	}
	sc.mu.Unlock()

	if sc.logger != nil {
		sc.logger.Info("registered with router",
			"client_id", sc.clientID,
			"heartbeat_interval", sc.heartbeatInterval)
	}

	return nil
}

// unregisterFromRouter unregisters this client from the router
func (sc *ShardedClient) unregisterFromRouter(reason string) {
	sc.mu.RLock()
	client := sc.routerClient
	registered := sc.registered
	sc.mu.RUnlock()

	if client == nil || !registered {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.UnregisterClient(ctx, &pb.UnregisterClientRequest{
		ClientId: sc.clientID,
		Reason:   reason,
	})
	if err != nil {
		if sc.logger != nil {
			sc.logger.Warn("failed to unregister from router", "error", err)
		}
		return
	}

	sc.mu.Lock()
	sc.registered = false
	sc.mu.Unlock()

	if sc.logger != nil && resp.Success {
		sc.logger.Info("unregistered from router",
			"client_id", sc.clientID,
			"reason", reason)
	}
}

// heartbeatLoop sends periodic heartbeats to the router
func (sc *ShardedClient) heartbeatLoop() {
	sc.mu.RLock()
	interval := sc.heartbeatInterval
	sc.mu.RUnlock()

	if interval == 0 {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-sc.stopHeartbeat:
			return
		case <-ticker.C:
			sc.sendHeartbeat()
		}
	}
}

// sendHeartbeat sends a single heartbeat to the router
func (sc *ShardedClient) sendHeartbeat() {
	sc.mu.RLock()
	client := sc.routerClient
	registered := sc.registered
	sc.mu.RUnlock()

	if client == nil || !registered {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.ClientHeartbeat(ctx, &pb.ClientHeartbeatRequest{
		ClientId:        sc.clientID,
		OperationsCount: atomic.LoadInt64(&sc.operationsCount),
		BytesRead:       atomic.LoadInt64(&sc.bytesRead),
		ErrorsCount:     atomic.LoadInt64(&sc.errorsCount),
	})
	if err != nil {
		if sc.logger != nil {
			sc.logger.Debug("heartbeat failed", "error", err)
		}
		return
	}

	// If router says we should re-register, do so
	if resp.ShouldRegister {
		if sc.logger != nil {
			sc.logger.Info("router requested re-registration")
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		_ = sc.registerWithRouter(ctx2)
		cancel2()
	}
}

// RecordOperation increments the operation counter for metrics
func (sc *ShardedClient) RecordOperation() {
	atomic.AddInt64(&sc.operationsCount, 1)
}

// RecordBytesRead adds to the bytes read counter for metrics
func (sc *ShardedClient) RecordBytesRead(n int64) {
	atomic.AddInt64(&sc.bytesRead, n)
}

// RecordError increments the error counter for metrics
func (sc *ShardedClient) RecordError() {
	atomic.AddInt64(&sc.errorsCount, 1)
}

// IsGuardianVisible reports whether Guardian namespaces should be exposed.
func (sc *ShardedClient) IsGuardianVisible() bool {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.guardianVisible
}

// GetClientID returns the unique client identifier
func (sc *ShardedClient) GetClientID() string {
	return sc.clientID
}

// SetMountPoint sets the mount point for registration (call before first connection)
func (sc *ShardedClient) SetMountPoint(mountPoint string) {
	sc.mu.Lock()
	sc.mountPoint = mountPoint
	sc.mu.Unlock()
}

// BlobFileType mirrors packager.FileType values.
type BlobFileType = uint8

const (
	BlobFileRegular BlobFileType = 0 // regular file
	BlobFileDir     BlobFileType = 1 // directory
	BlobFileSymlink BlobFileType = 2 // symlink (resolved to content)
)

// BlobFile describes a single file to be ingested into the cluster.
type BlobFile struct {
	// Path relative to the blob root, e.g. "go/mod/cache/download/..."
	Path string
	// Content is the raw file bytes.
	Content []byte
	// Mode is the file permission bits.
	Mode uint32
	// FileType: 0=regular, 1=directory, 2=symlink.
	FileType BlobFileType
}

// IngestBlobsResult summarises a blob ingestion operation.
type IngestBlobsResult struct {
	FilesIngested int
	FilesFailed   int
	FailedFiles   []FailedBlobFile // per-file failure details
}

// FailedBlobFile describes a single file that failed to ingest and why.
type FailedBlobFile struct {
	Path   string
	Reason string
}

// IngestBlobs ingests blob files into the backend cluster so they are
// visible to every client under /mnt/monofs/dependency/...
//
// It performs the same sharded distribution that the router uses for regular
// repository files: RegisterRepository on all nodes, IngestFileBatch with
// HRW sharding, and BuildDirectoryIndexes.
func (sc *ShardedClient) IngestBlobs(ctx context.Context, files []BlobFile) (*IngestBlobsResult, error) {
	const displayPath = "dependency"
	storageID := sharding.GenerateStorageID(displayPath)
	result := &IngestBlobsResult{}

	sc.mu.RLock()
	hrw := sc.hrw
	nodeClients := make(map[string]pb.MonoFSClient)
	for id, c := range sc.clients {
		nodeClients[id] = c
	}
	sc.mu.RUnlock()

	if hrw == nil || len(nodeClients) == 0 {
		return nil, fmt.Errorf("not connected to cluster")
	}

	healthyNodes := hrw.GetHealthyNodes()
	if len(healthyNodes) == 0 {
		return nil, fmt.Errorf("no healthy nodes available")
	}

	if sc.logger != nil {
		sc.logger.Info("ingesting blobs into cluster",
			"files", len(files),
			"nodes", len(healthyNodes),
			"display_path", displayPath,
			"storage_id", storageID)
	}

	// Step 1: Register "dependency" repo on all nodes
	for _, node := range healthyNodes {
		client, ok := nodeClients[node.ID]
		if !ok {
			continue
		}

		regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := client.RegisterRepository(regCtx, &pb.RegisterRepositoryRequest{
			StorageId:   storageID,
			DisplayPath: displayPath,
			Source:      "dependency-upload",
		})
		cancel()

		if err != nil && sc.logger != nil {
			sc.logger.Warn("failed to register blob repo on node",
				"node_id", node.ID, "error", err)
		}
	}

	// Step 2: Build FileMetadata for each file and group by target node
	now := time.Now().Unix()
	batches := make(map[string][]*pb.FileMetadata) // nodeID -> files

	for _, f := range files {
		// Compute content hash for the blob
		blobHash := sha256.Sum256(f.Content)
		blobHashStr := hex.EncodeToString(blobHash[:])

		mode := f.Mode
		if mode == 0 {
			if f.FileType == BlobFileDir {
				mode = 0555 // FIXED: Read-only directory for go mod verify
			} else {
				mode = 0444 // FIXED: Read-only file for go mod verify
			}
		}

		meta := &pb.FileMetadata{
			Path:          f.Path,
			Size:          uint64(len(f.Content)),
			Mtime:         now,
			Mode:          mode,
			BlobHash:      blobHashStr,
			Source:        "dependency-upload",
			StorageId:     storageID,
			DisplayPath:   displayPath,
			InlineContent: f.Content,
		}
		if f.FileType == BlobFileDir {
			// Store directory marker: no inline content, no blob hash.
			// The server stores it with IsDir=true so GetAttr returns S_IFDIR.
			meta.BlobHash = ""
			meta.InlineContent = nil
		}
		if f.FileType != BlobFileRegular {
			if meta.BackendMetadata == nil {
				meta.BackendMetadata = make(map[string]string)
			}
			meta.BackendMetadata["file_type"] = fmt.Sprintf("%d", f.FileType)
		}

		// HRW shard key: same scheme as router
		shardKey := sharding.BuildShardKey(storageID, f.Path)
		targetNodes := hrw.GetNodes(shardKey, 1) // primary only for deps
		if len(targetNodes) == 0 {
			result.FilesFailed++
			result.FailedFiles = append(result.FailedFiles, FailedBlobFile{
				Path:   f.Path,
				Reason: "no target node from HRW (all nodes unhealthy?)",
			})
			continue
		}

		primaryNode := targetNodes[0]
		if _, ok := nodeClients[primaryNode.ID]; !ok {
			result.FilesFailed++
			result.FailedFiles = append(result.FailedFiles, FailedBlobFile{
				Path:   f.Path,
				Reason: fmt.Sprintf("no gRPC client for node %s", primaryNode.ID),
			})
			continue
		}

		batches[primaryNode.ID] = append(batches[primaryNode.ID], meta)
	}

	// Step 3: Send batches to each node.
	// Batch by total payload bytes (max ~64 MB per gRPC call) to avoid
	// exceeding the server/client message size limit.
	const maxBatchBytes = 64 * 1024 * 1024 // 64 MB
	const maxBatchFiles = 2000             // hard cap on file count too

	for nodeID, fileMetas := range batches {
		client, ok := nodeClients[nodeID]
		if !ok {
			for _, fm := range fileMetas {
				result.FailedFiles = append(result.FailedFiles, FailedBlobFile{
					Path:   fm.Path,
					Reason: fmt.Sprintf("no gRPC client for node %s", nodeID),
				})
			}
			result.FilesFailed += len(fileMetas)
			continue
		}

		// Build sub-batches respecting byte and count limits
		var batch []*pb.FileMetadata
		var batchBytes int64

		sendBatch := func(b []*pb.FileMetadata) {
			if len(b) == 0 {
				return
			}
			batchCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
			resp, err := client.IngestFileBatch(batchCtx, &pb.IngestFileBatchRequest{
				Files:       b,
				StorageId:   storageID,
				DisplayPath: displayPath,
				Source:      "dependency-upload",
			})
			cancel()

			if err != nil {
				if sc.logger != nil {
					sc.logger.Error("failed to ingest blob batch",
						"node_id", nodeID, "batch_size", len(b), "error", err)
				}
				for _, fm := range b {
					result.FailedFiles = append(result.FailedFiles, FailedBlobFile{
						Path:   fm.Path,
						Reason: fmt.Sprintf("IngestFileBatch on node %s: %v", nodeID, err),
					})
				}
				result.FilesFailed += len(b)
				return
			}

			result.FilesIngested += int(resp.FilesIngested)
			result.FilesFailed += int(resp.FilesFailed)
			// Note: server-side failures don't include per-file details yet,
			// but we log the count so the user sees the total.
		}

		for _, meta := range fileMetas {
			fileBytes := int64(len(meta.InlineContent)) + 512 // content + metadata overhead
			if len(batch) > 0 && (batchBytes+fileBytes > maxBatchBytes || len(batch) >= maxBatchFiles) {
				sendBatch(batch)
				batch = nil
				batchBytes = 0
			}
			batch = append(batch, meta)
			batchBytes += fileBytes
		}
		sendBatch(batch)
	}

	// Step 3b: Send dir-hint entries so every node has a complete directory index.
	// Each node only owns a subset of files (HRW sharding) but needs the full
	// directory listing. Dir-hint entries update the dir index without storing
	// metadata or ownership, keeping storage overhead minimal.
	{
		// Build lightweight dir-hint metadata for ALL files.
		allHints := make([]*pb.FileMetadata, 0, len(files))
		now := time.Now().Unix()
		for _, f := range files {
			mode := f.Mode
			if mode == 0 {
				if f.FileType == BlobFileDir {
					mode = 0555
				} else {
					mode = 0444
				}
			}
			hint := &pb.FileMetadata{
				Path:  f.Path,
				Size:  uint64(len(f.Content)),
				Mtime: now,
				Mode:  mode,
				BackendMetadata: map[string]string{
					"dir_hint": "true",
				},
			}
			if f.FileType != BlobFileRegular {
				hint.BackendMetadata["file_type"] = fmt.Sprintf("%d", f.FileType)
			}
			allHints = append(allHints, hint)
		}

		for _, node := range healthyNodes {
			client, ok := nodeClients[node.ID]
			if !ok {
				continue
			}
			hintCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			_, err := client.IngestFileBatch(hintCtx, &pb.IngestFileBatchRequest{
				Files:       allHints,
				StorageId:   storageID,
				DisplayPath: displayPath,
				Source:      "dir-hint",
			})
			cancel()
			if err != nil && sc.logger != nil {
				sc.logger.Warn("failed to send dir hints",
					"node_id", node.ID, "error", err)
			}
		}
	}

	// Step 4: Build directory indexes on all nodes
	for _, node := range healthyNodes {
		client, ok := nodeClients[node.ID]
		if !ok {
			continue
		}

		idxCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := client.BuildDirectoryIndexes(idxCtx, &pb.BuildDirectoryIndexesRequest{
			StorageId: storageID,
		})
		cancel()

		if err != nil && sc.logger != nil {
			sc.logger.Warn("failed to build dir indexes for blobs",
				"node_id", node.ID, "error", err)
		}
	}

	// Step 5: Mark repo as onboarded on all nodes
	for _, node := range healthyNodes {
		client, ok := nodeClients[node.ID]
		if !ok {
			continue
		}

		obCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, _ = client.MarkRepositoryOnboarded(obCtx, &pb.MarkRepositoryOnboardedRequest{
			StorageId: storageID,
		})
		cancel()
	}

	if sc.logger != nil {
		sc.logger.Info("blob ingestion complete",
			"ingested", result.FilesIngested,
			"failed", result.FilesFailed)
	}

	return result, nil
}

// DeleteBlobsResult summarises a blob deletion operation.
type DeleteBlobsResult struct {
	FilesDeleted int
	FilesFailed  int
}

// DeleteBlobs removes previously-ingested dependency files from the cluster
// backend. Each path is relative to the dependency root (e.g.
// "go/mod/cache/download/..."). The method routes per-file deletions via
// HRW to the correct primary node, then rebuilds directory indexes so the
// FUSE layer no longer sees the deleted files.
func (sc *ShardedClient) DeleteBlobs(ctx context.Context, paths []string) (*DeleteBlobsResult, error) {
	const displayPath = "dependency"
	storageID := sharding.GenerateStorageID(displayPath)
	result := &DeleteBlobsResult{}

	sc.mu.RLock()
	hrw := sc.hrw
	nodeClients := make(map[string]pb.MonoFSClient)
	for id, c := range sc.clients {
		nodeClients[id] = c
	}
	sc.mu.RUnlock()

	if hrw == nil || len(nodeClients) == 0 {
		return nil, fmt.Errorf("not connected to cluster")
	}

	healthyNodes := hrw.GetHealthyNodes()
	if len(healthyNodes) == 0 {
		return nil, fmt.Errorf("no healthy nodes available")
	}

	if sc.logger != nil {
		sc.logger.Info("deleting blobs from cluster",
			"paths", len(paths),
			"storage_id", storageID)
	}

	// Group deletion paths by target node using HRW (same scheme as IngestBlobs).
	batches := make(map[string][]string) // nodeID -> file paths
	for _, p := range paths {
		shardKey := sharding.BuildShardKey(storageID, p)
		targetNodes := hrw.GetNodes(shardKey, 1)
		if len(targetNodes) == 0 {
			result.FilesFailed++
			continue
		}
		primaryNode := targetNodes[0]
		if _, ok := nodeClients[primaryNode.ID]; !ok {
			result.FilesFailed++
			continue
		}
		batches[primaryNode.ID] = append(batches[primaryNode.ID], p)
	}

	// Send deletion requests to each node.
	for nodeID, filePaths := range batches {
		client, ok := nodeClients[nodeID]
		if !ok {
			result.FilesFailed += len(filePaths)
			continue
		}
		for _, fp := range filePaths {
			delCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			resp, err := client.DeleteFile(delCtx, &pb.DeleteFileRequest{
				StorageId: storageID,
				FilePath:  fp,
			})
			cancel()
			if err != nil {
				if sc.logger != nil {
					sc.logger.Warn("failed to delete blob file",
						"node_id", nodeID, "file_path", fp, "error", err)
				}
				result.FilesFailed++
				continue
			}
			if resp.Success {
				result.FilesDeleted++
			} else {
				result.FilesFailed++
			}
		}
	}

	// Rebuild directory indexes on all nodes so the deleted files
	// disappear from readdir listings.
	for _, node := range healthyNodes {
		client, ok := nodeClients[node.ID]
		if !ok {
			continue
		}
		idxCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := client.BuildDirectoryIndexes(idxCtx, &pb.BuildDirectoryIndexesRequest{
			StorageId: storageID,
		})
		cancel()
		if err != nil && sc.logger != nil {
			sc.logger.Warn("failed to rebuild dir indexes after blob deletion",
				"node_id", node.ID, "error", err)
		}
	}

	if sc.logger != nil {
		sc.logger.Info("blob deletion complete",
			"deleted", result.FilesDeleted,
			"failed", result.FilesFailed)
	}

	return result, nil
}

// QueryLogs delegates LogQL queries directly to the MonoFSRouter.
func (sc *ShardedClient) QueryLogs(ctx context.Context, query string) ([]byte, error) {
	sc.mu.RLock()
	routerClient := sc.routerClient
	sc.mu.RUnlock()

	if routerClient == nil {
		return nil, fmt.Errorf("no router connection")
	}

	callCtx, cancel := context.WithTimeout(ctx, sc.rpcTimeout)
	defer cancel()

	resp, err := routerClient.QueryLogs(callCtx, &pb.QueryLogsRequest{
		Query: query,
	})
	if err != nil {
		return nil, fmt.Errorf("QueryLogs failed: %w", err)
	}
	return resp.ResultsJson, nil
}

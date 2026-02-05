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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/sharding"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
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

	// Always update node health from router - this fixes local health corruption
	// caused by markNodeUnhealthy calls on transient RPC failures.
	// The router is the authoritative source of node health.
	if sc.hrw == nil {
		sc.hrw = sharding.NewHRWFromProto(resp.Nodes)
	} else {
		// Always sync health state from router (authoritative source)
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

// buildShardKey builds the sharding key in the format "storageID:filePath"
// to match the router's sharding algorithm used during ingestion.
// The full path format is: "host_domain/org/repo/path/to/file"
// Standard repo IDs are 3 parts: "github_com/owner/repo"
//
// CRITICAL: The storageID is a SHA-256 hash of the displayPath (e.g. "github_com/owner/repo")
// This MUST match the router's generateStorageID() function in ingest.go
//
// Examples:
//
//	"github.com/owner/repo/README.md" -> "sha256(github.com/owner/repo):README.md"
//	"github.com/owner/repo/path/to/file.txt" -> "sha256(github.com/owner/repo):path/to/file.txt"
//	"github.com/owner/repo" -> "sha256(github.com/owner/repo)" (repo dir itself)
func buildShardKey(fullPath string) string {
	if fullPath == "" || fullPath == "/" {
		return fullPath
	}

	// Split path into parts
	parts := strings.Split(fullPath, "/")

	// Standard repo structure: host_domain/org/repo (3 parts)
	// If we have more than 3 parts, assume first 3 are the repo ID
	// and the rest is the file path within the repo
	if len(parts) > 3 {
		// Build shard key: "storageID:filePath" (matches router)
		displayPath := strings.Join(parts[:3], "/")
		filePath := strings.Join(parts[3:], "/")
		// Generate storageID as SHA-256 hash of displayPath (matches router)
		storageID := generateStorageID(displayPath)
		return storageID + ":" + filePath
	}

	// If path has 3 or fewer parts, it's either:
	// - A repo directory itself ("github.com/owner/repo")
	// - An intermediate directory ("github.com" or "github.com/owner")
	// For repo dirs, return the hashed storageID
	if len(parts) == 3 {
		return generateStorageID(fullPath)
	}

	// For intermediate dirs, return the raw path (they're handled specially)
	return fullPath
}

// generateStorageID creates a SHA-256 hash of the display path.
// This MUST match the router's generateStorageID() function in ingest.go
func generateStorageID(displayPath string) string {
	hash := sha256.Sum256([]byte(displayPath))
	return hex.EncodeToString(hash[:])
}

// getNodeForFileFromRouter queries the router for the correct node to serve a file.
// This is used during failover scenarios when the HRW primary is unavailable.
// Returns the primary node ID and a list of fallback node IDs.
func (sc *ShardedClient) getNodeForFileFromRouter(ctx context.Context, fullPath string) (string, []string, error) {
	// Extract storage ID and file path from full path
	parts := strings.Split(fullPath, "/")
	if len(parts) < 4 {
		// Not a file path, can't query router
		return "", nil, fmt.Errorf("not a file path: %s", fullPath)
	}

	displayPath := strings.Join(parts[:3], "/")
	storageID := generateStorageID(displayPath) // Use hashed storageID to match router
	filePath := strings.Join(parts[3:], "/")

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
			// Mark node unhealthy only for connectivity errors
			sc.markNodeUnhealthyOnError(nodeID, err)
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
			sc.markNodeUnhealthyOnError(nodeID, err)
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
					sc.markNodeUnhealthyOnError(nodeID, err)
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
			// Mark node unhealthy only for connectivity errors
			sc.markNodeUnhealthyOnError(nodeID, err)
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
			sc.markNodeUnhealthyOnError(nodeID, err)
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

// ReadDir performs a readdir operation routed via HRW to a single node.
// The directory path is used as the sharding key.
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
		sc.logger.Debug("readdir: querying healthy nodes only", "path", path, "healthy_node_count", len(clients))
	}

	// Collect entries from all nodes
	allEntries := make(map[string]*pb.DirEntry) // deduplicate by name
	queriedNodes := 0
	errorNodes := 0

	for i, client := range clients {
		// Check context cancellation between node queries
		if ctx.Err() != nil {
			if sc.logger != nil {
				sc.logger.Debug("readdir: context canceled, returning partial results",
					"path", path,
					"entries_so_far", len(allEntries),
					"nodes_queried", queriedNodes)
			}
			break
		}

		// Add timeout per node to prevent hanging
		nodeCtx, nodeCancel := context.WithTimeout(ctx, sc.rpcTimeout)
		stream, err := client.ReadDir(nodeCtx, &pb.ReadDirRequest{
			Path: path,
		})
		if err != nil {
			if sc.logger != nil {
				sc.logger.Debug("readdir: node error",
					"path", path,
					"node_id", nodeIDs[i],
					"error", err)
			}
			// Mark node unhealthy only for connectivity errors
			sc.markNodeUnhealthyOnError(nodeIDs[i], err)
			errorNodes++
			nodeCancel()
			// Continue on error - node might not have this path
			continue
		}

		nodeEntries := 0
		for {
			entry, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				if sc.logger != nil {
					sc.logger.Debug("readdir: stream error",
						"path", path,
						"node_id", nodeIDs[i],
						"error", err)
				}
				// Mark node unhealthy only for connectivity errors
				sc.markNodeUnhealthyOnError(nodeIDs[i], err)
				break // Continue with other nodes
			}
			// Deduplicate by name (take first occurrence)
			if _, exists := allEntries[entry.Name]; !exists {
				allEntries[entry.Name] = entry
				nodeEntries++
			}
		}
		queriedNodes++
		if sc.logger != nil {
			sc.logger.Debug("readdir: node completed",
				"path", path,
				"node_id", nodeIDs[i],
				"entries", nodeEntries)
		}
		nodeCancel()
	}

	if sc.logger != nil {
		sc.logger.Debug("readdir: all healthy nodes queried",
			"path", path,
			"total_entries", len(allEntries),
			"queried_nodes", queriedNodes,
			"error_nodes", errorNodes)
	}

	// Convert map to slice
	entries := make([]*pb.DirEntry, 0, len(allEntries))
	for _, entry := range allEntries {
		entries = append(entries, entry)
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
			// Mark node unhealthy only for connectivity errors
			sc.markNodeUnhealthyOnError(nodeID, err)
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

		// Read stream
		var data []byte
		streamErr := false
		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				cancel()
				// Mark node unhealthy only for connectivity errors
				sc.markNodeUnhealthyOnError(nodeID, err)
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
					sc.markNodeUnhealthyOnError(nodeID, err)
					continue
				}

				var data []byte
				streamErr := false
				for {
					chunk, err := stream.Recv()
					if err == io.EOF {
						break
					}
					if err != nil {
						cancel()
						sc.markNodeUnhealthyOnError(nodeID, err)
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

// markNodeUnhealthyOnError marks a node unhealthy only if the connection is actually failing.
// This checks the gRPC connection state rather than error codes, so we only mark
// nodes unhealthy when the connection is in TransientFailure or Shutdown state.
// Application-level errors (like "path not found") won't affect node health.
func (sc *ShardedClient) markNodeUnhealthyOnError(nodeID string, err error) {
	if err == nil {
		return
	}

	// Check the actual connection state
	sc.mu.RLock()
	conn := sc.conns[nodeID]
	sc.mu.RUnlock()

	if conn == nil {
		// No connection found - node might have been removed
		return
	}

	// Get the connection state
	state := conn.GetState()

	// Only mark unhealthy if connection is actually failing
	switch state {
	case connectivity.TransientFailure, connectivity.Shutdown:
		// Connection is truly failing - mark unhealthy
		sc.doMarkNodeUnhealthy(nodeID, state.String())
	default:
		// Connection is Idle, Connecting, or Ready - error is application-level
		if sc.logger != nil {
			sc.logger.Debug("not marking node unhealthy - connection is healthy",
				"node_id", nodeID,
				"conn_state", state.String(),
				"error", err)
		}
	}
}

// doMarkNodeUnhealthy actually marks the node unhealthy in HRW.
func (sc *ShardedClient) doMarkNodeUnhealthy(nodeID string, reason string) {
	sc.mu.RLock()
	hrw := sc.hrw
	sc.mu.RUnlock()

	if hrw != nil {
		hrw.SetNodeHealth(nodeID, false)
		if sc.logger != nil {
			sc.logger.Warn("marked node unhealthy due to connectivity error",
				"node_id", nodeID,
				"reason", reason)
		}
	}
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

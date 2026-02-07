// Package router provides the MonoFSRouter service for cluster topology management.
package router

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/buildlayout"
	"github.com/radryc/monofs/internal/fetcher"
	"github.com/radryc/monofs/internal/sharding"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// RouterConfig holds configuration for the Router service.
type RouterConfig struct {
	ClusterID           string
	RouterName          string
	HealthCheckInterval time.Duration
	UnhealthyThreshold  time.Duration
	PeerRouters         []RouterPeer
	FetcherAddresses    []string // Fetcher cluster addresses for monitoring

	// Replication and failover configuration
	ReplicationFactor     int           // Number of copies (primary + backups), default: 2
	RebalanceDelay        time.Duration // Wait before triggering permanent rebalance after failure, default: 10m
	GracefulFailoverDelay time.Duration // Wait for planned restarts/upgrades, default: 60s
}

// RouterPeer identifies another router instance to aggregate UI data from.
type RouterPeer struct {
	Name string
	URL  string
}

// DefaultRouterConfig returns default router configuration.
func DefaultRouterConfig() RouterConfig {
	return RouterConfig{
		ClusterID:             "monofs-cluster",
		HealthCheckInterval:   5 * time.Second,  // Check every 5 seconds (reduced frequency to avoid lock contention)
		UnhealthyThreshold:    15 * time.Second, // Mark unhealthy after 15 seconds (3 missed checks)
		PeerRouters:           nil,
		ReplicationFactor:     2,                // Primary + 1 backup (protects against 1 node failure)
		RebalanceDelay:        10 * time.Minute, // Wait 10 minutes before permanent rebalancing
		GracefulFailoverDelay: 60 * time.Second, // 60 seconds for planned restarts
	}
}

// Router implements the MonoFSRouter gRPC service.
// It manages cluster topology and node health.
type Router struct {
	pb.UnimplementedMonoFSRouterServer

	mu                   sync.RWMutex
	nodes                map[string]*nodeState
	ingestedRepos        map[string]*ingestedRepo          // repoID -> repo info
	inProgressIngestions map[string]*inProgressIngestion   // storageID -> ingestion progress
	layoutRegistry       *buildlayout.LayoutMapperRegistry // Build layout mappers (Go modules, etc.)
	version              atomic.Int64
	config               RouterConfig
	stopHealth           chan struct{}
	logger               *slog.Logger

	// Version information
	buildVersion string
	buildCommit  string
	buildTime    string

	// Failover tracking
	failoverMap        sync.Map               // failedNodeID -> backupNodeID
	nodeFileIndex      sync.Map               // nodeID -> map[path]bool
	failoverTimers     map[string]*time.Timer // failedNodeID -> rebalance timer (cancelled if node returns)
	failoverTimersMu   sync.Mutex
	failoverStartTimes map[string]time.Time // failedNodeID -> when failure was detected

	// UI communication channels (prevents UI from blocking router operations)
	uiRequests chan UIRequest
	stopUI     chan struct{}

	// UI caches
	repoCacheMu sync.RWMutex
	repoCache   *RepositoriesData
	repoCacheAt time.Time

	statusCacheMu sync.RWMutex
	statusCache   *StatusData
	statusCacheAt time.Time

	routersCacheMu sync.RWMutex
	routersCache   *RoutersData
	routersCacheAt time.Time

	// Search service integration
	searchClient pb.MonoFSSearchClient
	searchConn   *grpc.ClientConn

	// Fetcher cluster integration (for monitoring)
	fetcherClient *fetcher.Client

	// Connected FUSE clients
	clients     map[string]*clientState // clientID -> state
	clientsMu   sync.RWMutex
	stopClients chan struct{}

	// Drain mode for planned maintenance
	drainMode   atomic.Bool
	drainedAt   time.Time
	drainReason string
	drainMu     sync.RWMutex

	// Topology snapshots for rebalancing (version -> node list)
	topologySnapshots   map[int64][]sharding.Node
	topologySnapshotsMu sync.RWMutex

	// Directory index rebuild tracking (nodeID -> set of storageIDs)
	pendingIndexRebuilds   map[string]map[string]bool
	pendingIndexRebuildsMu sync.Mutex
}

// clientState tracks a connected FUSE client with performance metrics.
type clientState struct {
	info            *pb.ClientInfo
	lastHeartbeat   time.Time
	operationsCount int64 // Total FUSE operations
	bytesRead       int64 // Total bytes read
	mu              sync.RWMutex
}

// nodeState tracks a backend node's state.
type nodeState struct {
	info            *pb.NodeInfo
	externalAddress string // External address for host clients (e.g., localhost:9001)
	lastSeen        time.Time
	conn            *grpc.ClientConn
	client          pb.MonoFSClient

	// NEW: Staging and failover state
	status           NodeStatus
	syncProgress     float64  // 0.0 - 1.0 for new nodes syncing
	ownedFilesCount  int64    // Count of files owned by this node
	diskUsedBytes    int64    // Disk space used by this node
	diskTotalBytes   int64    // Total disk space available on this node
	diskFreeBytes    int64    // Disk space free on filesystem
	backingUpNodes   []string // Node IDs this node is backing up
	onboardRequested bool     // Track if onboarding has been requested
}

// NodeStatus represents the operational status of a node.
type NodeStatus int

const (
	NodeStaging NodeStatus = iota // Just registered, not in HRW pool
	NodeSyncing                   // Fetching assigned metadata
	NodeActive                    // Fully operational
)

// String returns the string representation of NodeStatus.
func (ns NodeStatus) String() string {
	switch ns {
	case NodeStaging:
		return "Staging"
	case NodeSyncing:
		return "Syncing"
	case NodeActive:
		return "Active"
	default:
		return "Unknown"
	}
}

// RebalanceState represents the rebalancing status of a repository.
type RebalanceState int

const (
	RebalanceStateStable      RebalanceState = iota // Using current topology, no rebalancing
	RebalanceStateRebalancing                       // Transitioning to new topology
	RebalanceStateDualActive                        // Files exist on both old and new nodes
)

// String returns the string representation of RebalanceState.
func (rs RebalanceState) String() string {
	switch rs {
	case RebalanceStateStable:
		return "Stable"
	case RebalanceStateRebalancing:
		return "Rebalancing"
	case RebalanceStateDualActive:
		return "DualActive"
	default:
		return "Unknown"
	}
}

// ingestedRepo tracks an ingested repository.
type ingestedRepo struct {
	repoID     string
	repoURL    string
	branch     string
	filesCount int64
	ingestedAt time.Time

	// Topology tracking for atomic rebalancing
	topologyVersion   int64          // Topology version when ingested/last rebalanced
	targetTopology    int64          // Target version if rebalancing
	rebalanceState    RebalanceState // Current rebalancing state
	rebalanceProgress float64        // 0.0 - 1.0
	mu                sync.RWMutex   // Protects rebalance state
}

// inProgressIngestion tracks an active ingestion.
type inProgressIngestion struct {
	storageID      string
	repoID         string
	repoURL        string
	branch         string
	startedAt      time.Time
	stage          pb.IngestProgress_Stage
	message        string
	filesProcessed int64
	totalFiles     int64
	mu             sync.RWMutex
}

// NewRouter creates a new Router service.
func NewRouter(cfg RouterConfig, logger *slog.Logger) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "router")
	r := &Router{
		nodes:                make(map[string]*nodeState),
		ingestedRepos:        make(map[string]*ingestedRepo),
		inProgressIngestions: make(map[string]*inProgressIngestion),
		clients:              make(map[string]*clientState),
		topologySnapshots:    make(map[int64][]sharding.Node),
		pendingIndexRebuilds: make(map[string]map[string]bool),
		failoverTimers:       make(map[string]*time.Timer),
		failoverStartTimes:   make(map[string]time.Time),
		config:               cfg,
		stopHealth:           make(chan struct{}),
		stopClients:          make(chan struct{}),
		uiRequests:           make(chan UIRequest, 100), // Buffered to prevent UI blocking
		stopUI:               make(chan struct{}),
		logger:               logger,
	}
	r.version.Store(1)

	// Start UI request handler goroutine
	go r.handleUIRequests()

	// Start client cleanup goroutine
	go r.cleanupStaleClients()

	return r
}

// SetVersion sets the build version information.
func (r *Router) SetVersion(version, commit, buildTime string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buildVersion = version
	r.buildCommit = commit
	r.buildTime = buildTime
}

// markForIndexRebuild marks a repository on a node for directory index rebuild.
// This is used after rebalancing or recovery to defer index updates until operations complete.
func (r *Router) markForIndexRebuild(nodeID, storageID string) {
	r.pendingIndexRebuildsMu.Lock()
	defer r.pendingIndexRebuildsMu.Unlock()

	if r.pendingIndexRebuilds[nodeID] == nil {
		r.pendingIndexRebuilds[nodeID] = make(map[string]bool)
	}
	r.pendingIndexRebuilds[nodeID][storageID] = true

	r.logger.Debug("marked repository for index rebuild",
		"node_id", nodeID,
		"storage_id", storageID)
}

// triggerIndexRebuild triggers directory index rebuild on a specific node for a repository.
func (r *Router) triggerIndexRebuild(nodeID, storageID string) error {
	r.mu.RLock()
	state := r.nodes[nodeID]
	r.mu.RUnlock()

	if state == nil {
		return fmt.Errorf("node not found: %s", nodeID)
	}

	r.logger.Info("triggering directory index rebuild",
		"node_id", nodeID,
		"storage_id", storageID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	resp, err := state.client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})

	if err != nil {
		r.logger.Error("failed to rebuild directory indexes",
			"node_id", nodeID,
			"storage_id", storageID,
			"error", err)
		return err
	}

	r.logger.Info("directory indexes rebuilt",
		"node_id", nodeID,
		"storage_id", storageID,
		"directories", resp.DirectoriesIndexed)

	return nil
}

// SetSearchClient configures the search service client for automatic indexing.
// It will retry the connection in the background if initial connection fails.
func (r *Router) SetSearchClient(addr string) error {
	if addr == "" {
		r.logger.Info("search service not configured")
		return nil
	}

	// Try initial connection
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		r.logger.Warn("failed to connect to search service, will retry in background",
			"addr", addr,
			"error", err)
		// Start background retry
		go r.retrySearchConnection(addr)
		return nil
	}

	r.searchConn = conn
	r.searchClient = pb.NewMonoFSSearchClient(conn)
	r.logger.Info("search service client configured", "addr", addr)
	return nil
}

// retrySearchConnection retries connecting to search service in background
func (r *Router) retrySearchConnection(addr string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if r.searchClient != nil {
			// Already connected
			return
		}

		conn, err := grpc.NewClient(addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			r.logger.Debug("retry search connection failed",
				"addr", addr,
				"error", err)
			continue
		}

		r.searchConn = conn
		r.searchClient = pb.NewMonoFSSearchClient(conn)
		r.logger.Info("search service client connected after retry", "addr", addr)
		return
	}
}

// SetFetcherClient configures the fetcher cluster client for monitoring.
func (r *Router) SetFetcherClient(addrs []string) error {
	if len(addrs) == 0 {
		r.logger.Info("fetcher cluster not configured")
		return nil
	}

	config := fetcher.DefaultClientConfig()
	config.FetcherAddresses = addrs

	client, err := fetcher.NewClient(config, r.logger)
	if err != nil {
		r.logger.Warn("failed to connect to fetcher cluster",
			"addrs", addrs,
			"error", err)
		return err
	}

	r.fetcherClient = client
	r.logger.Info("fetcher cluster client configured", "addrs", addrs)
	return nil
}

// SetLayoutRegistry configures the layout mapper registry for build tool integration.
func (r *Router) SetLayoutRegistry(registry *buildlayout.LayoutMapperRegistry) {
	r.layoutRegistry = registry
}

// GetLayoutRegistry returns the current layout mapper registry.
func (r *Router) GetLayoutRegistry() *buildlayout.LayoutMapperRegistry {
	return r.layoutRegistry
}

// GetFetcherClusterStats returns statistics from all fetchers in the cluster.
func (r *Router) GetFetcherClusterStats(ctx context.Context, includeSourceStats bool) (*fetcher.ClusterStats, error) {
	if r.fetcherClient == nil {
		return nil, fmt.Errorf("fetcher cluster not configured")
	}
	return r.fetcherClient.GetClusterStats(ctx, includeSourceStats)
}

// RegisterNode adds a backend node to the cluster.
func (r *Router) RegisterNode(nodeID, address string, weight uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Connect to the node to verify it's reachable
	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("connect to node %s: %w", nodeID, err)
	}

	client := pb.NewMonoFSClient(conn)

	// Verify node is responsive
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = client.GetNodeInfo(ctx, &pb.NodeInfoRequest{})
	if err != nil {
		conn.Close()
		return fmt.Errorf("verify node %s: %w", nodeID, err)
	}

	// Close existing connection if re-registering
	if existing, ok := r.nodes[nodeID]; ok {
		if existing.conn != nil {
			existing.conn.Close()
		}
	}

	r.nodes[nodeID] = &nodeState{
		info: &pb.NodeInfo{
			NodeId:  nodeID,
			Address: address,
			Weight:  weight,
			Healthy: true,
		},
		lastSeen:         time.Now(),
		conn:             conn,
		client:           client,
		status:           NodeStaging, // NEW: Start in staging
		ownedFilesCount:  0,
		backingUpNodes:   []string{},
		onboardRequested: true, // Will be onboarded immediately
	}

	// DO NOT increment version yet - node is not in active pool

	// NEW: Start metadata sync in background
	go r.onboardNewNode(nodeID)

	return nil
}

// RegisterNodeStatic adds a node without health checking (for initial setup).
func (r *Router) RegisterNodeStatic(nodeID, address string, weight uint32) {
	r.RegisterNodeWithExternalAddr(nodeID, address, "", weight)
}

// RegisterNodeWithExternalAddr registers a node with separate internal and external addresses.
// Internal address is used for router-to-node communication within Docker network.
// External address is returned to clients connecting from outside (e.g., host machine).
func (r *Router) RegisterNodeWithExternalAddr(nodeID, internalAddr, externalAddr string, weight uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nodes[nodeID] = &nodeState{
		info: &pb.NodeInfo{
			NodeId:  nodeID,
			Address: internalAddr,
			Weight:  weight,
			Healthy: true,
		},
		externalAddress:  externalAddr,
		lastSeen:         time.Now(),
		status:           NodeActive, // Static nodes are immediately active
		ownedFilesCount:  0,
		backingUpNodes:   []string{},
		onboardRequested: false, // Will be detected by health check
	}

	r.version.Add(1)
}

// UnregisterNode removes a backend node from the cluster.
func (r *Router) UnregisterNode(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if state, ok := r.nodes[nodeID]; ok {
		if state.conn != nil {
			state.conn.Close()
		}
		delete(r.nodes, nodeID)
		r.version.Add(1)
	}
}

// GetClusterInfo implements pb.MonoFSRouterServer.
func (r *Router) GetClusterInfo(ctx context.Context, req *pb.ClusterInfoRequest) (*pb.ClusterInfoResponse, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Determine if client is external (requesting external addresses)
	useExternalAddrs := req.GetUseExternalAddresses()

	nodes := make([]*pb.NodeInfo, 0, len(r.nodes))
	for _, state := range r.nodes {
		// NEW: Only include nodes in ACTIVE status for HRW sharding
		// But still report all nodes for admin visibility
		includeInCluster := state.status == NodeActive

		// Build metadata map with operational status
		metadata := make(map[string]string)
		metadata["status"] = state.status.String()
		if state.status == NodeSyncing {
			metadata["sync_progress"] = fmt.Sprintf("%.1f%%", state.syncProgress*100)
		}
		if len(state.backingUpNodes) > 0 {
			metadata["backing_up"] = fmt.Sprintf("%d nodes", len(state.backingUpNodes))
		}

		// Determine which address to return
		address := state.info.Address
		if useExternalAddrs && state.externalAddress != "" {
			address = state.externalAddress
			r.logger.Debug("using external address for client",
				"node_id", state.info.NodeId,
				"internal", state.info.Address,
				"external", state.externalAddress)
		}

		// Create NodeInfo with metadata
		nodeInfo := &pb.NodeInfo{
			NodeId:         state.info.NodeId,
			Address:        address,
			Weight:         state.info.Weight,
			Healthy:        state.info.Healthy,
			Tags:           state.info.Tags,
			TotalFiles:     state.ownedFilesCount,
			Metadata:       metadata,
			DiskUsedBytes:  state.diskUsedBytes,
			DiskTotalBytes: state.diskTotalBytes,
			DiskFreeBytes:  state.diskFreeBytes,
		}

		// Only report as part of cluster if Active
		if includeInCluster {
			nodes = append(nodes, nodeInfo)
		} else {
			r.logger.Debug("excluding node from cluster sharding",
				"node_id", state.info.NodeId,
				"status", state.status)
			// Still include in response but clients know it's not in shard pool
			nodes = append(nodes, nodeInfo)
		}
	}

	return &pb.ClusterInfoResponse{
		Nodes:     nodes,
		ClusterId: r.config.ClusterID,
		Version:   r.version.Load(),
	}, nil
}

// Heartbeat implements pb.MonoFSRouterServer.
func (r *Router) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if state, ok := r.nodes[req.NodeId]; ok {
		state.lastSeen = time.Now()
		if !state.info.Healthy {
			r.logger.Info("node recovered via heartbeat", "node_id", req.NodeId)
			state.info.Healthy = true
			r.version.Add(1)

			// Recovery will be triggered by checkAllNodes health check
			// which will verify onboarding status and recover if needed
		}
	}

	return &pb.HeartbeatResponse{
		Acknowledged:   true,
		ClusterVersion: r.version.Load(),
	}, nil
}

// GetNodeForFile returns the correct node for a file, handling rebalancing and failover scenarios.
func (r *Router) GetNodeForFile(ctx context.Context, req *pb.GetNodeForFileRequest) (*pb.GetNodeForFileResponse, error) {
	r.mu.RLock()
	repo, exists := r.ingestedRepos[req.StorageId]
	r.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("repository not found: %s", req.StorageId)
	}

	repo.mu.RLock()
	state := repo.rebalanceState
	topologyVersion := repo.topologyVersion
	targetTopology := repo.targetTopology
	repo.mu.RUnlock()

	key := req.StorageId + ":" + req.FilePath

	// Helper to check if a node is healthy and add failover if needed
	checkNodeWithFailover := func(primaryNodeID string) (string, []string) {
		r.mu.RLock()
		defer r.mu.RUnlock()

		primaryState := r.nodes[primaryNodeID]
		if primaryState != nil && primaryState.info.Healthy && primaryState.status == NodeActive {
			// Primary is healthy, return it with other healthy nodes as fallbacks (HRW order)
			fallbacks := r.getHRWFallbacks(key, primaryNodeID, 2)
			return primaryNodeID, fallbacks
		}

		// Primary is unhealthy - check if we have a failover assignment
		if backupNodeIDVal, hasFailover := r.failoverMap.Load(primaryNodeID); hasFailover {
			backupNodeID := backupNodeIDVal.(string)
			backupState := r.nodes[backupNodeID]
			if backupState != nil && backupState.info.Healthy && backupState.status == NodeActive {
				r.logger.Debug("routing to failover node",
					"primary_node", primaryNodeID,
					"failover_node", backupNodeID,
					"key", key)
				// Return backup node as primary, with other healthy nodes as fallbacks
				fallbacks := r.getHRWFallbacks(key, backupNodeID, 2)
				return backupNodeID, fallbacks
			}
		}

		// No failover assignment - return next healthy nodes per HRW
		// This allows on-demand fetch to work on any healthy node
		fallbacks := r.getHRWFallbacks(key, "", 3)
		if len(fallbacks) > 0 {
			return fallbacks[0], fallbacks[1:]
		}

		// No healthy nodes at all
		return "", nil
	}

	switch state {
	case RebalanceStateStable:
		// Normal case: Use current topology
		r.mu.RLock()
		primaryNode := r.getNodeForTopologyVersion(key, topologyVersion)
		r.mu.RUnlock()

		nodeID, fallbacks := checkNodeWithFailover(primaryNode)
		if nodeID == "" {
			return nil, fmt.Errorf("no healthy nodes available")
		}

		// Short TTL if we're using failover
		ttl := int64(300) // 5 minutes - stable
		if nodeID != primaryNode {
			ttl = 30 // 30 seconds during failover
		}

		return &pb.GetNodeForFileResponse{
			NodeId:          nodeID,
			FallbackNodeIds: fallbacks,
			CacheTtlSeconds: ttl,
			RebalanceState:  "stable",
		}, nil

	case RebalanceStateRebalancing:
		// Copying in progress: Try new location first, fallback to old
		r.mu.RLock()
		newNode := r.getNodeForTopologyVersion(key, targetTopology)
		oldNode := r.getNodeForTopologyVersion(key, topologyVersion)
		r.mu.RUnlock()

		nodeID, fallbacks := checkNodeWithFailover(newNode)
		if nodeID == "" {
			// Try old node
			nodeID, fallbacks = checkNodeWithFailover(oldNode)
		}
		if nodeID == "" {
			return nil, fmt.Errorf("no healthy nodes available")
		}

		// Ensure old node is in fallbacks if different
		if oldNode != newNode && oldNode != nodeID {
			fallbacks = append([]string{oldNode}, fallbacks...)
		}

		return &pb.GetNodeForFileResponse{
			NodeId:          nodeID,
			FallbackNodeIds: fallbacks,
			CacheTtlSeconds: 10, // 10 seconds - rebalancing
			RebalanceState:  "rebalancing",
		}, nil

	case RebalanceStateDualActive:
		// Both locations valid: Prefer new, but old also works
		r.mu.RLock()
		newNode := r.getNodeForTopologyVersion(key, targetTopology)
		oldNode := r.getNodeForTopologyVersion(key, topologyVersion)
		r.mu.RUnlock()

		nodeID, fallbacks := checkNodeWithFailover(newNode)
		if nodeID == "" {
			nodeID, fallbacks = checkNodeWithFailover(oldNode)
		}
		if nodeID == "" {
			return nil, fmt.Errorf("no healthy nodes available")
		}

		// Ensure old node is in fallbacks if different
		if oldNode != newNode && oldNode != nodeID {
			fallbacks = append([]string{oldNode}, fallbacks...)
		}

		return &pb.GetNodeForFileResponse{
			NodeId:          nodeID,
			FallbackNodeIds: fallbacks,
			CacheTtlSeconds: 60, // 1 minute - dual active
			RebalanceState:  "dual-active",
		}, nil

	default:
		return nil, fmt.Errorf("unknown rebalance state: %v", state)
	}
}

// getHRWFallbacks returns fallback nodes for a key in HRW order, excluding a specific node.
// Must be called with r.mu held.
func (r *Router) getHRWFallbacks(key string, excludeNodeID string, count int) []string {
	// Build list of healthy active nodes
	nodes := make([]sharding.Node, 0, len(r.nodes))
	for _, state := range r.nodes {
		if state.info.Healthy && state.status == NodeActive {
			nodes = append(nodes, sharding.Node{
				ID:      state.info.NodeId,
				Address: state.info.Address,
				Weight:  state.info.Weight,
				Healthy: true,
			})
		}
	}

	if len(nodes) == 0 {
		return nil
	}

	sharder := sharding.NewHRW(nodes)
	rankedNodes := sharder.GetNodes(key, count+1) // Get one extra since we might exclude

	fallbacks := make([]string, 0, count)
	for _, node := range rankedNodes {
		if node.ID != excludeNodeID {
			fallbacks = append(fallbacks, node.ID)
			if len(fallbacks) >= count {
				break
			}
		}
	}

	return fallbacks
}

// getNodeForTopologyVersion calculates the node for a key using a specific topology version.
// It uses topology snapshots captured during rebalancing to ensure consistent routing.
// Must be called with r.mu held for reading.
func (r *Router) getNodeForTopologyVersion(key string, version int64) string {
	// Try to get snapshot for this version
	r.topologySnapshotsMu.RLock()
	snapshot, exists := r.topologySnapshots[version]
	r.topologySnapshotsMu.RUnlock()

	var nodes []sharding.Node
	if exists {
		// Use snapshot if available
		nodes = snapshot
	} else {
		// Fallback: Use current topology (for backwards compatibility)
		// This happens when version matches current version or no snapshot exists
		nodes = make([]sharding.Node, 0, len(r.nodes))
		for _, state := range r.nodes {
			if state.info.Healthy && state.status == NodeActive {
				nodes = append(nodes, sharding.Node{
					ID:      state.info.NodeId,
					Address: state.info.Address,
					Weight:  state.info.Weight,
					Healthy: true,
				})
			}
		}
	}

	if len(nodes) == 0 {
		return ""
	}

	// Create sharder and get node
	sharder := sharding.NewHRW(nodes)
	node := sharder.GetNode(key)
	if node == nil {
		return ""
	}
	return node.ID
}

// StartHealthCheck starts the background health checking loop.
func (r *Router) StartHealthCheck() {
	go r.healthCheckLoop()
	// Discover existing repositories from nodes on startup (after nodes connect)
	go func() {
		// Wait for initial health check to establish connections
		time.Sleep(r.config.HealthCheckInterval + time.Second)
		r.discoverClusterRepositories()
	}()
}

// discoverClusterRepositories queries all connected nodes to discover existing repositories.
// This builds the router's view of the cluster state on startup, enabling recovery/rebalancing.
func (r *Router) discoverClusterRepositories() {
	r.logger.Info("discovering cluster repositories from nodes")

	// Get snapshot of connected nodes
	r.mu.RLock()
	nodeClients := make(map[string]pb.MonoFSClient)
	for nodeID, state := range r.nodes {
		if state.client != nil && state.info.Healthy {
			nodeClients[nodeID] = state.client
		}
	}
	r.mu.RUnlock()

	if len(nodeClients) == 0 {
		r.logger.Warn("no healthy nodes available for repository discovery")
		return
	}

	// Collect all unique repositories from all nodes
	discoveredRepos := make(map[string]*ingestedRepo) // storageID -> repo info
	var mu sync.Mutex
	var wg sync.WaitGroup

	for nodeID, client := range nodeClients {
		wg.Add(1)
		go func(nid string, c pb.MonoFSClient) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// List repositories on this node
			listResp, err := c.ListRepositories(ctx, &pb.ListRepositoriesRequest{})
			if err != nil {
				r.logger.Warn("failed to list repositories from node",
					"node_id", nid,
					"error", err)
				return
			}

			r.logger.Debug("discovered repositories on node",
				"node_id", nid,
				"count", len(listResp.RepositoryIds))

			// Get info for each repository
			for _, storageID := range listResp.RepositoryIds {
				infoCtx, infoCancel := context.WithTimeout(context.Background(), 5*time.Second)
				infoResp, err := c.GetRepositoryInfo(infoCtx, &pb.GetRepositoryInfoRequest{
					StorageId: storageID,
				})
				infoCancel()

				if err != nil {
					r.logger.Debug("failed to get repo info",
						"node_id", nid,
						"storage_id", storageID,
						"error", err)
					continue
				}

				mu.Lock()
				if _, exists := discoveredRepos[storageID]; !exists {
					discoveredRepos[storageID] = &ingestedRepo{
						repoID:            infoResp.DisplayPath,
						repoURL:           infoResp.Source,
						branch:            infoResp.Ref,
						filesCount:        0, // Will be updated by health checks
						ingestedAt:        time.Now(),
						topologyVersion:   r.version.Load(),
						rebalanceState:    RebalanceStateStable,
						rebalanceProgress: 1.0,
					}
					r.logger.Debug("discovered repository",
						"storage_id", storageID,
						"display_path", infoResp.DisplayPath,
						"from_node", nid)
				}
				mu.Unlock()
			}
		}(nodeID, client)
	}

	wg.Wait()

	// Merge discovered repos into ingestedRepos (don't overwrite existing)
	r.mu.Lock()
	newCount := 0
	for storageID, repo := range discoveredRepos {
		if _, exists := r.ingestedRepos[storageID]; !exists {
			r.ingestedRepos[storageID] = repo
			newCount++
		}
	}
	r.mu.Unlock()

	r.logger.Info("cluster repository discovery complete",
		"discovered", len(discoveredRepos),
		"new_added", newCount,
		"total_tracked", len(r.ingestedRepos))
}

// StopHealthCheck stops the background health checking.
func (r *Router) StopHealthCheck() {
	close(r.stopHealth)
}

// healthCheckLoop periodically checks node health.
func (r *Router) healthCheckLoop() {
	ticker := time.NewTicker(r.config.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.checkAllNodes()
		case <-r.stopHealth:
			return
		}
	}
}

// checkAllNodes checks health of all registered nodes.
func (r *Router) checkAllNodes() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()

	// Log overall cluster health state at start of each check
	healthyCount := 0
	for _, s := range r.nodes {
		if s.info.Healthy {
			healthyCount++
		}
	}
	r.logger.Debug("health check starting", "total_nodes", len(r.nodes), "healthy_nodes", healthyCount)

	for nodeID, state := range r.nodes {
		// Check if node has timed out based on lastSeen (for passive monitoring)
		timeSinceLastSeen := now.Sub(state.lastSeen)

		// NEW: Detect node transition to unhealthy (but still try to reconnect)
		if timeSinceLastSeen > r.config.UnhealthyThreshold {
			if state.info.Healthy {
				r.logger.Warn("node became unhealthy, assigning failover",
					"node_id", nodeID,
					"last_seen", timeSinceLastSeen,
					"status", state.status.String())
				state.info.Healthy = false
				r.version.Add(1)

				// Close stale connection
				if state.conn != nil {
					state.conn.Close()
					state.conn = nil
					state.client = nil
				}

				// NEW: Assign failover node (only for NodeActive nodes that held data, and not in drain mode)
				if state.status == NodeActive && !r.IsDrained() {
					r.assignFailoverNodeLocked(nodeID)
				}
			}
			// IMPORTANT: Don't skip - still try to reconnect even if unhealthy
			// This allows nodes to recover when they come back online
		}

		// Try to create client connection if needed (for statically registered nodes)
		if state.client == nil && state.conn == nil {
			conn, err := grpc.NewClient(state.info.Address,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
			)
			if err != nil {
				r.logger.Debug("failed to connect to node", "node_id", nodeID, "error", err)
				// Mark unhealthy only if connection fails AND we're past threshold
				if timeSinceLastSeen > r.config.UnhealthyThreshold/2 && state.info.Healthy {
					r.logger.Warn("node became unhealthy (connection failed), assigning failover",
						"node_id", nodeID,
						"last_seen", timeSinceLastSeen,
						"status", state.status.String())
					state.info.Healthy = false
					r.version.Add(1)
					// Assign failover node (only for NodeActive nodes that held data, and not in drain mode)
					if state.status == NodeActive && !r.IsDrained() {
						r.assignFailoverNodeLocked(nodeID)
					}
				}
				continue
			}
			state.conn = conn
			state.client = pb.NewMonoFSClient(conn)
			r.logger.Info("established connection to node", "node_id", nodeID, "address", state.info.Address)
		}

		// Active health check if we have a connection
		if state.client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			nodeInfo, err := state.client.GetNodeInfo(ctx, &pb.NodeInfoRequest{})
			cancel()

			if err != nil {
				r.logger.Warn("health check failed", "node_id", nodeID, "error", err, "was_healthy", state.info.Healthy)
				// Close stale connection
				if state.conn != nil {
					state.conn.Close()
					state.conn = nil
					state.client = nil
				}
				// Only mark unhealthy if we're past threshold
				if timeSinceLastSeen > r.config.UnhealthyThreshold/2 && state.info.Healthy {
					r.logger.Warn("node became unhealthy (health check failed), assigning failover",
						"node_id", nodeID,
						"last_seen", timeSinceLastSeen,
						"status", state.status.String())
					state.info.Healthy = false
					r.version.Add(1)
					// Assign failover node (only for NodeActive nodes that held data, and not in drain mode)
					if state.status == NodeActive && !r.IsDrained() {
						r.assignFailoverNodeLocked(nodeID)
					}
				}
			} else {
				state.lastSeen = now

				// Update file count and disk usage from node info
				state.ownedFilesCount = nodeInfo.TotalFiles
				state.diskUsedBytes = nodeInfo.DiskUsedBytes
				state.diskTotalBytes = nodeInfo.DiskTotalBytes
				state.diskFreeBytes = nodeInfo.DiskFreeBytes

				// NEW: Detect node recovery/health restoration
				if !state.info.Healthy && state.status == NodeActive {
					r.logger.Info("node recovered", "node_id", nodeID)
					state.info.Healthy = true
					r.version.Add(1)

					// Cancel rebalance timer if still pending (node returned in time)
					timerWasCancelled := r.cancelFailoverTimer(nodeID)

					// Clear any failover mapping for this node
					if backupNodeID, hadFailover := r.failoverMap.LoadAndDelete(nodeID); hadFailover {
						r.logger.Info("cleared failover mapping for recovered node",
							"node_id", nodeID,
							"backup_node", backupNodeID,
							"timer_was_pending", timerWasCancelled)

						// Remove this node from backup node's backingUpNodes list
						if backupState, exists := r.nodes[backupNodeID.(string)]; exists {
							for i, id := range backupState.backingUpNodes {
								if id == nodeID {
									backupState.backingUpNodes = append(
										backupState.backingUpNodes[:i],
										backupState.backingUpNodes[i+1:]...,
									)
									r.logger.Debug("removed from backup node's list",
										"recovered_node", nodeID,
										"backup_node", backupNodeID)
									break
								}
							}
						}
					}

					// Handle recovery based on whether timer had fired
					if timerWasCancelled {
						// Node returned before RebalanceDelay elapsed
						// Only sync repos that were ingested during the outage
						go r.handleEarlyRecovery(nodeID)
					} else {
						// Timer already fired (or was never set), full rebalance was triggered
						// Just need to sync any remaining repos
						go r.triggerRebalanceOnRecovery(nodeID)
					}
				} else if !state.info.Healthy {
					// Node was unhealthy, now healthy (but maybe never was ACTIVE before)
					state.info.Healthy = true
					r.version.Add(1)

					// Cancel any pending timer and clear failover state
					r.cancelFailoverTimer(nodeID)

					// Clear any failover mapping
					if backupNodeID, hadFailover := r.failoverMap.LoadAndDelete(nodeID); hadFailover {
						r.logger.Info("cleared failover mapping for newly healthy node",
							"node_id", nodeID,
							"backup_node", backupNodeID)

						// Remove from backup node's backingUpNodes list
						if backupState, exists := r.nodes[backupNodeID.(string)]; exists {
							for i, id := range backupState.backingUpNodes {
								if id == nodeID {
									backupState.backingUpNodes = append(
										backupState.backingUpNodes[:i],
										backupState.backingUpNodes[i+1:]...,
									)
									r.logger.Debug("removed from backup node's list",
										"recovered_node", nodeID,
										"backup_node", backupNodeID)
									break
								}
							}
						}
					}

					// Handle recovery for newly healthy nodes
					if state.status == NodeActive {
						go r.handleEarlyRecovery(nodeID)
					}
				}

				// Check onboarding status and trigger recovery if needed
				if state.status == NodeActive && state.client != nil && !state.onboardRequested {
					// Only check if not already in progress
					go r.checkAndRecoverNode(nodeID, state)
				}
			}
		}
		_ = nodeID // Silence unused variable warning
	}
}

// Close shuts down the router and all connections.
func (r *Router) Close() error {
	r.StopHealthCheck()

	// Stop UI handler
	close(r.stopUI)

	// Close search connection
	if r.searchConn != nil {
		r.searchConn.Close()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, state := range r.nodes {
		if state.conn != nil {
			state.conn.Close()
		}
	}
	r.nodes = nil

	return nil
}

// NodeCount returns the number of registered nodes.
func (r *Router) NodeCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.nodes)
}

// HealthyNodeCount returns the number of healthy nodes.
func (r *Router) HealthyNodeCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	count := 0
	for _, state := range r.nodes {
		if state.info.Healthy {
			count++
		}
	}
	return count
}

// assignFailoverNode assigns a healthy node to cover for a failed node.
func (r *Router) assignFailoverNode(failedNodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assignFailoverNodeLocked(failedNodeID)
}

// assignFailoverNodeLocked assigns a healthy node to cover for a failed node.
// Must be called with r.mu held.
// The backup node already has replica data from ingestion, so failover is instant.
// A delayed rebalance timer is started - if the node doesn't return within
// RebalanceDelay, permanent rebalancing will be triggered.
func (r *Router) assignFailoverNodeLocked(failedNodeID string) {
	// Find healthiest node with least load
	var bestNode *nodeState
	var bestNodeID string
	minBackups := 999

	for nodeID, state := range r.nodes {
		if nodeID == failedNodeID || !state.info.Healthy || state.status != NodeActive {
			continue
		}

		if len(state.backingUpNodes) < minBackups {
			bestNode = state
			bestNodeID = nodeID
			minBackups = len(state.backingUpNodes)
		}
	}

	if bestNode == nil {
		r.logger.Error("no healthy nodes available for failover", "failed_node", failedNodeID)
		return
	}

	// Assign failover
	r.failoverMap.Store(failedNodeID, bestNodeID)
	bestNode.backingUpNodes = append(bestNode.backingUpNodes, failedNodeID)

	r.logger.Info("assigned failover node",
		"failed_node", failedNodeID,
		"backup_node", bestNodeID)

	// Track when failure started (for detecting repos ingested during outage)
	r.failoverTimersMu.Lock()
	r.failoverStartTimes[failedNodeID] = time.Now()

	// Start delayed rebalance timer
	// If node returns before timer fires, timer is cancelled and no rebalance happens
	// If timer fires, permanent rebalancing is triggered
	if existingTimer, exists := r.failoverTimers[failedNodeID]; exists {
		existingTimer.Stop()
	}
	r.failoverTimers[failedNodeID] = time.AfterFunc(r.config.RebalanceDelay, func() {
		r.triggerDelayedRebalance(failedNodeID)
	})
	r.failoverTimersMu.Unlock()

	r.logger.Info("started rebalance timer",
		"failed_node", failedNodeID,
		"backup_node", bestNodeID,
		"delay", r.config.RebalanceDelay)
}

// triggerDelayedRebalance is called when the rebalance timer fires after RebalanceDelay.
// This means the node has been down long enough that we should permanently redistribute its data.
func (r *Router) triggerDelayedRebalance(failedNodeID string) {
	r.logger.Info("rebalance timer fired - node still down, triggering permanent rebalance",
		"failed_node", failedNodeID,
		"delay_elapsed", r.config.RebalanceDelay)

	// Clean up timer tracking
	r.failoverTimersMu.Lock()
	delete(r.failoverTimers, failedNodeID)
	// Keep failoverStartTimes for now - needed for recovery
	r.failoverTimersMu.Unlock()

	// Collect all repos for rebalancing
	r.mu.RLock()
	allRepos := make([]string, 0, len(r.ingestedRepos))
	for storageID := range r.ingestedRepos {
		allRepos = append(allRepos, storageID)
	}
	r.mu.RUnlock()

	r.logger.Info("triggering permanent rebalance for all repositories",
		"failed_node", failedNodeID,
		"repo_count", len(allRepos))

	for _, storageID := range allRepos {
		go r.rebalanceRepository(storageID)
	}
}

// cancelFailoverTimer cancels the pending rebalance timer for a node.
// Called when a node recovers before the timer fires.
// Returns true if a timer was cancelled (node was in failover state).
func (r *Router) cancelFailoverTimer(nodeID string) bool {
	r.failoverTimersMu.Lock()
	defer r.failoverTimersMu.Unlock()

	timer, exists := r.failoverTimers[nodeID]
	if !exists {
		return false
	}

	timer.Stop()
	delete(r.failoverTimers, nodeID)
	r.logger.Info("cancelled rebalance timer - node recovered",
		"node_id", nodeID)
	return true
}

// getReposIngestedDuringOutage returns repos that were ingested while a node was down.
// These repos need to be synced to the returning node.
func (r *Router) getReposIngestedDuringOutage(nodeID string) []string {
	r.failoverTimersMu.Lock()
	failoverStart, wasDown := r.failoverStartTimes[nodeID]
	r.failoverTimersMu.Unlock()

	if !wasDown {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	var newRepos []string
	for storageID, repo := range r.ingestedRepos {
		if repo.ingestedAt.After(failoverStart) {
			newRepos = append(newRepos, storageID)
		}
	}

	return newRepos
}

// clearFailoverState cleans up all failover tracking for a recovered node.
// Also notifies backup nodes to clear their failover cache.
func (r *Router) clearFailoverState(nodeID string) {
	// Get the backup node before deleting the failover mapping
	backupNodeID, hadFailover := r.failoverMap.LoadAndDelete(nodeID)

	r.failoverTimersMu.Lock()
	delete(r.failoverStartTimes, nodeID)
	if timer, exists := r.failoverTimers[nodeID]; exists {
		timer.Stop()
		delete(r.failoverTimers, nodeID)
	}
	r.failoverTimersMu.Unlock()

	// Notify backup nodes to clear their failover cache
	if hadFailover {
		backupID := backupNodeID.(string)
		r.mu.RLock()
		backupState := r.nodes[backupID]
		r.mu.RUnlock()

		if backupState != nil && backupState.client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := backupState.client.ClearFailoverCache(ctx, &pb.ClearFailoverCacheRequest{
				RecoveredNodeId: nodeID,
			})
			cancel()

			if err != nil {
				r.logger.Warn("failed to clear failover cache on backup node",
					"recovered_node", nodeID,
					"backup_node", backupID,
					"error", err)
			} else {
				r.logger.Info("cleared failover cache on backup node",
					"recovered_node", nodeID,
					"backup_node", backupID)
			}
		}
	}
}

// handleEarlyRecovery handles a node that recovered before the rebalance timer fired.
// For repos that existed BEFORE the outage: node still has data, no action needed.
// For repos ingested DURING the outage: node needs to sync them (mark as not onboarded).
func (r *Router) handleEarlyRecovery(nodeID string) {
	// Get repos ingested while this node was down
	newRepos := r.getReposIngestedDuringOutage(nodeID)

	r.logger.Info("handling early node recovery",
		"node_id", nodeID,
		"repos_ingested_during_outage", len(newRepos))

	if len(newRepos) == 0 {
		// No new repos during outage - node has all data, just clear state
		r.clearFailoverState(nodeID)
		r.logger.Info("early recovery complete - no repos to sync",
			"node_id", nodeID)
		return
	}

	// Node needs to sync the repos that were ingested while it was down
	r.logger.Info("node needs to sync repos ingested during outage",
		"node_id", nodeID,
		"repos", newRepos)

	// Get node client
	r.mu.RLock()
	state := r.nodes[nodeID]
	r.mu.RUnlock()

	if state == nil || state.client == nil {
		r.logger.Warn("cannot sync repos - node not available",
			"node_id", nodeID)
		r.clearFailoverState(nodeID)
		return
	}

	// For each new repo, trigger the onboarding/sync process
	// This marks them as not-onboarded so checkAndRecoverNode will sync them
	for _, storageID := range newRepos {
		r.mu.RLock()
		repo, exists := r.ingestedRepos[storageID]
		r.mu.RUnlock()

		if !exists {
			continue
		}

		// Register the repository on the returning node (it doesn't know about it)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := state.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
			StorageId:   storageID,
			DisplayPath: repo.repoID,
			Source:      repo.repoURL,
		})
		cancel()

		if err != nil {
			r.logger.Warn("failed to register repo on recovered node",
				"node_id", nodeID,
				"storage_id", storageID,
				"error", err)
		} else {
			r.logger.Info("registered missing repo on recovered node",
				"node_id", nodeID,
				"storage_id", storageID)
		}
	}

	// Clear failover state
	r.clearFailoverState(nodeID)

	// Trigger checkAndRecoverNode which will handle the actual file sync
	go r.checkAndRecoverNode(nodeID, state)
}

// checkAndRecoverNode verifies node onboarding status and triggers recovery if needed.
// This handles nodes that were offline during repository ingestion.
func (r *Router) checkAndRecoverNode(nodeID string, state *nodeState) {
	// Get cluster's known repositories
	r.mu.RLock()
	clusterRepos := make(map[string]*ingestedRepo)
	for storageID, repo := range r.ingestedRepos {
		clusterRepos[storageID] = repo
	}
	fileCount := state.ownedFilesCount
	r.mu.RUnlock()

	if len(clusterRepos) == 0 {
		r.logger.Debug("no cluster repositories to check for recovery", "node_id", nodeID)
		return // No repositories to check
	}

	r.logger.Debug("checking node onboarding status",
		"node_id", nodeID,
		"owned_files", fileCount,
		"cluster_repos", len(clusterRepos))

	// Query node's onboarding status
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	statusResp, err := state.client.GetOnboardingStatus(ctx, &pb.OnboardingStatusRequest{
		NodeId: nodeID,
	})
	cancel()

	if err != nil {
		r.logger.Warn("failed to get onboarding status",
			"node_id", nodeID,
			"error", err)
		return
	}

	// Check for missing or incomplete repositories
	missingRepos := []string{}
	incompleteRepos := []string{}

	for storageID := range clusterRepos {
		onboarded, exists := statusResp.Repositories[storageID]
		if !exists {
			missingRepos = append(missingRepos, storageID)
		} else if !onboarded {
			incompleteRepos = append(incompleteRepos, storageID)
		}
	}

	if len(missingRepos) > 0 || len(incompleteRepos) > 0 {
		r.logger.Info("detected node needs recovery",
			"node_id", nodeID,
			"missing_repos", len(missingRepos),
			"incomplete_repos", len(incompleteRepos),
			"missing_repo_ids", missingRepos,
			"incomplete_repo_ids", incompleteRepos)

		// Trigger recovery
		r.mu.Lock()
		if !state.onboardRequested {
			state.status = NodeSyncing
			state.onboardRequested = true
			r.mu.Unlock()

			go r.recoverNode(nodeID, missingRepos, incompleteRepos)
		} else {
			r.logger.Debug("recovery already in progress for node", "node_id", nodeID)
			r.mu.Unlock()
		}
	} else {
		r.logger.Debug("node fully onboarded, no recovery needed",
			"node_id", nodeID,
			"repositories", len(clusterRepos))
	}
}

// recoverNode synchronizes missing/incomplete repositories to a node.
// This is the primary recovery mechanism for nodes with missing data.
func (r *Router) recoverNode(nodeID string, missingRepos, incompleteRepos []string) {
	r.logger.Info("starting node recovery",
		"node_id", nodeID,
		"missing_repos", len(missingRepos),
		"incomplete_repos", len(incompleteRepos))

	r.mu.RLock()
	targetState := r.nodes[nodeID]

	// Find source nodes (healthy active nodes with data)
	sourceNodes := make(map[string]*nodeState)
	for nid, ns := range r.nodes {
		if nid != nodeID && ns.info.Healthy && ns.status == NodeActive && ns.ownedFilesCount > 0 {
			sourceNodes[nid] = ns
		}
	}
	r.mu.RUnlock()

	if targetState == nil {
		r.logger.Warn("cannot recover node - target node not found", "node_id", nodeID)
		return
	}

	// If no source nodes available, we can still register repos and let rebalancing handle data
	// This is important when all nodes were offline and come back - they already have their data
	noSourceNodes := len(sourceNodes) == 0
	if noSourceNodes {
		r.logger.Info("no source nodes available for recovery, will register repos and trigger rebalancing",
			"node_id", nodeID,
			"missing_repos", len(missingRepos),
			"incomplete_repos", len(incompleteRepos))
	}

	allReposToRecover := append(missingRepos, incompleteRepos...)
	totalRecovered := int64(0)

	for _, storageID := range allReposToRecover {
		r.logger.Info("recovering repository",
			"node_id", nodeID,
			"storage_id", storageID)

		// Get repository metadata
		r.mu.RLock()
		repoInfo, exists := r.ingestedRepos[storageID]
		r.mu.RUnlock()

		if !exists {
			r.logger.Warn("repository not found in cluster tracking", "storage_id", storageID)
			continue
		}

		// STEP 1: Register repository on target node FIRST
		// This ensures the repo is available even if rebalancing moves files here later
		r.logger.Info("registering repository on recovering node",
			"node_id", nodeID,
			"storage_id", storageID,
			"display_path", repoInfo.repoID,
			"repo_url", repoInfo.repoURL)

		regCtx, regCancel := context.WithTimeout(context.Background(), 10*time.Second)
		regResp, err := targetState.client.RegisterRepository(regCtx, &pb.RegisterRepositoryRequest{
			StorageId:   storageID,
			DisplayPath: repoInfo.repoID,
			Source:      repoInfo.repoURL,
		})
		regCancel()

		if err != nil {
			r.logger.Error("failed to register repository on recovering node",
				"node_id", nodeID,
				"storage_id", storageID,
				"error", err)
			continue
		}

		r.logger.Info("repository registered on recovering node",
			"node_id", nodeID,
			"storage_id", storageID,
			"success", regResp.Success,
			"message", regResp.Message)

		// If no source nodes, skip file syncing - the node may already have data
		// or rebalancing will handle redistributing files later
		if noSourceNodes {
			r.logger.Info("no source nodes for file sync, marking repository onboarded",
				"node_id", nodeID,
				"storage_id", storageID)
			markCtx, markCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = targetState.client.MarkRepositoryOnboarded(markCtx, &pb.MarkRepositoryOnboardedRequest{
				StorageId: storageID,
			})
			markCancel()
			continue
		}

		// STEP 2: Build HRW sharder to determine which files belong to this node
		r.mu.RLock()
		activeNodes := []sharding.Node{}
		for nid, ns := range r.nodes {
			if ns.info.Healthy && ns.status == NodeActive {
				activeNodes = append(activeNodes, sharding.Node{
					ID:      nid,
					Address: ns.info.Address,
					Weight:  ns.info.Weight,
					Healthy: true,
				})
			}
		}
		r.mu.RUnlock()

		sharder := sharding.NewHRW(activeNodes)

		// STEP 3: Collect files from source nodes and determine which belong to this node
		repoFilesRecovered := int64(0)
		filesToSync := []struct {
			sourceNodeID string
			filePath     string
		}{}

		for sourceID, sourceState := range sourceNodes {
			listCtx, listCancel := context.WithTimeout(context.Background(), 30*time.Second)
			filesResp, err := sourceState.client.GetRepositoryFiles(listCtx, &pb.GetRepositoryFilesRequest{
				StorageId: storageID,
			})
			listCancel()

			if err != nil {
				r.logger.Warn("failed to get file list from source node",
					"source_node", sourceID,
					"storage_id", storageID,
					"error", err)
				continue
			}

			// Identify files that should belong to target node according to HRW
			for _, filePath := range filesResp.Files {
				key := storageID + ":" + filePath
				targetNode := sharder.GetNode(key)

				if targetNode != nil && targetNode.ID == nodeID {
					// This file should be on the recovering node
					filesToSync = append(filesToSync, struct {
						sourceNodeID string
						filePath     string
					}{sourceID, filePath})
				}
			}
		}

		// STEP 4: Sync files if this node owns any
		if len(filesToSync) == 0 {
			r.logger.Info("node does not own any files for this repository, skipping file sync",
				"node_id", nodeID,
				"storage_id", storageID,
				"repo_name", repoInfo.repoID)
			// Mark as onboarded even though no files synced - repo is registered
			markCtx, markCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = targetState.client.MarkRepositoryOnboarded(markCtx, &pb.MarkRepositoryOnboardedRequest{
				StorageId: storageID,
			})
			markCancel()
			continue
		}

		// STEP 5: Sync the identified files in batch
		r.logger.Info("syncing files to recovering node",
			"node_id", nodeID,
			"storage_id", storageID,
			"file_count", len(filesToSync))

		// Group files by source node
		filesBySource := make(map[string][]*pb.FileInfo)
		for _, fileInfo := range filesToSync {
			if _, exists := filesBySource[fileInfo.sourceNodeID]; !exists {
				filesBySource[fileInfo.sourceNodeID] = []*pb.FileInfo{}
			}
			filesBySource[fileInfo.sourceNodeID] = append(filesBySource[fileInfo.sourceNodeID], &pb.FileInfo{
				StorageId: storageID,
				FilePath:  fileInfo.filePath,
			})
		}

		// Sync files from each source node
		for sourceNodeID, files := range filesBySource {
			syncCtx, syncCancel := context.WithTimeout(context.Background(), 30*time.Second)
			resp, err := targetState.client.SyncMetadataFromNode(syncCtx, &pb.SyncMetadataFromNodeRequest{
				SourceNodeId: sourceNodeID,
				TargetNodeId: nodeID,
				Files:        files,
			})
			syncCancel()

			if err != nil {
				r.logger.Warn("failed to sync files from source node",
					"source_node", sourceNodeID,
					"target_node", nodeID,
					"file_count", len(files),
					"error", err)
			} else {
				repoFilesRecovered += resp.FilesSynced
				totalRecovered += resp.FilesSynced
				r.logger.Info("synced files from source node",
					"source_node", sourceNodeID,
					"target_node", nodeID,
					"synced", resp.FilesSynced,
					"total", len(files))
			}
		}

		// STEP 6: Mark repository as onboarded (only after successful sync)
		markCtx, markCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = targetState.client.MarkRepositoryOnboarded(markCtx, &pb.MarkRepositoryOnboardedRequest{
			StorageId: storageID,
		})
		markCancel()

		if err != nil {
			r.logger.Warn("failed to mark repository onboarded after recovery",
				"node_id", nodeID,
				"storage_id", storageID,
				"error", err)
		} else {
			// Mark repository for directory index rebuild (deferred, after recovery completes)
			r.markForIndexRebuild(nodeID, storageID)

			r.logger.Info("repository recovery complete",
				"node_id", nodeID,
				"storage_id", storageID,
				"files_recovered", repoFilesRecovered)
		}
	}

	// Mark node as active again
	r.mu.Lock()
	if state := r.nodes[nodeID]; state != nil {
		state.status = NodeActive
		state.syncProgress = 1.0
		state.onboardRequested = false
		r.version.Add(1) // CRITICAL: Increment version to trigger topology change
	}
	r.mu.Unlock()

	r.logger.Info("node recovery complete",
		"node_id", nodeID,
		"repositories_recovered", len(allReposToRecover),
		"total_files_recovered", totalRecovered)

	// CRITICAL: Trigger rebalancing for ALL repositories after topology change
	// This ensures consistent HRW sharding across all repositories
	r.logger.Info("triggering rebalancing for all repositories after topology change",
		"node_id", nodeID,
		"new_topology_version", r.version.Load())

	r.mu.RLock()
	allRepos := make([]string, 0, len(r.ingestedRepos))
	for storageID := range r.ingestedRepos {
		allRepos = append(allRepos, storageID)
	}
	r.mu.RUnlock()

	// Trigger rebalancing for each repository
	for _, storageID := range allRepos {
		go r.rebalanceRepository(storageID)
	}

	// Trigger directory index rebuilds for repositories that were recovered
	// This is done asynchronously after rebalancing to ensure correctness
	for _, storageID := range allReposToRecover {
		if err := r.triggerIndexRebuild(nodeID, storageID); err != nil {
			r.logger.Warn("failed to trigger index rebuild after recovery",
				"node_id", nodeID,
				"storage_id", storageID,
				"error", err)
		}
	}
}

// onboardNewNode brings a new node into active rotation and rebalances data.
func (r *Router) onboardNewNode(nodeID string) {
	r.logger.Info("starting node onboarding", "node_id", nodeID)

	r.mu.Lock()
	state := r.nodes[nodeID]
	if state == nil {
		r.mu.Unlock()
		return
	}
	state.status = NodeSyncing
	newNode := state
	r.mu.Unlock()

	// Build list of all active nodes (including the new one for HRW calculation)
	r.mu.RLock()
	activeNodes := []sharding.Node{}
	for nid, ns := range r.nodes {
		if ns.info.Healthy {
			activeNodes = append(activeNodes, sharding.Node{
				ID:      nid,
				Address: ns.info.Address,
				Weight:  ns.info.Weight,
				Healthy: ns.info.Healthy,
			})
		}
	}
	r.mu.RUnlock()

	// Create HRW sharder with new topology
	sharder := sharding.NewHRW(activeNodes)

	totalSynced := int64(0)
	totalChecked := int64(0)

	// Discover repositories using majority consensus from all nodes
	repoVotes := make(map[string]int) // repoID -> count of nodes that have it

	r.mu.RLock()
	existingNodes := make(map[string]*nodeState)
	for nid, ns := range r.nodes {
		if nid != nodeID && ns.info.Healthy && ns.status == NodeActive {
			existingNodes[nid] = ns
		}
	}
	totalNodes := len(existingNodes)
	r.mu.RUnlock()

	// Query all existing nodes for their repository lists
	r.logger.Info("discovering repositories via consensus",
		"node_count", totalNodes,
		"new_node", nodeID)

	for sourceNodeID, sourceState := range existingNodes {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		reposResp, err := sourceState.client.ListRepositories(ctx, &pb.ListRepositoriesRequest{})
		cancel()

		if err != nil {
			r.logger.Warn("failed to list repositories from node",
				"node", sourceNodeID,
				"error", err)
			continue
		}

		for _, repoID := range reposResp.RepositoryIds {
			repoVotes[repoID]++
		}
	}

	// Calculate majority threshold (more than 50% of nodes)
	majorityThreshold := (totalNodes / 2) + 1

	// Select repositories that have majority consensus
	repoIDsSet := make(map[string]bool)
	for repoID, votes := range repoVotes {
		if votes >= majorityThreshold {
			repoIDsSet[repoID] = true
			r.logger.Info("repository has majority consensus",
				"repo", repoID,
				"votes", votes,
				"total_nodes", totalNodes,
				"threshold", majorityThreshold)
		} else {
			r.logger.Warn("repository does not have majority consensus, skipping",
				"repo", repoID,
				"votes", votes,
				"total_nodes", totalNodes,
				"threshold", majorityThreshold)
		}
	}

	r.logger.Info("discovered repositories via majority consensus",
		"repo_count", len(repoIDsSet),
		"total_candidates", len(repoVotes))

	// For each repository, redistribute files
	for repoID := range repoIDsSet {
		r.logger.Info("rebalancing repository",
			"repo", repoID,
			"new_node", nodeID)

		// STEP 1: Get repository metadata from router's tracking or query a node
		var repoURL, displayPath string

		r.mu.RLock()
		if repoInfo, ok := r.ingestedRepos[repoID]; ok {
			repoURL = repoInfo.repoURL
			displayPath = repoInfo.repoID // displayPath stored in repoID field
		}
		r.mu.RUnlock()

		// If not in router memory, query nodes for repository info
		if repoURL == "" {
			for _, sourceState := range existingNodes {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				repoInfo, err := sourceState.client.GetRepositoryInfo(ctx, &pb.GetRepositoryInfoRequest{
					StorageId: repoID,
				})
				cancel()

				if err == nil {
					repoURL = repoInfo.Source
					displayPath = repoInfo.DisplayPath
					r.logger.Info("retrieved repository metadata from node",
						"repo", repoID,
						"display_path", displayPath,
						"repo_url", repoURL)
					break
				}
			}
		}

		if repoURL == "" {
			r.logger.Warn("could not find repository metadata, skipping",
				"repo", repoID)
			continue
		}

		// STEP 2: Register repository on new node before syncing files
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := newNode.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
			StorageId:   repoID,
			DisplayPath: displayPath,
			Source:      repoURL,
		})
		cancel()

		if err != nil {
			r.logger.Error("failed to register repository on new node",
				"node", nodeID,
				"repo", repoID,
				"error", err)
			continue
		}

		r.logger.Info("registered repository on new node",
			"node", nodeID,
			"repo", repoID,
			"display_path", displayPath)

		// STEP 3: Get files from all existing nodes
		r.mu.RLock()
		existingNodes := make(map[string]*nodeState)
		for nid, ns := range r.nodes {
			if nid != nodeID && ns.info.Healthy && ns.status == NodeActive {
				existingNodes[nid] = ns
			}
		}
		r.mu.RUnlock()

		// Collect all files from existing nodes
		allFiles := make(map[string]string) // filePath -> sourceNodeID
		for sourceNodeID, sourceState := range existingNodes {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			filesResp, err := sourceState.client.GetRepositoryFiles(ctx, &pb.GetRepositoryFilesRequest{
				StorageId: repoID,
			})
			cancel()

			if err != nil {
				r.logger.Warn("failed to get file list from node",
					"node", sourceNodeID,
					"repo", repoID,
					"error", err)
				continue
			}

			for _, filePath := range filesResp.Files {
				allFiles[filePath] = sourceNodeID
			}
		}

		r.logger.Info("found files for rebalancing",
			"repo", repoID,
			"file_count", len(allFiles))

		// Determine which files should belong to the new node
		// Group files by source node for batch syncing
		filesToSync := make(map[string][]*pb.FileInfo) // sourceNodeID -> files

		for filePath, sourceNodeID := range allFiles {
			totalChecked++

			// Compute HRW hash for this file
			key := repoID + ":" + filePath
			targetNode := sharder.GetNode(key)

			if targetNode != nil && targetNode.ID == nodeID {
				// This file should belong to the new node
				if _, exists := filesToSync[sourceNodeID]; !exists {
					filesToSync[sourceNodeID] = []*pb.FileInfo{}
				}
				filesToSync[sourceNodeID] = append(filesToSync[sourceNodeID], &pb.FileInfo{
					StorageId: repoID,
					FilePath:  filePath,
				})
			}
		}

		// Batch sync files from each source node
		for sourceNodeID, files := range filesToSync {
			r.mu.RLock()
			sourceState := r.nodes[sourceNodeID]
			r.mu.RUnlock()

			if sourceState == nil {
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			resp, err := newNode.client.SyncMetadataFromNode(ctx, &pb.SyncMetadataFromNodeRequest{
				SourceNodeId: sourceNodeID,
				TargetNodeId: nodeID,
				Files:        files,
			})
			cancel()

			if err != nil {
				r.logger.Warn("failed to sync files from source node",
					"source_node", sourceNodeID,
					"file_count", len(files),
					"error", err)
			} else {
				totalSynced += resp.FilesSynced
				r.logger.Info("synced files from source node",
					"source_node", sourceNodeID,
					"synced", resp.FilesSynced,
					"total", len(files))

				// Update progress
				r.mu.Lock()
				if totalChecked > 0 {
					state.syncProgress = float64(totalSynced) / float64(totalChecked)
				}
				r.mu.Unlock()
			}
		}
	}

	r.mu.Lock()
	state.status = NodeActive
	state.syncProgress = 1.0
	r.version.Add(1) // NOW increment version - node is active
	r.mu.Unlock()

	r.logger.Info("node onboarding complete",
		"node_id", nodeID,
		"files_synced", totalSynced,
		"files_checked", totalChecked)

	// CRITICAL: Trigger rebalancing for ALL repositories after new node is active
	// This ensures consistent HRW sharding across all repositories with the new topology
	r.logger.Info("triggering rebalancing for all repositories after node onboarding",
		"node_id", nodeID,
		"new_topology_version", r.version.Load())

	r.mu.RLock()
	allRepos := make([]string, 0, len(r.ingestedRepos))
	for storageID := range r.ingestedRepos {
		allRepos = append(allRepos, storageID)
	}
	r.mu.RUnlock()

	// Trigger rebalancing for each repository
	for _, storageID := range allRepos {
		go r.rebalanceRepository(storageID)
	}
}

// triggerRebalanceOnRecovery handles rebalancing when a previously unhealthy node recovers.
// This ensures that files are redistributed correctly when nodes come back online.
func (r *Router) triggerRebalanceOnRecovery(nodeID string) {
	r.logger.Info("triggering rebalancing after node recovery",
		"node_id", nodeID,
		"topology_version", r.version.Load())

	// Wait briefly for other nodes to potentially recover as well
	// This prevents a flurry of rebalancing operations when multiple nodes recover simultaneously
	time.Sleep(2 * time.Second)

	// First check if the node still needs onboarding (may have missed repo ingestion)
	r.mu.RLock()
	state := r.nodes[nodeID]
	if state == nil || !state.info.Healthy {
		r.mu.RUnlock()
		r.logger.Debug("node no longer healthy, skipping recovery rebalance", "node_id", nodeID)
		return
	}

	// Check if there are any ingested repos to rebalance
	if len(r.ingestedRepos) == 0 {
		r.mu.RUnlock()
		r.logger.Debug("no repositories to rebalance", "node_id", nodeID)
		return
	}

	allRepos := make([]string, 0, len(r.ingestedRepos))
	for storageID := range r.ingestedRepos {
		allRepos = append(allRepos, storageID)
	}
	r.mu.RUnlock()

	// Check onboarding status first - the node might need to sync repos it missed
	if state.client != nil && !state.onboardRequested {
		r.checkAndRecoverNode(nodeID, state)
	}

	// Then trigger rebalancing for all repositories
	r.logger.Info("rebalancing all repositories after node recovery",
		"node_id", nodeID,
		"repo_count", len(allRepos),
		"topology_version", r.version.Load())

	for _, storageID := range allRepos {
		go r.rebalanceRepository(storageID)
	}
}

// rebalanceRepository redistributes files for a specific repository across all active nodes.
// Uses atomic rebalancing with dual-state period to ensure zero downtime.
func (r *Router) rebalanceRepository(storageID string) {
	r.logger.Info("starting atomic repository rebalancing", "storage_id", storageID)

	r.mu.RLock()
	repo, exists := r.ingestedRepos[storageID]
	r.mu.RUnlock()

	if !exists {
		r.logger.Warn("repository not found for rebalancing", "storage_id", storageID)
		return
	}

	// Check if already rebalancing
	repo.mu.Lock()
	if repo.rebalanceState != RebalanceStateStable {
		r.logger.Info("rebalancing already in progress for repository",
			"storage_id", storageID,
			"state", repo.rebalanceState.String())
		repo.mu.Unlock()
		return
	}

	// Mark as rebalancing
	currentTopology := repo.topologyVersion
	targetTopology := r.version.Load()

	// Check if rebalancing is needed
	if currentTopology == targetTopology {
		r.logger.Debug("repository already using current topology",
			"storage_id", storageID,
			"topology_version", currentTopology)
		repo.mu.Unlock()
		return
	}

	repo.rebalanceState = RebalanceStateRebalancing
	repo.targetTopology = targetTopology
	repo.rebalanceProgress = 0.0
	repo.mu.Unlock()

	r.logger.Info("rebalancing repository",
		"storage_id", storageID,
		"from_topology", currentTopology,
		"to_topology", targetTopology)

	// Build list of all active healthy nodes
	r.mu.RLock()
	activeNodes := make([]sharding.Node, 0, len(r.nodes))
	nodeStates := make(map[string]*nodeState)
	for nodeID, state := range r.nodes {
		if state.info.Healthy && state.status == NodeActive {
			activeNodes = append(activeNodes, sharding.Node{
				ID:      state.info.NodeId,
				Address: state.info.Address,
				Weight:  state.info.Weight,
				Healthy: true,
			})
			nodeStates[nodeID] = state
		}
	}
	r.mu.RUnlock()

	// Capture topology snapshots for old and new versions
	// This allows getNodeForTopologyVersion to route correctly during rebalancing
	r.topologySnapshotsMu.Lock()
	if _, exists := r.topologySnapshots[currentTopology]; !exists {
		// Capture old topology snapshot (if not already captured)
		r.topologySnapshots[currentTopology] = make([]sharding.Node, len(activeNodes))
		copy(r.topologySnapshots[currentTopology], activeNodes)
	}
	if _, exists := r.topologySnapshots[targetTopology]; !exists {
		// Capture new topology snapshot
		r.topologySnapshots[targetTopology] = make([]sharding.Node, len(activeNodes))
		copy(r.topologySnapshots[targetTopology], activeNodes)
	}
	r.topologySnapshotsMu.Unlock()

	if len(activeNodes) < 1 {
		r.logger.Warn("not enough nodes for rebalancing", "active_nodes", len(activeNodes))
		repo.mu.Lock()
		repo.rebalanceState = RebalanceStateStable
		repo.mu.Unlock()
		return
	}

	// Create HRW sharder with current topology
	sharder := sharding.NewHRW(activeNodes)

	// PHASE 1: Collect all files from all nodes for this repository
	allFiles := make(map[string]string) // filePath -> currentNodeID

	for nodeID, state := range nodeStates {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		filesResp, err := state.client.GetRepositoryFiles(ctx, &pb.GetRepositoryFilesRequest{
			StorageId: storageID,
		})
		cancel()

		if err != nil {
			r.logger.Warn("failed to get file list during rebalancing",
				"node", nodeID,
				"storage_id", storageID,
				"error", err)
			continue
		}

		for _, filePath := range filesResp.Files {
			allFiles[filePath] = nodeID
		}
	}

	r.logger.Info("collected files for rebalancing",
		"storage_id", storageID,
		"file_count", len(allFiles))

	// PHASE 2: Copy files to new locations (DON'T delete from old)
	filesMoved := int64(0)
	filesChecked := int64(0)
	filesToMove := make(map[string]struct {
		from string
		to   string
	})

	for filePath, currentNodeID := range allFiles {
		filesChecked++

		// Calculate where this file should be
		key := storageID + ":" + filePath
		targetNode := sharder.GetNode(key)
		if targetNode == nil {
			r.logger.Warn("no target node for file", "file", filePath)
			continue
		}

		// If file needs to move, record it
		if targetNode.ID != currentNodeID {
			filesToMove[filePath] = struct {
				from string
				to   string
			}{currentNodeID, targetNode.ID}
		}

		// Update progress
		if filesChecked%100 == 0 {
			repo.mu.Lock()
			repo.rebalanceProgress = float64(filesChecked) / float64(len(allFiles)) * 0.5 // First 50%
			repo.mu.Unlock()
		}
	}

	r.logger.Info("identified files to move",
		"storage_id", storageID,
		"files_to_move", len(filesToMove),
		"files_checked", filesChecked)

	// Group files by (source, target) pair for batch syncing
	filesByRoute := make(map[string]map[string][]*pb.FileInfo) // targetNodeID -> sourceNodeID -> files
	for filePath, moveInfo := range filesToMove {
		if filesByRoute[moveInfo.to] == nil {
			filesByRoute[moveInfo.to] = make(map[string][]*pb.FileInfo)
		}
		if filesByRoute[moveInfo.to][moveInfo.from] == nil {
			filesByRoute[moveInfo.to][moveInfo.from] = []*pb.FileInfo{}
		}
		filesByRoute[moveInfo.to][moveInfo.from] = append(filesByRoute[moveInfo.to][moveInfo.from], &pb.FileInfo{
			StorageId: storageID,
			FilePath:  filePath,
		})
	}

	// Copy files to new locations in batches
	for targetNodeID, sourceMap := range filesByRoute {
		targetState := nodeStates[targetNodeID]
		if targetState == nil {
			r.logger.Warn("target node not found", "target_node", targetNodeID)
			continue
		}

		for sourceNodeID, files := range sourceMap {
			// Sync batch of files from source to target
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			resp, err := targetState.client.SyncMetadataFromNode(ctx, &pb.SyncMetadataFromNodeRequest{
				SourceNodeId: sourceNodeID,
				TargetNodeId: targetNodeID,
				Files:        files,
			})
			cancel()

			if err != nil {
				r.logger.Warn("failed to copy files during rebalancing",
					"from", sourceNodeID,
					"to", targetNodeID,
					"file_count", len(files),
					"error", err)
			} else {
				filesMoved += resp.FilesSynced
				r.logger.Info("copied files during rebalancing",
					"from", sourceNodeID,
					"to", targetNodeID,
					"synced", resp.FilesSynced,
					"total", len(files))

				// Update progress
				repo.mu.Lock()
				repo.rebalanceProgress = 0.5 + (float64(filesMoved)/float64(len(filesToMove)))*0.4 // 50-90%
				repo.mu.Unlock()
			}
		}
	}

	// Mark affected nodes for directory index rebuild
	// Track which nodes received new files during rebalancing
	affectedNodes := make(map[string]bool)
	for _, moveInfo := range filesToMove {
		affectedNodes[moveInfo.to] = true
	}
	for nodeID := range affectedNodes {
		r.markForIndexRebuild(nodeID, storageID)
	}

	// PHASE 3: Dual-active period (both old and new locations valid)
	r.logger.Info("entering dual-active period",
		"storage_id", storageID,
		"files_moved", filesMoved)

	repo.mu.Lock()
	repo.rebalanceState = RebalanceStateDualActive
	repo.rebalanceProgress = 0.95
	repo.mu.Unlock()

	// Grace period for clients to refresh routing
	time.Sleep(30 * time.Second)

	// PHASE 4: Atomic switchover
	repo.mu.Lock()
	repo.topologyVersion = targetTopology
	repo.rebalanceState = RebalanceStateStable
	repo.rebalanceProgress = 1.0
	repo.mu.Unlock()

	r.logger.Info("rebalancing complete, topology switched",
		"storage_id", storageID,
		"new_topology", targetTopology,
		"files_moved", filesMoved)

	// PHASE 5: Trigger directory index rebuild (async, non-blocking)
	r.pendingIndexRebuildsMu.Lock()
	pendingRebuilds := make(map[string]map[string]bool)
	for nodeID, storageIDs := range r.pendingIndexRebuilds {
		pendingRebuilds[nodeID] = make(map[string]bool)
		for sid := range storageIDs {
			pendingRebuilds[nodeID][sid] = true
		}
	}
	r.pendingIndexRebuildsMu.Unlock()

	// Trigger rebuilds asynchronously
	for nodeID, storageIDs := range pendingRebuilds {
		for sid := range storageIDs {
			if sid != storageID {
				continue // Only rebuild this repository
			}
			go func(nid, sid string) {
				if err := r.triggerIndexRebuild(nid, sid); err != nil {
					r.logger.Warn("index rebuild failed",
						"node_id", nid,
						"storage_id", sid,
						"error", err)
				} else {
					// Remove from pending on success
					r.pendingIndexRebuildsMu.Lock()
					if r.pendingIndexRebuilds[nid] != nil {
						delete(r.pendingIndexRebuilds[nid], sid)
						if len(r.pendingIndexRebuilds[nid]) == 0 {
							delete(r.pendingIndexRebuilds, nid)
						}
					}
					r.pendingIndexRebuildsMu.Unlock()
				}
			}(nodeID, sid)
		}
	}

	// PHASE 6: Cleanup old locations (async, best effort)
	go r.cleanupOldFileLocations(storageID, filesToMove)
}

// cleanupOldFileLocations removes files from old locations after rebalancing.
// This is called ONLY after rebalancing (not during recovery) to avoid data loss.
// Safety measures:
// 1. 5-minute grace period ensures all clients have refreshed routing cache
// 2. Deletion is best-effort (failures are logged but don't fail rebalancing)
// 3. Only deletes files that were successfully copied to new locations
func (r *Router) cleanupOldFileLocations(storageID string, filesToMove map[string]struct {
	from string
	to   string
}) {
	// Wait for dual-active period + grace period
	// This ensures all clients have refreshed their routing cache
	gracePeriod := 5 * time.Minute
	time.Sleep(gracePeriod)

	// Build summary of which nodes will have files deleted
	nodeCleanupCount := make(map[string]int)
	for _, moveInfo := range filesToMove {
		nodeCleanupCount[moveInfo.from]++
	}

	// Format node summary for logging
	nodeSummary := make([]string, 0, len(nodeCleanupCount))
	for nodeID, count := range nodeCleanupCount {
		nodeSummary = append(nodeSummary, fmt.Sprintf("%s:%d", nodeID, count))
	}

	r.logger.Info("starting cleanup of old file locations after rebalancing",
		"storage_id", storageID,
		"files_to_cleanup", len(filesToMove),
		"grace_period", gracePeriod,
		"nodes_to_cleanup", nodeSummary)

	// Get current node states
	r.mu.RLock()
	nodeStates := make(map[string]*nodeState)
	for nodeID, state := range r.nodes {
		nodeStates[nodeID] = state
	}
	r.mu.RUnlock()

	deletedCount := 0
	failedCount := 0
	skippedCount := 0
	deletedPerNode := make(map[string]int)

	for filePath, moveInfo := range filesToMove {
		sourceState := nodeStates[moveInfo.from]
		if sourceState == nil || sourceState.client == nil {
			r.logger.Warn("source node not available for cleanup",
				"file", filePath,
				"source_node", moveInfo.from,
				"storage_id", storageID)
			skippedCount++
			continue
		}

		// Delete old copy from source node
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := sourceState.client.DeleteFile(ctx, &pb.DeleteFileRequest{
			StorageId: storageID,
			FilePath:  filePath,
		})
		cancel()

		if err != nil {
			r.logger.Warn("failed to delete old file copy",
				"file", filePath,
				"source_node", moveInfo.from,
				"target_node", moveInfo.to,
				"storage_id", storageID,
				"error", err)
			failedCount++
		} else if !resp.Success {
			r.logger.Warn("failed to delete old file copy (server error)",
				"file", filePath,
				"source_node", moveInfo.from,
				"target_node", moveInfo.to,
				"storage_id", storageID,
				"message", resp.Message)
			failedCount++
		} else {
			deletedCount++
			deletedPerNode[moveInfo.from]++

			// Log progress every 100 files
			if deletedCount%100 == 0 {
				r.logger.Info("cleanup progress",
					"storage_id", storageID,
					"deleted", deletedCount,
					"failed", failedCount,
					"skipped", skippedCount,
					"remaining", len(filesToMove)-deletedCount-failedCount-skippedCount,
					"deleted_per_node", deletedPerNode)
			}
		}
	}

	r.logger.Info("rebalancing cleanup complete",
		"storage_id", storageID,
		"files_deleted", deletedCount,
		"files_failed", failedCount,
		"files_skipped", skippedCount,
		"total_files", len(filesToMove),
		"deleted_per_node", deletedPerNode)

	// Log warning if many files failed to delete
	if failedCount > 0 && float64(failedCount)/float64(len(filesToMove)) > 0.1 {
		r.logger.Warn("significant number of files failed to delete during cleanup",
			"storage_id", storageID,
			"failed_percentage", float64(failedCount)/float64(len(filesToMove))*100)
	}
}

// ============================================================================
// Client Registration and Lifecycle Management
// ============================================================================

// RegisterClient registers a new FUSE client with the router
func (r *Router) RegisterClient(ctx context.Context, req *pb.RegisterClientRequest) (*pb.RegisterClientResponse, error) {
	if req.ClientId == "" {
		return &pb.RegisterClientResponse{
			Success: false,
			Message: "client_id is required",
		}, nil
	}

	r.clientsMu.Lock()
	defer r.clientsMu.Unlock()

	// Check if client already exists (reconnection)
	if existing, ok := r.clients[req.ClientId]; ok {
		existing.mu.Lock()
		existing.info.MountPoint = req.MountPoint
		existing.info.Hostname = req.Hostname
		existing.info.Writable = req.Writable
		existing.info.Version = req.Version
		existing.info.State = pb.ClientState_CLIENT_CONNECTED
		existing.lastHeartbeat = time.Now()
		existing.mu.Unlock()

		r.logger.Info("client reconnected",
			"client_id", req.ClientId,
			"hostname", req.Hostname,
			"mount_point", req.MountPoint)

		return &pb.RegisterClientResponse{
			Success:             true,
			Message:             "reconnected",
			HeartbeatIntervalMs: 30000, // 30 seconds
		}, nil
	}

	// Create new client state
	now := time.Now()
	state := &clientState{
		info: &pb.ClientInfo{
			ClientId:        req.ClientId,
			MountPoint:      req.MountPoint,
			Hostname:        req.Hostname,
			Writable:        req.Writable,
			Version:         req.Version,
			State:           pb.ClientState_CLIENT_CONNECTED,
			ConnectedAt:     now.Unix(),
			LastHeartbeat:   now.Unix(),
			OperationsCount: 0,
			BytesRead:       0,
		},
		lastHeartbeat:   now,
		operationsCount: 0,
		bytesRead:       0,
	}

	r.clients[req.ClientId] = state

	r.logger.Info("client registered",
		"client_id", req.ClientId,
		"hostname", req.Hostname,
		"mount_point", req.MountPoint,
		"writable", req.Writable,
		"version", req.Version,
		"total_clients", len(r.clients))

	return &pb.RegisterClientResponse{
		Success:             true,
		Message:             "registered",
		HeartbeatIntervalMs: 30000, // 30 seconds
	}, nil
}

// UnregisterClient removes a FUSE client from the router
func (r *Router) UnregisterClient(ctx context.Context, req *pb.UnregisterClientRequest) (*pb.UnregisterClientResponse, error) {
	if req.ClientId == "" {
		return &pb.UnregisterClientResponse{
			Success: false,
			Message: "client_id is required",
		}, nil
	}

	r.clientsMu.Lock()
	defer r.clientsMu.Unlock()

	state, ok := r.clients[req.ClientId]
	if !ok {
		return &pb.UnregisterClientResponse{
			Success: true,
			Message: "client not found (already unregistered)",
		}, nil
	}

	// Log final stats
	state.mu.RLock()
	opsCount := state.operationsCount
	bytesRead := state.bytesRead
	connectedAt := time.Unix(state.info.ConnectedAt, 0)
	state.mu.RUnlock()

	duration := time.Since(connectedAt)

	delete(r.clients, req.ClientId)

	r.logger.Info("client unregistered",
		"client_id", req.ClientId,
		"reason", req.Reason,
		"session_duration", duration.String(),
		"total_operations", opsCount,
		"total_bytes_read", bytesRead,
		"remaining_clients", len(r.clients))

	return &pb.UnregisterClientResponse{
		Success: true,
		Message: "unregistered",
	}, nil
}

// ClientHeartbeat updates client state and metrics
func (r *Router) ClientHeartbeat(ctx context.Context, req *pb.ClientHeartbeatRequest) (*pb.ClientHeartbeatResponse, error) {
	if req.ClientId == "" {
		return &pb.ClientHeartbeatResponse{
			Success: false,
			Message: "client_id is required",
		}, nil
	}

	r.clientsMu.RLock()
	state, ok := r.clients[req.ClientId]
	r.clientsMu.RUnlock()

	if !ok {
		// Client not registered - tell it to re-register
		return &pb.ClientHeartbeatResponse{
			Success:        false,
			Message:        "client not registered",
			ShouldRegister: true,
		}, nil
	}

	now := time.Now()

	state.mu.Lock()
	state.lastHeartbeat = now
	state.operationsCount = req.OperationsCount
	state.bytesRead = req.BytesRead
	state.info.LastHeartbeat = now.Unix()
	state.info.OperationsCount = req.OperationsCount
	state.info.BytesRead = req.BytesRead
	state.info.State = pb.ClientState_CLIENT_CONNECTED
	state.mu.Unlock()

	return &pb.ClientHeartbeatResponse{
		Success: true,
		Message: "heartbeat received",
	}, nil
}

// ListClients returns information about all connected clients
func (r *Router) ListClients(ctx context.Context, req *pb.ListClientsRequest) (*pb.ListClientsResponse, error) {
	r.clientsMu.RLock()
	defer r.clientsMu.RUnlock()

	clients := make([]*pb.ClientInfo, 0, len(r.clients))
	now := time.Now()

	for _, state := range r.clients {
		state.mu.RLock()
		// Create a copy of the info
		info := &pb.ClientInfo{
			ClientId:        state.info.ClientId,
			MountPoint:      state.info.MountPoint,
			Hostname:        state.info.Hostname,
			Writable:        state.info.Writable,
			Version:         state.info.Version,
			State:           state.info.State,
			ConnectedAt:     state.info.ConnectedAt,
			LastHeartbeat:   state.info.LastHeartbeat,
			OperationsCount: state.operationsCount,
			BytesRead:       state.bytesRead,
		}

		// Update state based on heartbeat freshness
		lastHB := time.Unix(state.info.LastHeartbeat, 0)
		if now.Sub(lastHB) > 60*time.Second {
			info.State = pb.ClientState_CLIENT_STALE
		}

		state.mu.RUnlock()
		clients = append(clients, info)
	}

	return &pb.ListClientsResponse{
		Clients: clients,
	}, nil
}

// cleanupStaleClients periodically removes clients that haven't sent heartbeats
func (r *Router) cleanupStaleClients() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	staleThreshold := 60 * time.Second // Mark as stale after 60s
	removeThreshold := 5 * time.Minute // Remove after 5 minutes

	for {
		select {
		case <-r.stopClients:
			return
		case <-ticker.C:
			r.cleanupStaleClientsOnce(staleThreshold, removeThreshold)
		}
	}
}

func (r *Router) cleanupStaleClientsOnce(staleThreshold, removeThreshold time.Duration) {
	now := time.Now()
	var toRemove []string
	var staleCount int

	r.clientsMu.RLock()
	for clientID, state := range r.clients {
		state.mu.RLock()
		lastHB := state.lastHeartbeat
		currentState := state.info.State
		state.mu.RUnlock()

		elapsed := now.Sub(lastHB)

		if elapsed > removeThreshold {
			toRemove = append(toRemove, clientID)
		} else if elapsed > staleThreshold && currentState == pb.ClientState_CLIENT_CONNECTED {
			// Mark as stale
			state.mu.Lock()
			state.info.State = pb.ClientState_CLIENT_STALE
			state.mu.Unlock()
			staleCount++
		}
	}
	r.clientsMu.RUnlock()

	// Remove timed-out clients
	if len(toRemove) > 0 {
		r.clientsMu.Lock()
		for _, clientID := range toRemove {
			if state, ok := r.clients[clientID]; ok {
				state.mu.RLock()
				r.logger.Warn("removing stale client",
					"client_id", clientID,
					"hostname", state.info.Hostname,
					"last_heartbeat", state.lastHeartbeat.Format(time.RFC3339))
				state.mu.RUnlock()
				delete(r.clients, clientID)
			}
		}
		r.clientsMu.Unlock()
	}

	if len(toRemove) > 0 || staleCount > 0 {
		r.logger.Debug("client cleanup completed",
			"removed", len(toRemove),
			"marked_stale", staleCount)
	}
}

// GetClientCount returns the number of connected clients (for dashboard)
func (r *Router) GetClientCount() int {
	r.clientsMu.RLock()
	defer r.clientsMu.RUnlock()
	return len(r.clients)
}

// GetClientStats returns aggregated client statistics for the performance page
func (r *Router) GetClientStats() (total int, connected int, stale int, totalOps int64, totalBytes int64) {
	r.clientsMu.RLock()
	defer r.clientsMu.RUnlock()

	for _, state := range r.clients {
		state.mu.RLock()
		total++
		switch state.info.State {
		case pb.ClientState_CLIENT_CONNECTED:
			connected++
		case pb.ClientState_CLIENT_STALE:
			stale++
		}
		totalOps += state.operationsCount
		totalBytes += state.bytesRead
		state.mu.RUnlock()
	}

	return
}

// RequestFailover handles graceful node shutdown by redistributing its responsibilities
func (r *Router) RequestFailover(ctx context.Context, req *pb.FailoverRequest) (*pb.FailoverResponse, error) {
	sourceNodeID := req.SourceNodeId

	// Check if cluster is in drain mode
	if r.IsDrained() {
		r.logger.Info("ignoring failover request - cluster is in drain mode",
			"source_node", sourceNodeID)
		return &pb.FailoverResponse{
			Success: false,
			Message: "cluster is in drain mode - failover disabled",
		}, nil
	}

	r.logger.Info("received failover request", "source_node", sourceNodeID)

	r.mu.Lock()
	sourceNode, exists := r.nodes[sourceNodeID]
	if !exists {
		r.mu.Unlock()
		return &pb.FailoverResponse{
			Success: false,
			Message: fmt.Sprintf("source node %s not found", sourceNodeID),
		}, nil
	}

	// Find a healthy node to take over
	var targetNode *nodeState
	var targetNodeID string
	minLoad := int64(^uint64(0) >> 1) // max int64

	for nodeID, node := range r.nodes {
		if nodeID == sourceNodeID || !node.info.Healthy {
			continue
		}
		// Select node with minimum load (owned files)
		if node.ownedFilesCount < minLoad {
			minLoad = node.ownedFilesCount
			targetNode = node
			targetNodeID = nodeID
		}
	}

	if targetNode == nil {
		r.mu.Unlock()
		return &pb.FailoverResponse{
			Success: false,
			Message: "no healthy nodes available for failover",
		}, nil
	}

	r.logger.Info("selected failover target",
		"source_node", sourceNodeID,
		"target_node", targetNodeID,
		"target_load", minLoad)

	// Mark source node as unhealthy immediately
	sourceNode.info.Healthy = false
	sourceNode.status = NodeStaging // Remove from active pool

	// Track the failover relationship
	if targetNode.backingUpNodes == nil {
		targetNode.backingUpNodes = []string{}
	}
	targetNode.backingUpNodes = append(targetNode.backingUpNodes, sourceNodeID)

	// Store failover mapping
	r.failoverMap.Store(sourceNodeID, targetNodeID)

	// Track when failure started
	r.failoverTimersMu.Lock()
	r.failoverStartTimes[sourceNodeID] = time.Now()

	// For graceful failover (planned restart), use shorter delay
	// The node is intentionally leaving, so we expect a quick return
	delay := r.config.GracefulFailoverDelay
	if existingTimer, exists := r.failoverTimers[sourceNodeID]; exists {
		existingTimer.Stop()
	}
	r.failoverTimers[sourceNodeID] = time.AfterFunc(delay, func() {
		r.triggerDelayedRebalance(sourceNodeID)
	})
	r.failoverTimersMu.Unlock()

	// Increment cluster version to trigger client topology refresh
	r.version.Add(1)

	r.mu.Unlock()

	r.logger.Info("graceful failover initiated",
		"source_node", sourceNodeID,
		"target_node", targetNodeID,
		"rebalance_delay", delay,
		"topology_version", r.version.Load())

	// Target node already has replica data (from ingestion), so it can serve immediately
	// Mark target as active - no sync needed since replicas were populated during ingestion
	r.mu.Lock()
	if target, exists := r.nodes[targetNodeID]; exists {
		target.status = NodeActive
		target.syncProgress = 1.0
	}
	r.mu.Unlock()

	return &pb.FailoverResponse{
		Success:      true,
		TargetNodeId: targetNodeID,
		Message:      fmt.Sprintf("graceful failover to node %s, rebalance in %v if not recovered", targetNodeID, delay),
	}, nil
}

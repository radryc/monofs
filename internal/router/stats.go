package router

import (
	"context"

	pb "github.com/radryc/monofs/api/proto"
)

// GetClusterStats returns cluster-wide statistics
func (r *Router) GetClusterStats(ctx context.Context, req *pb.ClusterStatsRequest) (*pb.ClusterStatsResponse, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Count nodes by health status
	totalNodes := int32(len(r.nodes))
	var healthyNodes, unhealthyNodes, syncingNodes int32

	for _, node := range r.nodes {
		// Consider node healthy if we've seen it recently
		isHealthy := node.info != nil && !node.lastSeen.IsZero()
		if isHealthy {
			healthyNodes++
		} else {
			unhealthyNodes++
		}
		if node.status == NodeSyncing {
			syncingNodes++
		}
	}

	// Count repositories
	totalRepos := int64(len(r.ingestedRepos))
	var totalFiles, totalSize int64

	for _, repo := range r.ingestedRepos {
		// Aggregate file counts and sizes from nodes
		if repo.filesCount > 0 {
			totalFiles += repo.filesCount
		}
		// Note: totalSize would need to be tracked separately in ingestedRepo
	}

	// Get failover mappings
	failovers := make(map[string]string)
	r.failoverMap.Range(func(key, value interface{}) bool {
		sourceID := key.(string)
		targetID := value.(string)
		failovers[sourceID] = targetID
		return true
	})

	clusterVersion := r.version.Load()

	return &pb.ClusterStatsResponse{
		TotalNodes:        totalNodes,
		HealthyNodes:      healthyNodes,
		UnhealthyNodes:    unhealthyNodes,
		SyncingNodes:      syncingNodes,
		TotalRepositories: totalRepos,
		TotalFiles:        totalFiles,
		TotalSizeBytes:    totalSize,
		ClusterVersion:    clusterVersion,
		Failovers:         failovers,
	}, nil
}

// GetNodeStats returns per-node statistics
func (r *Router) GetNodeStats(ctx context.Context, req *pb.NodeStatsRequest) (*pb.NodeStatsResponse, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var nodeStats []*pb.NodeStatInfo

	for nodeID, node := range r.nodes {
		// Get backing up nodes
		backingUpNodes := make([]string, len(node.backingUpNodes))
		copy(backingUpNodes, node.backingUpNodes)

		// Calculate last heartbeat timestamp
		var lastHeartbeat int64
		if !node.lastSeen.IsZero() {
			lastHeartbeat = node.lastSeen.Unix()
		}

		// Get status string
		statusStr := "Unknown"
		switch node.status {
		case NodeActive:
			statusStr = "Active"
		case NodeSyncing:
			statusStr = "Syncing"
		case NodeStaging:
			statusStr = "Staging"
		}

		// Get node address
		address := ""
		if node.info != nil {
			address = node.info.Address
		}

		// Check if node is healthy (has recent lastSeen)
		isHealthy := node.info != nil && !node.lastSeen.IsZero()

		// Get file count from node's owned files
		fileCount := node.ownedFilesCount

		nodeStats = append(nodeStats, &pb.NodeStatInfo{
			NodeId:          nodeID,
			Address:         address,
			Status:          statusStr,
			Healthy:         isHealthy,
			FileCount:       fileCount,
			UsedSpaceBytes:  node.diskUsedBytes,
			TotalSpaceBytes: node.diskTotalBytes,
			FreeSpaceBytes:  node.diskFreeBytes,
			BackingUpNodes:  backingUpNodes,
			SyncProgress:    node.syncProgress,
			LastHeartbeat:   lastHeartbeat,
		})
	}

	return &pb.NodeStatsResponse{
		Nodes: nodeStats,
	}, nil
}

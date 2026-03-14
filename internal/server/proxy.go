// Package server implements the MonoFS gRPC server with NutsDB storage.
package server

import (
	"context"
	"fmt"
	"io"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/sharding"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// EnableForwarding enables server-side request forwarding via HRW routing.
// The server will connect to the router, fetch cluster topology, and forward
// requests to the correct owner node when needed.
//
// This makes any server act as a "smart proxy" that can handle any request
// by routing it to the appropriate backend node.
func (s *Server) EnableForwarding(routerAddr string, refreshInterval time.Duration) error {
	if routerAddr == "" {
		return fmt.Errorf("router address is required for forwarding")
	}

	s.hrwMu.Lock()
	defer s.hrwMu.Unlock()

	s.routerAddr = routerAddr
	s.enableForwarding = true
	s.refreshInterval = refreshInterval
	if s.refreshInterval == 0 {
		s.refreshInterval = 30 * time.Second // Default refresh interval
	}
	s.rpcTimeout = 5 * time.Second // Timeout for forwarded RPCs
	s.stopRefresh = make(chan struct{})
	s.peerConns = make(map[string]*grpc.ClientConn)
	s.peerClients = make(map[string]pb.MonoFSClient)

	// Initial topology refresh
	if err := s.refreshTopology(); err != nil {
		s.logger.Warn("initial topology refresh failed, will retry", "error", err)
		// Don't return error - we'll retry in the background
	}

	// Start background topology refresh
	go s.topologyRefreshLoop()

	s.logger.Info("server-side forwarding enabled",
		"router", routerAddr,
		"refresh_interval", s.refreshInterval)

	return nil
}

// DisableForwarding stops the forwarding functionality and closes connections.
func (s *Server) DisableForwarding() {
	s.hrwMu.Lock()
	defer s.hrwMu.Unlock()

	if !s.enableForwarding {
		return
	}

	s.enableForwarding = false
	close(s.stopRefresh)

	// Close all peer connections
	for nodeID, conn := range s.peerConns {
		if err := conn.Close(); err != nil {
			s.logger.Debug("error closing peer connection", "node_id", nodeID, "error", err)
		}
	}
	s.peerConns = nil
	s.peerClients = nil

	// Close router connection
	if s.routerConn != nil {
		s.routerConn.Close()
		s.routerConn = nil
		s.routerClient = nil
	}

	s.logger.Info("server-side forwarding disabled")
}

// topologyRefreshLoop periodically refreshes cluster topology from router.
func (s *Server) topologyRefreshLoop() {
	ticker := time.NewTicker(s.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := s.refreshTopology(); err != nil {
				s.logger.Warn("topology refresh failed", "error", err)
			}
		case <-s.stopRefresh:
			return
		}
	}
}

// refreshTopology fetches cluster info from router and updates HRW hasher.
func (s *Server) refreshTopology() error {
	s.hrwMu.Lock()
	defer s.hrwMu.Unlock()

	// Connect to router if not connected
	if s.routerConn == nil {
		conn, err := grpc.NewClient(s.routerAddr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(100*1024*1024)),
		)
		if err != nil {
			return fmt.Errorf("failed to connect to router: %w", err)
		}
		s.routerConn = conn
		s.routerClient = pb.NewMonoFSRouterClient(conn)
		s.logger.Info("connected to router", "address", s.routerAddr)
	}

	// Fetch cluster info
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := s.routerClient.GetClusterInfo(ctx, &pb.ClusterInfoRequest{})
	if err != nil {
		return fmt.Errorf("failed to get cluster info: %w", err)
	}

	// Check if topology changed
	if resp.Version == s.clusterVersion.Load() {
		return nil // No change
	}

	// Update HRW hasher
	if s.hrw == nil {
		s.hrw = sharding.NewHRWFromProto(resp.Nodes)
	} else {
		s.hrw.UpdateNodeHealthFromProto(resp.Nodes)
	}

	// Update peer connections
	currentNodes := make(map[string]bool)
	for _, node := range resp.Nodes {
		currentNodes[node.GetNodeId()] = true

		// Skip self
		if node.GetNodeId() == s.nodeID {
			continue
		}

		// Check if we already have a connection to this node
		if _, exists := s.peerConns[node.GetNodeId()]; exists {
			continue
		}

		// Create new connection to peer
		nodeAddr := node.Address
		if node.GetAddress() != "" {
			nodeAddr = node.GetAddress()
		}

		conn, err := grpc.NewClient(nodeAddr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			s.logger.Warn("failed to connect to peer node",
				"node_id", node.GetNodeId(),
				"address", nodeAddr,
				"error", err)
			continue
		}

		s.peerConns[node.GetNodeId()] = conn
		s.peerClients[node.GetNodeId()] = pb.NewMonoFSClient(conn)
		s.logger.Info("connected to peer node", "node_id", node.GetNodeId(), "address", nodeAddr)
	}

	// Remove connections to nodes that are no longer in cluster
	for nodeID, conn := range s.peerConns {
		if !currentNodes[nodeID] {
			conn.Close()
			delete(s.peerConns, nodeID)
			delete(s.peerClients, nodeID)
			s.logger.Info("removed peer connection", "node_id", nodeID)
		}
	}

	s.clusterVersion.Store(resp.Version)
	s.logger.Info("topology updated",
		"version", resp.Version,
		"nodes", len(resp.Nodes),
		"peers", len(s.peerClients))

	return nil
}

// shouldHandleLocally checks if this node should handle the request based on HRW.
// Returns true if local, false if should forward to another node.
func (s *Server) shouldHandleLocally(storageID, filePath string) bool {
	s.hrwMu.RLock()
	defer s.hrwMu.RUnlock()

	if !s.enableForwarding || s.hrw == nil {
		// Forwarding disabled or no topology - handle locally
		return true
	}

	// Build shard key (same format as client)
	shardKey := storageID
	if filePath != "" {
		shardKey = storageID + ":" + filePath
	}

	// Get the node that should own this key
	targetNode := s.hrw.GetNode(shardKey)
	if targetNode == nil {
		// No healthy nodes - handle locally as fallback
		return true
	}

	return targetNode.ID == s.nodeID
}

// getTargetNode returns the node that should handle the given key.
func (s *Server) getTargetNode(storageID, filePath string) *sharding.Node {
	s.hrwMu.RLock()
	defer s.hrwMu.RUnlock()

	if !s.enableForwarding || s.hrw == nil {
		return nil
	}

	shardKey := storageID
	if filePath != "" {
		shardKey = storageID + ":" + filePath
	}

	return s.hrw.GetNode(shardKey)
}

// isNodeHealthy checks if a node is currently healthy.
func (s *Server) isNodeHealthy(nodeID string) bool {
	s.hrwMu.RLock()
	defer s.hrwMu.RUnlock()

	if s.hrw == nil {
		return false
	}

	node := s.hrw.GetNodeByID(nodeID)
	if node == nil {
		return false
	}

	return node.Healthy
}

// getBackupNodes returns backup nodes for a given key (excluding the primary).
// Used when the primary node is unhealthy to find replicas.
func (s *Server) getBackupNodes(storageID, filePath string) []*sharding.Node {
	s.hrwMu.RLock()
	defer s.hrwMu.RUnlock()

	if !s.enableForwarding || s.hrw == nil {
		return nil
	}

	shardKey := storageID
	if filePath != "" {
		shardKey = storageID + ":" + filePath
	}

	// Get top 3 nodes by HRW (primary + backups)
	rankedNodes := s.hrw.GetNodes(shardKey, 3)
	if len(rankedNodes) <= 1 {
		return nil
	}

	// Skip the first node (primary), return backups
	var backups []*sharding.Node
	for i := 1; i < len(rankedNodes); i++ {
		if rankedNodes[i].Healthy {
			backups = append(backups, &rankedNodes[i])
		}
	}

	return backups
}

// isAlreadyForwarded checks if this request has already been forwarded (loop detection).
func isAlreadyForwarded(ctx context.Context) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}

	// Check for forwarding marker
	forwarded := md.Get("x-monofs-forwarded")
	return len(forwarded) > 0
}

// withForwardingMarker adds forwarding marker to context for outgoing requests.
func withForwardingMarker(ctx context.Context, nodeID string) context.Context {
	md := metadata.New(map[string]string{
		"x-monofs-forwarded": nodeID,
	})
	return metadata.NewOutgoingContext(ctx, md)
}

// forwardLookup forwards a Lookup request to the target node.
func (s *Server) forwardLookup(ctx context.Context, req *pb.LookupRequest, targetNode *sharding.Node) (*pb.LookupResponse, error) {
	client := s.getPeerClient(targetNode.ID)
	if client == nil {
		return nil, fmt.Errorf("no connection to target node %s", targetNode.ID)
	}

	s.logger.Debug("forwarding lookup",
		"target_node", targetNode.ID,
		"path", req.ParentPath,
		"name", req.Name)

	forwardCtx := withForwardingMarker(ctx, s.nodeID)
	forwardCtx, cancel := context.WithTimeout(forwardCtx, s.rpcTimeout)
	defer cancel()

	resp, err := client.Lookup(forwardCtx, req)
	if err != nil {
		s.logger.Warn("forwarded lookup failed",
			"target_node", targetNode.ID,
			"error", err)
		return nil, err
	}

	return resp, nil
}

// forwardGetAttr forwards a GetAttr request to the target node.
func (s *Server) forwardGetAttr(ctx context.Context, req *pb.GetAttrRequest, targetNode *sharding.Node) (*pb.GetAttrResponse, error) {
	client := s.getPeerClient(targetNode.ID)
	if client == nil {
		return nil, fmt.Errorf("no connection to target node %s", targetNode.ID)
	}

	s.logger.Debug("forwarding getattr",
		"target_node", targetNode.ID,
		"path", req.Path)

	forwardCtx := withForwardingMarker(ctx, s.nodeID)
	forwardCtx, cancel := context.WithTimeout(forwardCtx, s.rpcTimeout)
	defer cancel()

	resp, err := client.GetAttr(forwardCtx, req)
	if err != nil {
		s.logger.Warn("forwarded getattr failed",
			"target_node", targetNode.ID,
			"error", err)
		return nil, err
	}

	return resp, nil
}

// forwardRead forwards a Read request to the target node (streaming).
func (s *Server) forwardRead(req *pb.ReadRequest, stream pb.MonoFS_ReadServer, targetNode *sharding.Node) error {
	client := s.getPeerClient(targetNode.ID)
	if client == nil {
		return fmt.Errorf("no connection to target node %s", targetNode.ID)
	}

	s.logger.Debug("forwarding read",
		"target_node", targetNode.ID,
		"path", req.Path)

	forwardCtx := withForwardingMarker(stream.Context(), s.nodeID)
	forwardCtx, cancel := context.WithTimeout(forwardCtx, s.rpcTimeout)
	defer cancel()

	// Create a client stream
	clientStream, err := client.Read(forwardCtx, req)
	if err != nil {
		s.logger.Warn("forwarded read failed",
			"target_node", targetNode.ID,
			"error", err)
		return err
	}

	// Proxy the stream
	for {
		chunk, err := clientStream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			s.logger.Warn("error receiving from forwarded read",
				"target_node", targetNode.ID,
				"error", err)
			return err
		}

		if err := stream.Send(chunk); err != nil {
			s.logger.Warn("error sending to client in forwarded read", "error", err)
			return err
		}
	}
}

// forwardReadDir forwards a ReadDir request to the target node (streaming).
func (s *Server) forwardReadDir(req *pb.ReadDirRequest, stream pb.MonoFS_ReadDirServer, targetNode *sharding.Node) error {
	client := s.getPeerClient(targetNode.ID)
	if client == nil {
		return fmt.Errorf("no connection to target node %s", targetNode.ID)
	}

	s.logger.Debug("forwarding readdir",
		"target_node", targetNode.ID,
		"path", req.Path)

	forwardCtx := withForwardingMarker(stream.Context(), s.nodeID)
	forwardCtx, cancel := context.WithTimeout(forwardCtx, s.rpcTimeout)
	defer cancel()

	// Create a client stream
	clientStream, err := client.ReadDir(forwardCtx, req)
	if err != nil {
		s.logger.Warn("forwarded readdir failed",
			"target_node", targetNode.ID,
			"error", err)
		return err
	}

	// Proxy the stream
	for {
		entry, err := clientStream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			s.logger.Warn("error receiving from forwarded readdir",
				"target_node", targetNode.ID,
				"error", err)
			return err
		}

		if err := stream.Send(entry); err != nil {
			s.logger.Warn("error sending to client in forwarded readdir", "error", err)
			return err
		}
	}
}

// getPeerClient returns the gRPC client for a peer node.
func (s *Server) getPeerClient(nodeID string) pb.MonoFSClient {
	s.hrwMu.RLock()
	defer s.hrwMu.RUnlock()

	if s.peerClients == nil {
		return nil
	}

	return s.peerClients[nodeID]
}

// forwardResult is a generic wrapper for forwarding responses.
type forwardResult[T any] struct {
	resp T
	ok   bool
}

// forwardToTarget forwards the request to the appropriate node if needed.
// It handles the HRW routing logic: tries the primary node first, then falls back to backups.
// Returns (forwarded=true, result, nil) if a successful response was received.
// Returns (forwarded=false, zero-value, nil) if the request should be handled locally.
//
// The forwardFn should perform the actual RPC call to the target node and return (response, success, error).
// The success boolean indicates whether the response is valid/usable (e.g., resp.Found for Lookup/GetAttr).
func (s *Server) forwardToTarget(ctx context.Context, storageID, filePath string, forwardFn func(node *sharding.Node) (forwardResult[interface{}], error)) (bool, interface{}, error) {
	if !s.enableForwarding || isAlreadyForwarded(ctx) {
		return false, nil, nil
	}

	targetNode := s.getTargetNode(storageID, filePath)
	if targetNode == nil {
		return false, nil, nil
	}

	// Try primary node first if healthy and not self
	if targetNode.ID != s.nodeID && s.isNodeHealthy(targetNode.ID) {
		s.logger.Debug("forwarding to primary node",
			"storage_id", storageID,
			"file_path", filePath,
			"target_node", targetNode.ID)
		result, err := forwardFn(targetNode)
		if err == nil && result.ok {
			return true, result.resp, nil
		}
		// Primary failed, will try backups below
	}

	// Primary is unhealthy or failed, try backup nodes
	if !s.isNodeHealthy(targetNode.ID) {
		backupNodes := s.getBackupNodes(storageID, filePath)
		for _, backup := range backupNodes {
			if backup.ID == s.nodeID {
				// This node is a backup, handle locally
				return false, nil, nil
			}
			s.logger.Debug("forwarding to backup node",
				"storage_id", storageID,
				"file_path", filePath,
				"primary", targetNode.ID,
				"backup", backup.ID)
			result, err := forwardFn(backup)
			if err == nil && result.ok {
				return true, result.resp, nil
			}
			// Try next backup
		}
	}

	// No successful forwarding, handle locally
	return false, nil, nil
}

// GetForwardingStats returns statistics about the forwarding functionality.
func (s *Server) GetForwardingStats() map[string]interface{} {
	s.hrwMu.RLock()
	defer s.hrwMu.RUnlock()

	stats := map[string]interface{}{
		"enabled":         s.enableForwarding,
		"cluster_version": s.clusterVersion.Load(),
	}

	if s.hrw != nil {
		stats["healthy_nodes"] = len(s.hrw.GetHealthyNodes())
		stats["total_nodes"] = len(s.hrw.GetAllNodes())
	}

	stats["peer_connections"] = len(s.peerClients)

	return stats
}

// Package sharding provides Rendezvous (Highest Random Weight) hashing
// for consistent distribution of keys across backend nodes.
package sharding

import (
	"encoding/binary"
	"hash/fnv"
	"sort"
	"sync"

	pb "github.com/radryc/monofs/api/proto"
)

// Node represents a backend node for sharding purposes.
type Node struct {
	ID      string
	Address string
	Weight  uint32
	Healthy bool
}

// HRW implements Rendezvous (Highest Random Weight) hashing.
// It provides consistent hashing with minimal key redistribution
// when nodes are added or removed.
type HRW struct {
	mu    sync.RWMutex
	nodes []Node
}

// NewHRW creates a new HRW hasher with the given nodes.
func NewHRW(nodes []Node) *HRW {
	h := &HRW{
		nodes: make([]Node, len(nodes)),
	}
	copy(h.nodes, nodes)
	return h
}

// NewHRWFromProto creates a new HRW hasher from proto NodeInfo slice.
func NewHRWFromProto(nodes []*pb.NodeInfo) *HRW {
	converted := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		converted = append(converted, Node{
			ID:      n.NodeId,
			Address: n.Address,
			Weight:  n.Weight,
			Healthy: n.Healthy,
		})
	}
	return NewHRW(converted)
}

// UpdateNodes replaces the node list atomically.
func (h *HRW) UpdateNodes(nodes []Node) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nodes = make([]Node, len(nodes))
	copy(h.nodes, nodes)
}

// UpdateNodesFromProto replaces the node list from proto NodeInfo slice.
func (h *HRW) UpdateNodesFromProto(nodes []*pb.NodeInfo) {
	converted := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		converted = append(converted, Node{
			ID:      n.NodeId,
			Address: n.Address,
			Weight:  n.Weight,
			Healthy: n.Healthy,
		})
	}
	h.UpdateNodes(converted)
}

// UpdateNodeHealthFromProto updates node health/address from proto while preserving order.
// New nodes are appended, existing nodes update health/address in place.
// Nodes not in the update are marked unhealthy but kept in their position.
func (h *HRW) UpdateNodeHealthFromProto(nodes []*pb.NodeInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Build lookup map from incoming nodes
	incoming := make(map[string]*pb.NodeInfo, len(nodes))
	for _, n := range nodes {
		incoming[n.NodeId] = n
	}

	// Update existing nodes in place (preserving order)
	seen := make(map[string]bool)
	for i := range h.nodes {
		if info, ok := incoming[h.nodes[i].ID]; ok {
			// Update health and address for existing node
			h.nodes[i].Address = info.Address
			h.nodes[i].Weight = info.Weight
			h.nodes[i].Healthy = info.Healthy
			seen[h.nodes[i].ID] = true
		} else {
			// Node not in update - mark unhealthy but keep position
			h.nodes[i].Healthy = false
		}
	}

	// Append truly new nodes (not seen before)
	for _, n := range nodes {
		if !seen[n.NodeId] {
			// Check if we already have this node (just wasn't in incoming)
			found := false
			for _, existing := range h.nodes {
				if existing.ID == n.NodeId {
					found = true
					break
				}
			}
			if !found {
				h.nodes = append(h.nodes, Node{
					ID:      n.NodeId,
					Address: n.Address,
					Weight:  n.Weight,
					Healthy: n.Healthy,
				})
			}
		}
	}
}

// SetNodeHealth sets the health status of a specific node by ID.
// Returns true if node was found and updated.
func (h *HRW) SetNodeHealth(nodeID string, healthy bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	for i := range h.nodes {
		if h.nodes[i].ID == nodeID {
			h.nodes[i].Healthy = healthy
			return true
		}
	}
	return false
}

// GetNode returns the best node for the given key using HRW algorithm.
// Only healthy nodes are considered. Returns nil if no healthy nodes exist.
func (h *HRW) GetNode(key string) *Node {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var bestNode *Node
	var bestScore uint64

	for i := range h.nodes {
		node := &h.nodes[i]
		if !node.Healthy {
			continue
		}

		score := h.computeScore(key, node)
		if bestNode == nil || score > bestScore {
			bestNode = node
			bestScore = score
		}
	}

	if bestNode == nil {
		return nil
	}

	// Return a copy to prevent mutation
	result := *bestNode
	return &result
}

// GetNodes returns the top N nodes for the given key, ordered by score.
// Useful for replication or fallback scenarios.
func (h *HRW) GetNodes(key string, n int) []Node {
	h.mu.RLock()
	defer h.mu.RUnlock()

	type scored struct {
		node  Node
		score uint64
	}

	var candidates []scored
	for _, node := range h.nodes {
		if !node.Healthy {
			continue
		}
		candidates = append(candidates, scored{
			node:  node,
			score: h.computeScore(key, &node),
		})
	}

	// Sort by score descending
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	// Take top N
	if n > len(candidates) {
		n = len(candidates)
	}

	result := make([]Node, n)
	for i := 0; i < n; i++ {
		result[i] = candidates[i].node
	}
	return result
}

// computeScore calculates the HRW score for a key-node pair.
// Score = hash(key || nodeID) * weight
func (h *HRW) computeScore(key string, node *Node) uint64 {
	hasher := fnv.New64a()
	hasher.Write([]byte(key))
	hasher.Write([]byte(node.ID))
	hash := hasher.Sum64()

	// Multiply by weight (default weight 1 if 0)
	weight := uint64(node.Weight)
	if weight == 0 {
		weight = 1
	}

	// Use saturating multiplication to avoid overflow issues
	// For HRW, we just need relative ordering, so simple multiplication works
	return hash * weight
}

// GetNodeByID returns a node by its ID, or nil if not found.
func (h *HRW) GetNodeByID(nodeID string) *Node {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for i := range h.nodes {
		if h.nodes[i].ID == nodeID {
			result := h.nodes[i]
			return &result
		}
	}
	return nil
}

// GetAllNodes returns a copy of all nodes.
func (h *HRW) GetAllNodes() []Node {
	h.mu.RLock()
	defer h.mu.RUnlock()

	result := make([]Node, len(h.nodes))
	copy(result, h.nodes)
	return result
}

// GetHealthyNodes returns only healthy nodes.
func (h *HRW) GetHealthyNodes() []Node {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var result []Node
	for _, n := range h.nodes {
		if n.Healthy {
			result = append(result, n)
		}
	}
	return result
}

// NodeCount returns the total number of nodes.
func (h *HRW) NodeCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.nodes)
}

// HealthyNodeCount returns the number of healthy nodes.
func (h *HRW) HealthyNodeCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()

	count := 0
	for _, n := range h.nodes {
		if n.Healthy {
			count++
		}
	}
	return count
}

// HashKey returns a consistent hash for the given key.
// Can be used for debugging or logging.
func HashKey(key string) uint64 {
	hasher := fnv.New64a()
	hasher.Write([]byte(key))
	return hasher.Sum64()
}

// HashKeyBytes returns a consistent hash for the given bytes.
func HashKeyBytes(data []byte) uint64 {
	hasher := fnv.New64a()
	hasher.Write(data)
	return hasher.Sum64()
}

// HashKeyUint64 returns a consistent hash for a uint64.
func HashKeyUint64(v uint64) uint64 {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return HashKeyBytes(b)
}

package router

import (
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

// uiNodeSnapshot is a plain-value copy of the nodeState fields consumed by UI
// handlers. nodeState fields are mutated by the health loop under r.mu, so UI
// code must not dereference a *nodeState after releasing the lock.
type uiNodeSnapshot struct {
	id              string
	nodeID          string
	address         string
	externalAddress string
	healthy         bool
	weight          uint32
	lastSeen        time.Time
	client          pb.MonoFSClient
	status          NodeStatus
	syncProgress    float64
	ownedFilesCount int64
	diskUsedBytes   int64
	diskTotalBytes  int64
	diskFreeBytes   int64
	kvsStatus       *pb.KVSNodeStatus
	backingUpNodes  []string
	logEngineStats  *pb.LogEngineStats
}

// snapshotUINodesLocked must be called with r.mu held for reading.
func (r *Router) snapshotUINodesLocked() []uiNodeSnapshot {
	out := make([]uiNodeSnapshot, 0, len(r.nodes))
	for id, state := range r.nodes {
		if state == nil {
			continue
		}
		snapshot := uiNodeSnapshot{
			id:              id,
			lastSeen:        state.lastSeen,
			client:          state.client,
			status:          state.status,
			syncProgress:    state.syncProgress,
			ownedFilesCount: state.ownedFilesCount,
			diskUsedBytes:   state.diskUsedBytes,
			diskTotalBytes:  state.diskTotalBytes,
			diskFreeBytes:   state.diskFreeBytes,
			kvsStatus:       state.kvsStatus,
			backingUpNodes:  append([]string(nil), state.backingUpNodes...),
			logEngineStats:  state.logEngineStats,
			externalAddress: state.externalAddress,
		}
		if state.info != nil {
			snapshot.nodeID = state.info.NodeId
			snapshot.address = state.info.Address
			snapshot.healthy = state.info.Healthy
			snapshot.weight = state.info.Weight
		}
		out = append(out, snapshot)
	}
	return out
}

// snapshotUINodes copies node state under the router lock.
func (r *Router) snapshotUINodes() []uiNodeSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshotUINodesLocked()
}

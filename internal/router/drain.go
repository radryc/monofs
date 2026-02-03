// Package router provides drain/undrain functionality for cluster maintenance.
package router

import (
	"context"
	"fmt"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

// DrainCluster puts the cluster in maintenance mode, disabling failover.
// This allows nodes to be safely shut down without triggering failover logic.
func (r *Router) DrainCluster(ctx context.Context, req *pb.DrainClusterRequest) (*pb.DrainClusterResponse, error) {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()

	if r.drainMode.Load() {
		return &pb.DrainClusterResponse{
			Success: false,
			Message: "cluster is already in drain mode",
		}, nil
	}

	r.drainMode.Store(true)
	r.drainedAt = time.Now()
	r.drainReason = req.Reason

	reason := req.Reason
	if reason == "" {
		reason = "planned maintenance"
	}

	r.logger.Warn("cluster entered drain mode - failover disabled",
		"reason", reason,
		"drained_at", r.drainedAt)

	return &pb.DrainClusterResponse{
		Success:   true,
		Message:   fmt.Sprintf("cluster drained for: %s", reason),
		DrainedAt: r.drainedAt.Unix(),
	}, nil
}

// UndrainCluster exits maintenance mode, re-enabling failover.
func (r *Router) UndrainCluster(ctx context.Context, req *pb.UndrainClusterRequest) (*pb.UndrainClusterResponse, error) {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()

	if !r.drainMode.Load() {
		return &pb.UndrainClusterResponse{
			Success: false,
			Message: "cluster is not in drain mode",
		}, nil
	}

	r.drainMode.Store(false)
	duration := time.Since(r.drainedAt)

	r.logger.Info("cluster exited drain mode - failover re-enabled",
		"drain_duration", duration,
		"reason", r.drainReason)

	r.drainReason = ""

	return &pb.UndrainClusterResponse{
		Success: true,
		Message: fmt.Sprintf("cluster undrained after %v", duration.Round(time.Second)),
	}, nil
}

// IsDrained returns whether the cluster is in drain mode.
func (r *Router) IsDrained() bool {
	return r.drainMode.Load()
}

package router

import (
	"context"
	"log/slog"
	"strings"

	"github.com/radryc/monofs/pkg/repowatch"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultGuardianCoordinatorLease = "monofs-guardian-mutations"

// startGuardianCoordinatorLease starts the cross-router mutation lease. The
// lease serializes check -> node apply -> KVS commit across routers; KVS CAS
// alone cannot prevent a losing writer from leaving content on a node.
func (r *Router) startGuardianCoordinatorLease(logger *slog.Logger) {
	mode := strings.ToLower(strings.TrimSpace(r.config.GuardianCoordinatorLease))
	backend := strings.ToLower(strings.TrimSpace(r.config.GuardianVersionBackend))
	if mode == "" {
		if backend == "kvs" {
			mode = "required"
		} else {
			mode = "off"
		}
	}
	r.guardianLeaseMode = mode
	if mode == "off" {
		r.guardianLeader.Store(true)
		r.guardianLeaseActive.Store(false)
		return
	}

	name := strings.TrimSpace(r.config.GuardianCoordinatorLeaseName)
	if name == "" {
		name = defaultGuardianCoordinatorLease
	}
	cfg := repowatch.DefaultLeaseConfig(name)
	if ns := strings.TrimSpace(r.config.GuardianCoordinatorLeaseNS); ns != "" {
		cfg.Namespace = ns
	}
	if !cfg.Enabled() {
		if mode == "required" {
			r.guardianLeaseUnavailable.Store(true)
			r.guardianLeader.Store(false)
			logger.Error("guardian coordinator lease is required but Kubernetes is not configured; guardian mutations will be rejected")
			return
		}
		logger.Warn("guardian coordinator lease requested but Kubernetes is not configured; running single-router mode")
		r.guardianLeader.Store(true)
		r.guardianLeaseActive.Store(false)
		return
	}
	elector, err := repowatch.NewElector(cfg)
	if err != nil {
		if mode == "required" {
			r.guardianLeaseUnavailable.Store(true)
			r.guardianLeader.Store(false)
			logger.Error("guardian coordinator lease elector failed; guardian mutations will be rejected", "error", err)
			return
		}
		logger.Warn("guardian coordinator lease elector failed; running single-router mode", "error", err)
		r.guardianLeader.Store(true)
		r.guardianLeaseActive.Store(false)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.guardianLeaseCancel = cancel
	r.guardianLeaseActive.Store(true)
	go func() {
		err := elector.Run(ctx,
			func(context.Context) {
				r.guardianLeader.Store(true)
				guardianCoordinatorLeader.Set(1)
				logger.Info("acquired guardian coordinator lease", "lease", name)
			},
			func() {
				r.guardianLeader.Store(false)
				guardianCoordinatorLeader.Set(0)
				logger.Warn("lost guardian coordinator lease", "lease", name)
			},
		)
		if err != nil && ctx.Err() == nil {
			logger.Error("guardian coordinator lease elector stopped", "error", err)
			r.guardianLeader.Store(false)
			guardianCoordinatorLeader.Set(0)
		}
	}()
}

// requireGuardianCoordinator rejects mutations when this router is not the
// guardian mutation leader. Reads are always allowed.
func (r *Router) requireGuardianCoordinator() error {
	if r.config.GuardianAllowMultiWriter {
		return nil
	}
	if r.guardianLeaseUnavailable.Load() {
		return status.Error(codes.Unavailable, "guardian coordinator lease is unavailable")
	}
	if r.guardianLeaseMode == "off" {
		if strings.EqualFold(strings.TrimSpace(r.config.GuardianVersionBackend), "kvs") {
			return status.Error(codes.Unavailable, "kvs guardian version backend requires the coordinator lease (or --guardian-allow-multi-writer)")
		}
		return nil
	}
	if !r.guardianLeader.Load() {
		return status.Error(codes.Unavailable, "router is not the guardian mutation leader")
	}
	return nil
}

func (r *Router) stopGuardianCoordinatorLease() {
	if r.guardianLeaseCancel != nil {
		r.guardianLeaseCancel()
		r.guardianLeaseCancel = nil
	}
	r.guardianLeader.Store(false)
	guardianCoordinatorLeader.Set(0)
}

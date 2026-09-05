package router

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/router/workspacepolicy"
)

const (
	defaultAutoPushInterval   = 60 * time.Second
	defaultConcurrencyCap    = 20
	minAutoPushInterval      = 30 * time.Second
	maxBackoffInterval       = 30 * time.Minute
	deadLetterAfterFailures  = 10
)

type autoPushWorker struct {
	router       *Router
	logger       *slog.Logger
	interval     time.Duration
	concurrency  int
	stop         chan struct{}

	mu           sync.Mutex
	active       map[string]bool
	failureCount map[string]int
	backoffUntil map[string]time.Time
}

func newAutoPushWorker(r *Router, interval time.Duration, concurrency int, logger *slog.Logger) *autoPushWorker {
	if interval < minAutoPushInterval {
		interval = minAutoPushInterval
	}
	if concurrency <= 0 {
		concurrency = defaultConcurrencyCap
	}
	return &autoPushWorker{
		router:       r,
		logger:       logger.With("component", "autopush"),
		interval:     interval,
		concurrency:  concurrency,
		stop:         make(chan struct{}),
		active:       make(map[string]bool),
		failureCount: make(map[string]int),
		backoffUntil: make(map[string]time.Time),
	}
}

func (w *autoPushWorker) Start() {
	w.logger.Info("auto-push worker started", "interval", w.interval, "concurrency", w.concurrency)
	go w.loop()
}

func (w *autoPushWorker) Stop() {
	close(w.stop)
	w.logger.Info("auto-push worker stopped")
}

func (w *autoPushWorker) loop() {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.scan()
		case <-w.stop:
			return
		}
	}
}

func (w *autoPushWorker) scan() {
	w.logger.Debug("auto-push scan starting")

	candidates := w.getCandidates()
	if len(candidates) == 0 {
		return
	}

	sem := make(chan struct{}, w.concurrency)
	var wg sync.WaitGroup

	for _, ws := range candidates {
		wg.Add(1)
		sem <- struct{}{}

		go func(workspaceID string) {
			defer wg.Done()
			defer func() { <-sem }()

			w.tryPush(workspaceID)
		}(ws)
	}

	wg.Wait()
}

func (w *autoPushWorker) getCandidates() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	workspaces := make(map[string]bool)
	w.router.workspaceSyncMu.RLock()
	for _, entry := range w.router.workspaceSyncJobs {
		job := entry.snapshot()
		if job.GetAction() == pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_SOURCE_PUSH &&
			job.GetState() == pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_QUEUED {
			wsID := job.GetWorkspaceId()
			if w.active[wsID] {
				continue
			}
			if until, ok := w.backoffUntil[wsID]; ok && time.Now().Before(until) {
				continue
			}
			if w.failureCount[wsID] >= deadLetterAfterFailures {
				continue
			}
			workspaces[wsID] = true
		}
	}
	w.router.workspaceSyncMu.RUnlock()

	var candidates []string
	for ws := range workspaces {
		candidates = append(candidates, ws)
	}
	return candidates
}

func (w *autoPushWorker) tryPush(workspaceID string) {
	w.mu.Lock()
	if w.active[workspaceID] {
		w.mu.Unlock()
		return
	}
	w.active[workspaceID] = true
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		delete(w.active, workspaceID)
		w.mu.Unlock()
	}()

	w.logger.Info("auto-push attempting", "workspace_id", workspaceID)

	w.router.workspaceSyncMu.RLock()
	var bundleID, logicalBranch string
	for _, entry := range w.router.workspaceSyncJobs {
		job := entry.snapshot()
		if job.GetWorkspaceId() == workspaceID &&
			job.GetAction() == pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_SOURCE_PUSH &&
			job.GetState() == pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_QUEUED {
			bundleID = job.GetBundleId()
			logicalBranch = job.GetLogicalBranch()
			break
		}
	}
	w.router.workspaceSyncMu.RUnlock()

	if bundleID == "" {
		w.logger.Debug("auto-push skipped, no pending bundle", "workspace_id", workspaceID)
		return
	}

	bundleEntry := w.router.getWorkspaceBundle(bundleID)
	if bundleEntry == nil || bundleEntry.commitBundle == nil {
		w.recordFailure(workspaceID)
		w.logger.Warn("auto-push failed, bundle not found", "workspace_id", workspaceID, "bundle_id", bundleID)
		return
	}

	pushMode := w.router.config.SourcePushMode
	policyResult, err := w.router.evalPolicy(&workspacepolicy.EvaluationRequest{
		PrincipalID:    "autopush",
		WorkspaceID:    workspaceID,
		LogicalBranch:  logicalBranch,
		RepositoryIDs:  storageIDsFromSourceBundle(bundleEntry.commitBundle),
		Action:         workspacepolicy.ActionSourcePush,
		PushMode:       pushMode,
	})
	if err != nil {
		w.recordFailure(workspaceID)
		w.logger.Error("auto-push policy check failed", "workspace_id", workspaceID, "error", err)
		return
	}
	if policyResult.Effect != workspacepolicy.EffectAllow {
		w.logger.Warn("auto-push denied by policy", "workspace_id", workspaceID, "reason", policyResult.Reason)
		return
	}

	fetcherClient := w.router.getFetcherClient()
	if fetcherClient == nil {
		w.recordFailure(workspaceID)
		w.logger.Warn("auto-push skipped, no fetcher client", "workspace_id", workspaceID)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if _, err := fetcherClient.StageWorkspaceCommitBundle(ctx, bundleID, workspaceID, bundleEntry.data); err != nil {
		w.recordFailure(workspaceID)
		w.logger.Warn("auto-push stage failed", "workspace_id", workspaceID, "error", err)
		return
	}

	pushResults, err := fetcherClient.StartWorkspaceCommitPush(ctx, &pb.StartWorkspaceCommitPushRequest{
		JobId:          fmt.Sprintf("wsync-autopush-%d", time.Now().UnixNano()),
		WorkspaceId:    workspaceID,
		BundleId:       bundleID,
		LogicalBranch:  logicalBranch,
		SourcePushMode: pushMode,
	})
	if err != nil {
		w.recordFailure(workspaceID)
		w.logger.Warn("auto-push failed", "workspace_id", workspaceID, "error", err)
		return
	}

	allSucceeded := true
	for _, progress := range pushResults {
		if progress.GetStatus() != pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED &&
			progress.GetStatus() != pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED {
			allSucceeded = false
		}
	}

	if allSucceeded {
		w.resetBackoff(workspaceID)
		w.logger.Info("auto-push succeeded", "workspace_id", workspaceID)
	} else {
		w.recordFailure(workspaceID)
	}
}

func (w *autoPushWorker) recordFailure(workspaceID string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.failureCount[workspaceID]++
	count := w.failureCount[workspaceID]

	backoff := time.Duration(1<<uint(min(count-1, 5))) * time.Minute
	if backoff > maxBackoffInterval {
		backoff = maxBackoffInterval
	}
	w.backoffUntil[workspaceID] = time.Now().Add(backoff)

	if count >= deadLetterAfterFailures {
		w.logger.Warn("auto-push dead-lettered", "workspace_id", workspaceID, "failures", count)
	}
}

func (w *autoPushWorker) resetBackoff(workspaceID string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	delete(w.failureCount, workspaceID)
	delete(w.backoffUntil, workspaceID)
}

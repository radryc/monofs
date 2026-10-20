package router

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/pkg/repowatch"
)

const (
	defaultAutoRefreshInterval    = 1 * time.Minute
	defaultAutoRefreshMaxInterval = 1 * time.Hour
	defaultAutoRefreshConcurrency = 4
	defaultAutoRefreshBatchSize   = 32
	defaultAutoRefreshShards      = 1
	defaultAutoRefreshStateDir    = ".monofs-repowatch"
	autoRefreshBackoffMax         = 1 * time.Hour
	autoRefreshSyncInterval       = 30 * time.Second
	autoRefreshSaveInterval       = 15 * time.Second
	reingestDedupWindow           = 60 * time.Second
)

// repoRefreshState is the per-repository trace kept by the auto-refresh
// worker. The durable commit hash lives on the node metadata; these fields
// describe the watcher's last observation.
type repoRefreshState struct {
	lastProbedAt    time.Time
	lastProbeStatus string
	lastProbeError  string
	lastChangeAt    time.Time
	changesTotal    int64
	reingestsTotal  int64
	nextProbeAt     time.Time
}

func (r *ingestedRepo) markProbeStart(at time.Time) {
	if r == nil {
		return
	}
	r.refreshMu.Lock()
	r.refreshState.lastProbedAt = at
	r.refreshMu.Unlock()
}

func (r *ingestedRepo) markProbeResult(status string, at time.Time, next time.Time) {
	if r == nil {
		return
	}
	r.refreshMu.Lock()
	r.refreshState.lastProbedAt = at
	r.refreshState.lastProbeStatus = status
	r.refreshState.lastProbeError = ""
	r.refreshState.nextProbeAt = next
	r.refreshMu.Unlock()
}

func (r *ingestedRepo) markProbeError(errText string, at time.Time, next time.Time) {
	if r == nil {
		return
	}
	r.refreshMu.Lock()
	r.refreshState.lastProbedAt = at
	r.refreshState.lastProbeStatus = "error"
	r.refreshState.lastProbeError = errText
	r.refreshState.nextProbeAt = next
	r.refreshMu.Unlock()
}

func (r *ingestedRepo) markAdvance(at time.Time, next time.Time, reingested bool) {
	if r == nil {
		return
	}
	r.refreshMu.Lock()
	r.refreshState.lastProbedAt = at
	r.refreshState.lastProbeStatus = "advanced"
	r.refreshState.lastProbeError = ""
	r.refreshState.lastChangeAt = at
	r.refreshState.nextProbeAt = next
	r.refreshState.changesTotal++
	if reingested {
		r.refreshState.reingestsTotal++
	}
	r.refreshMu.Unlock()
}

func (r *ingestedRepo) refreshSnapshot() repoRefreshState {
	if r == nil {
		return repoRefreshState{}
	}
	r.refreshMu.RLock()
	defer r.refreshMu.RUnlock()
	return r.refreshState
}

func (r *ingestedRepo) restoreRefreshState(state repoRefreshState) {
	if r == nil {
		return
	}
	r.refreshMu.Lock()
	r.refreshState = state
	r.refreshMu.Unlock()
}

// refreshJSON returns the API shape for one repository. Unix seconds match the
// rest of the UI payload and avoid zero-value timestamp strings.
func (r *ingestedRepo) refreshJSON() map[string]any {
	state := r.refreshSnapshot()
	return map[string]any{
		"last_probed_at":    unixOrZero(state.lastProbedAt),
		"last_probe_status": state.lastProbeStatus,
		"last_probe_error":  state.lastProbeError,
		"last_change_at":    unixOrZero(state.lastChangeAt),
		"changes_total":     state.changesTotal,
		"reingests_total":   state.reingestsTotal,
		"next_probe_at":     unixOrZero(state.nextProbeAt),
	}
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// watchEntry is one due-time entry in the auto-refresh min-heap.
type watchEntry struct {
	storageID string
	due       time.Time
	index     int
}

type watchHeap []*watchEntry

func (h watchHeap) Len() int           { return len(h) }
func (h watchHeap) Less(i, j int) bool { return h[i].due.Before(h[j].due) }
func (h watchHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *watchHeap) Push(x any) {
	entry := x.(*watchEntry)
	entry.index = len(*h)
	*h = append(*h, entry)
}
func (h *watchHeap) Pop() any {
	old := *h
	n := len(old)
	entry := old[n-1]
	old[n-1] = nil
	entry.index = -1
	*h = old[:n-1]
	return entry
}

// watchRecord is the persisted schedule/trace for one repository.
type watchRecord struct {
	StorageID      string    `json:"storage_id"`
	RepoURL        string    `json:"repo_url"`
	Branch         string    `json:"branch"`
	CommitHash     string    `json:"commit_hash"`
	NextProbeAt    time.Time `json:"next_probe_at"`
	LastProbedAt   time.Time `json:"last_probed_at"`
	LastStatus     string    `json:"last_status"`
	LastError      string    `json:"last_error"`
	LastChangeAt   time.Time `json:"last_change_at"`
	ChangesTotal   int64     `json:"changes_total"`
	ReingestsTotal int64     `json:"reingests_total"`
	FailureCount   int       `json:"failure_count"`
	BackoffUntil   time.Time `json:"backoff_until"`
}

type watchStateFile struct {
	Shard   int                    `json:"shard"`
	Shards  int                    `json:"shards"`
	SavedAt time.Time              `json:"saved_at"`
	Repos   map[string]watchRecord `json:"repos"`
}

// autoRefreshWorker keeps a due-time heap of repositories and probes the
// fetcher in batches. It is sharded and optionally guarded by a Kubernetes
// Lease, so several router replicas can share the work and survive failures.
type autoRefreshWorker struct {
	router      *Router
	logger      *slog.Logger
	interval    time.Duration
	maxInterval time.Duration
	concurrency int
	batchSize   int
	shard       int
	shards      int
	stateDir    string
	lease       bool
	leaseName   string
	leaseNS     string

	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once
	batchSem chan struct{}

	mu           sync.Mutex
	active       map[string]bool
	failureCount map[string]int
	backoffUntil map[string]time.Time
	lastScan     time.Time
	probes       map[string]int64
	reingests    int64
	backoffs     int64
	leaseHeld    bool

	scheduleMu sync.Mutex
	entries    map[string]*watchEntry
	queue      watchHeap

	stateMu sync.Mutex
	records map[string]watchRecord
}

func newAutoRefreshWorker(r *Router, interval time.Duration, concurrency int, logger *slog.Logger) *autoRefreshWorker {
	if interval <= 0 {
		interval = defaultAutoRefreshInterval
	}
	if concurrency <= 0 {
		concurrency = defaultAutoRefreshConcurrency
	}
	return &autoRefreshWorker{
		router:       r,
		logger:       logger.With("component", "autorefresh"),
		interval:     interval,
		maxInterval:  defaultAutoRefreshMaxInterval,
		concurrency:  concurrency,
		batchSize:    defaultAutoRefreshBatchSize,
		shards:       defaultAutoRefreshShards,
		stateDir:     defaultAutoRefreshStateDir,
		active:       make(map[string]bool),
		failureCount: make(map[string]int),
		backoffUntil: make(map[string]time.Time),
		probes:       make(map[string]int64),
		entries:      make(map[string]*watchEntry),
		records:      make(map[string]watchRecord),
	}
}

func (w *autoRefreshWorker) configure(shard, shards, batchSize int, maxInterval time.Duration, stateDir string, lease bool, leaseName, leaseNS string) {
	if shards <= 0 {
		shards = defaultAutoRefreshShards
	}
	if shard < 0 || shard >= shards {
		shard = 0
	}
	if batchSize <= 0 {
		batchSize = defaultAutoRefreshBatchSize
	}
	if maxInterval <= 0 || maxInterval < w.interval {
		maxInterval = defaultAutoRefreshMaxInterval
	}
	w.shard = shard
	w.shards = shards
	w.batchSize = batchSize
	w.maxInterval = maxInterval
	if strings.TrimSpace(stateDir) != "" {
		w.stateDir = strings.TrimSpace(stateDir)
	}
	w.lease = lease
	if strings.TrimSpace(leaseName) != "" {
		w.leaseName = strings.TrimSpace(leaseName)
	}
	w.leaseNS = strings.TrimSpace(leaseNS)
}

func (w *autoRefreshWorker) Start() {
	w.stopOnce = sync.Once{}
	w.batchSem = make(chan struct{}, maxInt(1, w.concurrency))
	w.ctx, w.cancel = context.WithCancel(context.Background())
	go w.run(w.ctx)
}

func (w *autoRefreshWorker) Stop() {
	w.stopOnce.Do(func() {
		if w.cancel != nil {
			w.cancel()
		}
	})
	w.logger.Info("auto-refresh worker stopped")
}

func (w *autoRefreshWorker) run(ctx context.Context) {
	if w.lease {
		cfg := repowatch.DefaultLeaseConfig(w.leaseName)
		if w.leaseNS != "" {
			cfg.Namespace = w.leaseNS
		}
		if w.shards > 1 {
			cfg.Name = fmt.Sprintf("%s-%d", w.leaseName, w.shard)
		}
		if cfg.Enabled() {
			elector, err := repowatch.NewElector(cfg)
			if err == nil {
				w.logger.Info("auto-refresh waiting for lease", "lease", cfg.Name, "namespace", cfg.Namespace, "identity", cfg.Identity)
				_ = elector.Run(ctx, func(leaderCtx context.Context) {
					w.setLeaseHeld(true)
					defer w.setLeaseHeld(false)
					w.logger.Info("auto-refresh acquired lease", "lease", cfg.Name, "identity", cfg.Identity)
					w.runScheduler(leaderCtx)
				}, nil)
				return
			}
			w.logger.Warn("auto-refresh lease unavailable; using static shard ownership", "error", err)
		} else {
			w.logger.Warn("auto-refresh lease requested but Kubernetes API is not configured; using static shard ownership")
		}
	}
	w.runScheduler(ctx)
}

func (w *autoRefreshWorker) runScheduler(ctx context.Context) {
	w.loadState()
	w.syncRepos()
	w.logger.Info("auto-refresh scheduler started",
		"interval", w.interval, "max_interval", w.maxInterval,
		"concurrency", w.concurrency, "batch", w.batchSize,
		"shard", w.shard, "shards", w.shards)

	tick := time.NewTicker(time.Second)
	syncTick := time.NewTicker(autoRefreshSyncInterval)
	saveTick := time.NewTicker(autoRefreshSaveInterval)
	defer tick.Stop()
	defer syncTick.Stop()
	defer saveTick.Stop()
	defer w.saveState()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.processDue(ctx)
		case <-syncTick.C:
			w.syncRepos()
		case <-saveTick.C:
			w.saveState()
		}
	}
}

func (w *autoRefreshWorker) setLeaseHeld(held bool) {
	w.mu.Lock()
	w.leaseHeld = held
	w.mu.Unlock()
}

func (w *autoRefreshWorker) syncRepos() {
	snapshot := w.router.ingestedRepoSnapshot()
	now := time.Now()

	w.scheduleMu.Lock()
	defer w.scheduleMu.Unlock()

	present := make(map[string]struct{}, len(snapshot))
	for storageID, repo := range snapshot {
		if repo == nil || strings.TrimSpace(repo.repoURL) == "" {
			continue
		}
		if repowatch.ShardFor(storageID, w.shards) != w.shard {
			continue
		}
		present[storageID] = struct{}{}
		if _, ok := w.entries[storageID]; ok {
			continue
		}
		record, hasRecord := w.record(storageID)
		if hasRecord && (record.RepoURL != repo.repoURL || record.Branch != repo.branch) {
			hasRecord = false
		}
		if hasRecord {
			repo.restoreRefreshState(repoRefreshState{
				lastProbedAt:    record.LastProbedAt,
				lastProbeStatus: record.LastStatus,
				lastProbeError:  record.LastError,
				lastChangeAt:    record.LastChangeAt,
				changesTotal:    record.ChangesTotal,
				reingestsTotal:  record.ReingestsTotal,
				nextProbeAt:     record.NextProbeAt,
			})
			w.mu.Lock()
			if record.FailureCount > 0 {
				w.failureCount[storageID] = record.FailureCount
			}
			if !record.BackoffUntil.IsZero() {
				w.backoffUntil[storageID] = record.BackoffUntil
			}
			w.mu.Unlock()
		}
		due := now.Add(jitter(w.interval))
		if hasRecord && !record.NextProbeAt.IsZero() {
			due = record.NextProbeAt
			if due.Before(now) {
				due = now
			}
		} else {
			due = due.Add(jitter(w.interval))
		}
		entry := &watchEntry{storageID: storageID, due: due}
		w.entries[storageID] = entry
		heap.Push(&w.queue, entry)
	}
	for storageID, entry := range w.entries {
		if _, ok := present[storageID]; ok {
			continue
		}
		if entry.index >= 0 {
			heap.Remove(&w.queue, entry.index)
		}
		delete(w.entries, storageID)
	}
	w.mu.Lock()
	w.lastScan = now
	w.mu.Unlock()
}

func (w *autoRefreshWorker) processDue(ctx context.Context) {
	now := time.Now()
	w.scheduleMu.Lock()
	limit := maxInt(w.concurrency*w.batchSize, w.batchSize)
	due := make([]*watchEntry, 0, minInt(limit, len(w.queue)))
	for len(w.queue) > 0 && !w.queue[0].due.After(now) && len(due) < limit {
		entry := heap.Pop(&w.queue).(*watchEntry)
		delete(w.entries, entry.storageID)
		due = append(due, entry)
	}
	w.scheduleMu.Unlock()
	if len(due) == 0 {
		return
	}

	ready := due[:0]
	for _, entry := range due {
		if !w.eligible(entry.storageID) {
			w.scheduleAt(entry.storageID, w.nextEligible(entry.storageID))
			continue
		}
		ready = append(ready, entry)
	}
	for len(ready) > 0 {
		n := w.batchSize
		if n > len(ready) {
			n = len(ready)
		}
		batch := append([]*watchEntry(nil), ready[:n]...)
		ready = ready[n:]
		for _, entry := range batch {
			w.markActive(entry.storageID)
		}
		select {
		case w.batchSem <- struct{}{}:
			go func(batch []*watchEntry) {
				defer func() { <-w.batchSem }()
				defer func() {
					for _, entry := range batch {
						w.clearActive(entry.storageID)
					}
				}()
				w.processBatch(ctx, batch)
			}(batch)
		case <-ctx.Done():
			for _, entry := range batch {
				w.clearActive(entry.storageID)
				w.scheduleAt(entry.storageID, time.Now().Add(w.interval))
			}
			return
		}
	}
}

func (w *autoRefreshWorker) processBatch(ctx context.Context, batch []*watchEntry) {
	fetcherClient := w.router.getFetcherClient()
	if fetcherClient == nil {
		for _, entry := range batch {
			w.applyProbeError(entry.storageID, "no fetcher client", time.Now())
		}
		return
	}
	refs := make([]*pb.WorkspaceRepositoryRef, 0, len(batch))
	for _, entry := range batch {
		repo := w.repoFor(entry.storageID)
		if repo == nil || strings.TrimSpace(repo.repoURL) == "" {
			continue
		}
		branch := repo.branch
		if branch == "" {
			branch = "main"
		}
		repo.markProbeStart(time.Now())
		refs = append(refs, &pb.WorkspaceRepositoryRef{
			StorageId:    entry.storageID,
			DisplayPath:  repo.repoID,
			RepoUrl:      repo.repoURL,
			Branch:       branch,
			BaseCommit:   repo.commitHash,
			MetadataOnly: true,
		})
	}
	if len(refs) == 0 {
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	results, err := fetcherClient.ProbeWorkspaceRefresh(callCtx, &pb.ProbeWorkspaceRefreshRequest{
		JobId:        fmt.Sprintf("autorefresh-%d", time.Now().UnixNano()),
		WorkspaceId:  "autorefresh",
		Repositories: refs,
	})
	if err != nil {
		for _, entry := range batch {
			w.applyProbeError(entry.storageID, err.Error(), time.Now())
		}
		return
	}
	byID := make(map[string]*pb.RepoSyncProgress, len(results))
	for i, progress := range results {
		id := progress.GetRepository().GetStorageId()
		if id == "" && i < len(batch) {
			id = batch[i].storageID
		}
		if id != "" {
			byID[id] = progress
		}
	}
	for _, entry := range batch {
		repo := w.repoFor(entry.storageID)
		if repo == nil {
			continue
		}
		progress := byID[entry.storageID]
		if progress == nil {
			w.applyProbeError(entry.storageID, "probe returned no result", time.Now())
			continue
		}
		branch := repo.branch
		if branch == "" {
			branch = "main"
		}
		w.applyResult(entry.storageID, repo, progress.GetStatus(), progress.GetMessage(), "poll", branch)
	}
}

func (w *autoRefreshWorker) applyProbeError(storageID, message string, now time.Time) {
	backoff := w.recordFailure(storageID)
	if repo := w.repoFor(storageID); repo != nil {
		repo.markProbeError(message, now, now.Add(backoff))
	}
	w.incProbe("error")
	routerAutoRefreshProbesTotal.WithLabelValues("error").Inc()
	w.scheduleAt(storageID, now.Add(backoff))
}

func (w *autoRefreshWorker) applyResult(storageID string, repo *ingestedRepo, status pb.RepoSyncStatus, message, reason, branch string) {
	now := time.Now()
	switch status {
	case pb.RepoSyncStatus_REPO_SYNC_STATUS_ADVANCED:
		w.incProbe("advanced")
		routerAutoRefreshProbesTotal.WithLabelValues("advanced").Inc()
		w.resetBackoff(storageID)
		reingested := w.router.reingestRepoAsync(storageID, reason, branch)
		if reingested {
			routerAutoRefreshReingestsTotal.Inc()
			w.incReingest()
		}
		if repo != nil {
			repo.markAdvance(now, now.Add(w.nextInterval(repo, now, true)), reingested)
		}
		w.scheduleAt(storageID, now.Add(w.nextInterval(repo, now, true)))
	case pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED:
		w.incProbe("unchanged")
		routerAutoRefreshProbesTotal.WithLabelValues("unchanged").Inc()
		w.resetBackoff(storageID)
		if repo != nil {
			repo.markProbeResult("unchanged", now, now.Add(w.nextInterval(repo, now, false)))
		}
		w.scheduleAt(storageID, now.Add(w.nextInterval(repo, now, false)))
	case pb.RepoSyncStatus_REPO_SYNC_STATUS_DIVERGED, pb.RepoSyncStatus_REPO_SYNC_STATUS_MISSING_BRANCH:
		backoff := w.recordFailure(storageID)
		if repo != nil {
			repo.markProbeError(status.String(), now, now.Add(backoff))
		}
		w.incProbe("diverged")
		routerAutoRefreshProbesTotal.WithLabelValues("diverged").Inc()
		w.logger.Warn("auto-refresh conflict detected, skipping re-ingest",
			"storage_id", storageID, "status", status)
		w.scheduleAt(storageID, now.Add(backoff))
	default:
		backoff := w.recordFailure(storageID)
		if repo != nil {
			repo.markProbeError(message, now, now.Add(backoff))
		}
		w.incProbe("error")
		routerAutoRefreshProbesTotal.WithLabelValues("error").Inc()
		w.scheduleAt(storageID, now.Add(backoff))
	}
}

func (w *autoRefreshWorker) nextInterval(repo *ingestedRepo, now time.Time, changed bool) time.Duration {
	if changed || repo == nil {
		return w.interval + jitter(w.interval/5)
	}
	state := repo.refreshSnapshot()
	lastChange := state.lastChangeAt
	if lastChange.IsZero() {
		lastChange = repo.ingestedAt
	}
	age := now.Sub(lastChange)
	interval := w.interval
	switch {
	case age >= 7*24*time.Hour:
		interval = w.maxInterval
	case age >= 24*time.Hour:
		interval = 15 * time.Minute
	case age >= time.Hour:
		interval = 5 * time.Minute
	}
	if interval < w.interval {
		interval = w.interval
	}
	if interval > w.maxInterval {
		interval = w.maxInterval
	}
	return interval + jitter(interval/5)
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	delta := 0.2 * float64(d)
	return time.Duration((rand.Float64()*2 - 1) * delta)
}

func (w *autoRefreshWorker) repoFor(storageID string) *ingestedRepo {
	w.router.mu.RLock()
	repo := w.router.ingestedRepos[storageID]
	w.router.mu.RUnlock()
	return repo
}

func (w *autoRefreshWorker) scheduleAt(storageID string, due time.Time) {
	if due.IsZero() {
		due = time.Now()
	}
	w.scheduleMu.Lock()
	defer w.scheduleMu.Unlock()
	if entry, ok := w.entries[storageID]; ok {
		entry.due = due
		if entry.index >= 0 {
			heap.Fix(&w.queue, entry.index)
		}
		return
	}
	entry := &watchEntry{storageID: storageID, due: due}
	w.entries[storageID] = entry
	heap.Push(&w.queue, entry)
}

func (w *autoRefreshWorker) nextEligible(storageID string) time.Time {
	now := time.Now()
	w.mu.Lock()
	until := w.backoffUntil[storageID]
	w.mu.Unlock()
	if now.Before(until) {
		return until
	}
	return now.Add(w.interval)
}

func (w *autoRefreshWorker) eligible(storageID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active[storageID] {
		return false
	}
	if until, ok := w.backoffUntil[storageID]; ok && time.Now().Before(until) {
		return false
	}
	return true
}

func (w *autoRefreshWorker) markActive(storageID string) {
	w.mu.Lock()
	w.active[storageID] = true
	w.mu.Unlock()
}

func (w *autoRefreshWorker) clearActive(storageID string) {
	w.mu.Lock()
	delete(w.active, storageID)
	w.mu.Unlock()
}

// probeRepo probes one repository directly. It is used by tests and by the
// webhook path, where an immediate probe is preferable to waiting for the heap.
func (w *autoRefreshWorker) probeRepo(storageID string, repo *ingestedRepo, reason string) {
	if repo == nil || repo.repoURL == "" {
		return
	}
	probeStarted := time.Now()
	repo.markProbeStart(probeStarted)
	fetcherClient := w.router.getFetcherClient()
	if fetcherClient == nil {
		w.applyProbeError(storageID, "no fetcher client", time.Now())
		w.logger.Warn("auto-refresh skipped, no fetcher client", "storage_id", storageID)
		return
	}
	branch := repo.branch
	if branch == "" {
		branch = "main"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	results, err := fetcherClient.ProbeWorkspaceRefresh(ctx, &pb.ProbeWorkspaceRefreshRequest{
		JobId:       fmt.Sprintf("autorefresh-%d", time.Now().UnixNano()),
		WorkspaceId: "autorefresh",
		Repositories: []*pb.WorkspaceRepositoryRef{{
			StorageId:    storageID,
			DisplayPath:  repo.repoID,
			RepoUrl:      repo.repoURL,
			Branch:       branch,
			BaseCommit:   repo.commitHash,
			MetadataOnly: true,
		}},
	})
	if err != nil {
		w.applyProbeError(storageID, err.Error(), time.Now())
		w.logger.Warn("auto-refresh probe failed", "storage_id", storageID, "error", err)
		return
	}
	if len(results) == 0 {
		w.applyProbeError(storageID, "probe returned no result", time.Now())
		return
	}
	for _, progress := range results {
		w.applyResult(storageID, repo, progress.GetStatus(), progress.GetMessage(), reason, branch)
	}
}

func (w *autoRefreshWorker) incProbe(result string) {
	w.mu.Lock()
	w.probes[result]++
	w.mu.Unlock()
}

func (w *autoRefreshWorker) incReingest() {
	w.mu.Lock()
	w.reingests++
	w.mu.Unlock()
}

func (w *autoRefreshWorker) recordFailure(storageID string) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failureCount[storageID]++
	count := w.failureCount[storageID]
	backoff := time.Duration(1<<uint(minInt(count-1, 6))) * time.Minute
	if backoff > autoRefreshBackoffMax {
		backoff = autoRefreshBackoffMax
	}
	w.backoffUntil[storageID] = time.Now().Add(backoff)
	w.backoffs++
	routerAutoRefreshBackoffTotal.Inc()
	return backoff
}

func (w *autoRefreshWorker) resetBackoff(storageID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.failureCount, storageID)
	delete(w.backoffUntil, storageID)
}

func (w *autoRefreshWorker) probeStorageID(storageID string) bool {
	repo := w.repoFor(storageID)
	if repo == nil || strings.TrimSpace(repo.repoURL) == "" {
		return false
	}
	w.mu.Lock()
	if w.active[storageID] {
		w.mu.Unlock()
		return false
	}
	w.active[storageID] = true
	delete(w.backoffUntil, storageID)
	w.mu.Unlock()
	go func() {
		defer w.clearActive(storageID)
		w.probeRepo(storageID, repo, "manual")
	}()
	return true
}

// probeNow schedules an immediate probe for every ingested repo matching a
// webhook URL. It bypasses the regular interval but still uses the normal
// active/dedup guards.
func (w *autoRefreshWorker) probeNow(repoURL string) {
	w.router.mu.RLock()
	repos := make([]*ingestedRepo, 0)
	ids := make([]string, 0)
	for storageID, repo := range w.router.ingestedRepos {
		if repo != nil && repo.repoURL != "" && sameRepoURL(repo.repoURL, repoURL) {
			if !w.eligible(storageID) {
				continue
			}
			repos = append(repos, repo)
			ids = append(ids, storageID)
		}
	}
	w.router.mu.RUnlock()

	for i, repo := range repos {
		w.markActive(ids[i])
		go func(id string, rp *ingestedRepo) {
			defer w.clearActive(id)
			w.probeRepo(id, rp, "webhook")
		}(ids[i], repo)
	}
}

func (w *autoRefreshWorker) Stats() map[string]any {
	w.mu.Lock()
	active := len(w.active)
	lastScan := w.lastScan
	probes := make(map[string]int64, len(w.probes))
	for key, value := range w.probes {
		probes[key] = value
	}
	reingests := w.reingests
	backoffs := w.backoffs
	leaseHeld := w.leaseHeld
	w.mu.Unlock()

	w.scheduleMu.Lock()
	scheduled := len(w.entries)
	nextProbe := time.Time{}
	if len(w.queue) > 0 {
		nextProbe = w.queue[0].due
	}
	w.scheduleMu.Unlock()

	nextScan := time.Time{}
	if !lastScan.IsZero() {
		nextScan = lastScan.Add(autoRefreshSyncInterval)
	}
	snapshot := w.router.ingestedRepoSnapshot()
	repositories := make([]map[string]any, 0, len(snapshot))
	for storageID, repo := range snapshot {
		if repo == nil || strings.TrimSpace(repo.repoURL) == "" {
			continue
		}
		if repowatch.ShardFor(storageID, w.shards) != w.shard {
			continue
		}
		entry := repo.refreshJSON()
		entry["storage_id"] = storageID
		entry["repo_id"] = repo.repoID
		entry["repo_url"] = repo.repoURL
		entry["branch"] = repo.branch
		entry["commit_hash"] = repo.commitHash
		repositories = append(repositories, entry)
	}
	sort.Slice(repositories, func(i, j int) bool {
		return fmt.Sprint(repositories[i]["repo_id"]) < fmt.Sprint(repositories[j]["repo_id"])
	})

	return map[string]any{
		"enabled":      true,
		"interval":     w.interval.String(),
		"max_interval": w.maxInterval.String(),
		"concurrency":  w.concurrency,
		"batch_size":   w.batchSize,
		"shard":        w.shard,
		"shards":       w.shards,
		"lease_held":   leaseHeld,
		"tracked":      len(repositories),
		"scheduled":    scheduled,
		"active":       active,
		"last_scan":    unixOrZero(lastScan),
		"next_scan":    unixOrZero(nextScan),
		"next_probe":   unixOrZero(nextProbe),
		"probes":       probes,
		"reingests":    reingests,
		"backoffs":     backoffs,
		"repositories": repositories,
	}
}

func (w *autoRefreshWorker) statePath() string {
	if strings.TrimSpace(w.stateDir) == "" {
		return ""
	}
	return filepath.Join(w.stateDir, fmt.Sprintf("repowatch-%d.json", w.shard))
}

func (w *autoRefreshWorker) loadState() {
	path := w.statePath()
	if path == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return
	}
	var state watchStateFile
	if err := json.Unmarshal(raw, &state); err != nil {
		w.logger.Warn("auto-refresh state is invalid; ignoring", "path", path, "error", err)
		return
	}
	w.stateMu.Lock()
	for id, record := range state.Repos {
		w.records[id] = record
	}
	w.stateMu.Unlock()
	w.logger.Info("auto-refresh state loaded", "path", path, "repos", len(state.Repos))
}

func (w *autoRefreshWorker) saveState() {
	path := w.statePath()
	if path == "" {
		return
	}
	w.stateMu.Lock()
	records := make(map[string]watchRecord, len(w.records))
	for id, record := range w.records {
		records[id] = record
	}
	w.stateMu.Unlock()

	w.scheduleMu.Lock()
	now := time.Now()
	for id, entry := range w.entries {
		repo := w.repoFor(id)
		if repo == nil {
			continue
		}
		state := repo.refreshSnapshot()
		w.mu.Lock()
		failures := w.failureCount[id]
		backoff := w.backoffUntil[id]
		w.mu.Unlock()
		records[id] = watchRecord{
			StorageID:      id,
			RepoURL:        repo.repoURL,
			Branch:         repo.branch,
			CommitHash:     repo.commitHash,
			NextProbeAt:    entry.due,
			LastProbedAt:   state.lastProbedAt,
			LastStatus:     state.lastProbeStatus,
			LastError:      state.lastProbeError,
			LastChangeAt:   state.lastChangeAt,
			ChangesTotal:   state.changesTotal,
			ReingestsTotal: state.reingestsTotal,
			FailureCount:   failures,
			BackoffUntil:   backoff,
		}
	}
	w.scheduleMu.Unlock()

	state := watchStateFile{Shard: w.shard, Shards: w.shards, SavedAt: now, Repos: records}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(w.stateDir, 0o755); err != nil {
		w.logger.Warn("auto-refresh state dir", "error", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		w.logger.Warn("auto-refresh state write", "error", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		w.logger.Warn("auto-refresh state rename", "error", err)
	}
}

func (w *autoRefreshWorker) record(storageID string) (watchRecord, bool) {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	record, ok := w.records[storageID]
	return record, ok
}

// sameRepoURL compares two repository URLs, normalizing a trailing ".git".
func sameRepoURL(a, b string) bool {
	a = strings.TrimSuffix(strings.TrimSpace(a), ".git")
	b = strings.TrimSuffix(strings.TrimSpace(b), ".git")
	return a == b
}

// ingestedRepoSnapshot returns a snapshot of the tracked ingested repositories
// (storageID -> repo), for iteration outside the router lock.
func (r *Router) ingestedRepoSnapshot() map[string]*ingestedRepo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*ingestedRepo, len(r.ingestedRepos))
	for id, repo := range r.ingestedRepos {
		out[id] = repo
	}
	return out
}

// reingestRepoAsync re-ingests a repository via the existing ingest machinery
// (which bumps the native namespace generation so sessions observe the change
// and re-triggers search indexing). It deduplicates against the shared
// 60-second window and concurrent in-progress ingestions. Returns true when a
// re-ingest was scheduled.
func (r *Router) reingestRepoAsync(storageID, reason, branch string) bool {
	r.reingestDedupMu.Lock()
	if last, ok := r.reingestSeen[storageID]; ok && time.Since(last) < reingestDedupWindow {
		r.reingestDedupMu.Unlock()
		return false
	}
	r.reingestSeen[storageID] = time.Now()
	r.reingestDedupMu.Unlock()

	r.mu.RLock()
	repo := r.ingestedRepos[storageID]
	_, inFlight := r.inProgressIngestions[storageID]
	r.mu.RUnlock()
	if repo == nil || repo.repoURL == "" || inFlight {
		return false
	}
	if branch == "" {
		branch = repo.branch
	}
	if branch == "" {
		branch = "main"
	}
	go func() {
		err := r.IngestRepository(&pb.IngestRequest{
			Source:        repo.repoURL,
			Ref:           branch,
			SourceId:      repo.repoID,
			IngestionType: pb.IngestionType_INGESTION_GIT,
			FetchType:     pb.SourceType_SOURCE_TYPE_BLOB,
		}, &mockIngestStream{ctx: r.internalIdentityContext()})
		if err != nil {
			r.logger.Warn("auto-refresh re-ingest failed", "storage_id", storageID, "reason", reason, "error", err)
		} else {
			r.logger.Info("auto-refresh re-ingested", "storage_id", storageID, "reason", reason)
		}
	}()
	return true
}

// enqueueUpstreamRefresh handles a webhook signal that an upstream repository
// has moved. It maps the repository URL to the ingested repositories that
// track it and schedules an immediate probe/re-ingest (deduplicating within the
// shared window). When the auto-refresh worker is enabled it drives the probe;
// otherwise a direct re-ingest is scheduled.
func (r *Router) enqueueUpstreamRefresh(repoURL string) {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return
	}
	if len(r.autoRefreshWorkers) > 0 {
		for _, worker := range r.autoRefreshWorkers {
			worker.probeNow(repoURL)
		}
		return
	}
	if r.autoRefreshWorker != nil {
		r.autoRefreshWorker.probeNow(repoURL)
		return
	}
	r.mu.RLock()
	var matched []string
	for storageID, repo := range r.ingestedRepos {
		if repo != nil && repo.repoURL != "" && sameRepoURL(repo.repoURL, repoURL) {
			matched = append(matched, storageID)
		}
	}
	r.mu.RUnlock()
	for _, storageID := range matched {
		if r.reingestRepoAsync(storageID, "webhook", "") {
			routerAutoRefreshReingestsTotal.Inc()
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

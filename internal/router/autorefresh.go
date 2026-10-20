package router

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

const (
	defaultAutoRefreshInterval    = 5 * time.Minute
	defaultAutoRefreshConcurrency = 4
	minAutoRefreshInterval        = 30 * time.Second
	autoRefreshBackoffMax         = 30 * time.Minute
	reingestDedupWindow           = 60 * time.Second
)

// repoRefreshState is the per-repository trace kept by the auto-refresh
// worker. It is intentionally small: the durable commit hash lives on the node
// metadata, while these fields describe the watcher's last observation.
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

// autoRefreshWorker periodically probes ingested repositories' upstream heads
// and re-ingests those that have advanced. It is modeled on autoPushWorker.
type autoRefreshWorker struct {
	router      *Router
	logger      *slog.Logger
	interval    time.Duration
	concurrency int
	stop        chan struct{}

	mu           sync.Mutex
	active       map[string]bool
	failureCount map[string]int
	backoffUntil map[string]time.Time
	lastScan     time.Time
	probes       map[string]int64
	reingests    int64
	backoffs     int64
}

func newAutoRefreshWorker(r *Router, interval time.Duration, concurrency int, logger *slog.Logger) *autoRefreshWorker {
	if interval < minAutoRefreshInterval {
		interval = minAutoRefreshInterval
	}
	if concurrency <= 0 {
		concurrency = defaultAutoRefreshConcurrency
	}
	return &autoRefreshWorker{
		router:       r,
		logger:       logger.With("component", "autorefresh"),
		interval:     interval,
		concurrency:  concurrency,
		stop:         make(chan struct{}),
		active:       make(map[string]bool),
		failureCount: make(map[string]int),
		backoffUntil: make(map[string]time.Time),
		probes:       make(map[string]int64),
	}
}

func (w *autoRefreshWorker) Start() {
	w.logger.Info("auto-refresh worker started", "interval", w.interval, "concurrency", w.concurrency)
	go w.loop()
}

func (w *autoRefreshWorker) Stop() {
	close(w.stop)
	w.logger.Info("auto-refresh worker stopped")
}

func (w *autoRefreshWorker) loop() {
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

// scan probes every ingested repository (respecting backoff and in-flight
// state) and re-ingests those whose upstream has advanced.
func (w *autoRefreshWorker) scan() {
	w.mu.Lock()
	w.lastScan = time.Now()
	w.mu.Unlock()

	repos := w.router.ingestedRepoSnapshot()
	if len(repos) == 0 {
		return
	}

	sem := make(chan struct{}, w.concurrency)
	var wg sync.WaitGroup
	for storageID, repo := range repos {
		if !w.eligible(storageID) {
			continue
		}
		w.markActive(storageID)
		wg.Add(1)
		sem <- struct{}{}
		go func(id string, rp *ingestedRepo) {
			defer wg.Done()
			defer func() { <-sem }()
			defer w.clearActive(id)
			w.probeRepo(id, rp, "poll")
		}(storageID, repo)
	}
	wg.Wait()
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

// probeRepo probes a single repository's upstream head and re-ingests when it
// has advanced. reason is for logging ("poll" or "webhook").
func (w *autoRefreshWorker) probeRepo(storageID string, repo *ingestedRepo, reason string) {
	if repo == nil || repo.repoURL == "" {
		return
	}
	probeStarted := time.Now()
	repo.markProbeStart(probeStarted)
	fetcherClient := w.router.getFetcherClient()
	if fetcherClient == nil {
		backoff := w.recordFailure(storageID)
		repo.markProbeError("no fetcher client", time.Now(), time.Now().Add(backoff))
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
			StorageId:   storageID,
			DisplayPath: repo.repoID,
			RepoUrl:     repo.repoURL,
			Branch:      branch,
			BaseCommit:  repo.commitHash,
		}},
	})
	if err != nil {
		backoff := w.recordFailure(storageID)
		repo.markProbeError(err.Error(), time.Now(), time.Now().Add(backoff))
		w.incProbe("error")
		routerAutoRefreshProbesTotal.WithLabelValues("error").Inc()
		w.logger.Warn("auto-refresh probe failed", "storage_id", storageID, "error", err)
		return
	}

	for _, progress := range results {
		now := time.Now()
		next := now.Add(w.interval)
		switch progress.GetStatus() {
		case pb.RepoSyncStatus_REPO_SYNC_STATUS_ADVANCED:
			w.incProbe("advanced")
			routerAutoRefreshProbesTotal.WithLabelValues("advanced").Inc()
			w.resetBackoff(storageID)
			reingested := w.router.reingestRepoAsync(storageID, reason, branch)
			if reingested {
				routerAutoRefreshReingestsTotal.Inc()
				w.incReingest()
			}
			repo.markAdvance(now, next, reingested)
		case pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED:
			w.incProbe("unchanged")
			routerAutoRefreshProbesTotal.WithLabelValues("unchanged").Inc()
			w.resetBackoff(storageID)
			repo.markProbeResult("unchanged", now, next)
		case pb.RepoSyncStatus_REPO_SYNC_STATUS_DIVERGED, pb.RepoSyncStatus_REPO_SYNC_STATUS_MISSING_BRANCH:
			backoff := w.recordFailure(storageID)
			repo.markProbeError(progress.GetStatus().String(), now, now.Add(backoff))
			w.incProbe("diverged")
			routerAutoRefreshProbesTotal.WithLabelValues("diverged").Inc()
			w.logger.Warn("auto-refresh conflict detected, skipping re-ingest",
				"storage_id", storageID, "status", progress.GetStatus())
		default:
			backoff := w.recordFailure(storageID)
			repo.markProbeError(progress.GetStatus().String(), now, now.Add(backoff))
			w.incProbe("error")
			routerAutoRefreshProbesTotal.WithLabelValues("error").Inc()
		}
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
	backoff := time.Duration(1<<uint(min(count-1, 5))) * time.Minute
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

// probeStorageID queues an immediate probe for one repository, bypassing the
// normal interval/backoff so the UI "check now" action is useful.
func (w *autoRefreshWorker) probeStorageID(storageID string) bool {
	w.router.mu.RLock()
	repo := w.router.ingestedRepos[storageID]
	w.router.mu.RUnlock()
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

// Stats returns a point-in-time snapshot for the MonoFS worker UI/API.
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
	w.mu.Unlock()

	nextScan := time.Time{}
	if !lastScan.IsZero() {
		nextScan = lastScan.Add(w.interval)
	}

	snapshot := w.router.ingestedRepoSnapshot()
	repositories := make([]map[string]any, 0, len(snapshot))
	for storageID, repo := range snapshot {
		if repo == nil || strings.TrimSpace(repo.repoURL) == "" {
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
		"concurrency":  w.concurrency,
		"tracked":      len(repositories),
		"active":       active,
		"last_scan":    unixOrZero(lastScan),
		"next_scan":    unixOrZero(nextScan),
		"probes":       probes,
		"reingests":    reingests,
		"backoffs":     backoffs,
		"repositories": repositories,
	}
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

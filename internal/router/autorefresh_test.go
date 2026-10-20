package router

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/pkg/repowatch"
)

// invalidRepoURL uses the reserved .invalid TLD so any background ingest
// attempt fails DNS resolution immediately (no real network in tests).
const invalidRepoURL = "https://monofs.invalid/repo.git"

func setupAutoRefreshRouter(t *testing.T, responses []*pb.RepoSyncProgress) (*Router, *autoRefreshWorker) {
	t.Helper()
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	fetcherAddr, _, cleanup := startWorkspaceSyncTestFetcher(t, responses, nil, nil)
	t.Cleanup(cleanup)
	if err := r.SetFetcherClient([]string{fetcherAddr}); err != nil {
		t.Fatalf("set fetcher client: %v", err)
	}
	w := newAutoRefreshWorker(r, 5*time.Minute, 4, slog.New(slog.DiscardHandler))
	return r, w
}

func registerAutoRefreshRepo(t *testing.T, r *Router, storageID string) {
	t.Helper()
	r.mu.Lock()
	r.ingestedRepos[storageID] = &ingestedRepo{
		repoID:     "src/repo",
		repoURL:    invalidRepoURL,
		branch:     "main",
		commitHash: "abc123",
	}
	r.mu.Unlock()
}

func probeResponse(status pb.RepoSyncStatus) []*pb.RepoSyncProgress {
	return []*pb.RepoSyncProgress{{
		Repository: &pb.WorkspaceRepositoryRef{
			StorageId:   "repo-1",
			DisplayPath: "src/repo",
			RepoUrl:     invalidRepoURL,
			Branch:      "main",
			BaseCommit:  "abc123",
		},
		Status: status,
	}}
}

func TestAutoRefreshProbeAdvancedSchedulesReingest(t *testing.T) {
	r, w := setupAutoRefreshRouter(t, probeResponse(pb.RepoSyncStatus_REPO_SYNC_STATUS_ADVANCED))
	registerAutoRefreshRepo(t, r, "repo-1")

	repo := &ingestedRepo{repoID: "src/repo", repoURL: invalidRepoURL, branch: "main", commitHash: "abc123"}
	w.probeRepo("repo-1", repo, "test")

	r.reingestDedupMu.Lock()
	_, scheduled := r.reingestSeen["repo-1"]
	r.reingestDedupMu.Unlock()
	if !scheduled {
		t.Fatal("expected ADVANCED probe to schedule a re-ingest")
	}

	w.mu.Lock()
	_, backedOff := w.backoffUntil["repo-1"]
	w.mu.Unlock()
	if backedOff {
		t.Fatal("ADVANCED should not trigger backoff")
	}
	state := repo.refreshSnapshot()
	if state.lastProbeStatus != "advanced" || state.changesTotal != 1 || state.lastChangeAt.IsZero() {
		t.Fatalf("refresh state = %+v, want advanced change", state)
	}
}

func TestAutoRefreshProbeUnchangedDoesNothing(t *testing.T) {
	r, w := setupAutoRefreshRouter(t, probeResponse(pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED))
	registerAutoRefreshRepo(t, r, "repo-1")

	repo := &ingestedRepo{repoID: "src/repo", repoURL: invalidRepoURL, branch: "main", commitHash: "abc123"}
	w.probeRepo("repo-1", repo, "test")

	r.reingestDedupMu.Lock()
	_, scheduled := r.reingestSeen["repo-1"]
	r.reingestDedupMu.Unlock()
	if scheduled {
		t.Fatal("UNCHANGED should not schedule a re-ingest")
	}
	if state := repo.refreshSnapshot(); state.lastProbeStatus != "unchanged" {
		t.Fatalf("refresh status = %q, want unchanged", state.lastProbeStatus)
	}
}

func TestAutoRefreshProbeDivergedBacksOff(t *testing.T) {
	r, w := setupAutoRefreshRouter(t, probeResponse(pb.RepoSyncStatus_REPO_SYNC_STATUS_DIVERGED))
	registerAutoRefreshRepo(t, r, "repo-1")

	repo := &ingestedRepo{repoID: "src/repo", repoURL: invalidRepoURL, branch: "main", commitHash: "abc123"}
	w.probeRepo("repo-1", repo, "test")

	r.reingestDedupMu.Lock()
	_, scheduled := r.reingestSeen["repo-1"]
	r.reingestDedupMu.Unlock()
	if scheduled {
		t.Fatal("DIVERGED should not schedule a re-ingest")
	}

	w.mu.Lock()
	count := w.failureCount["repo-1"]
	until, backedOff := w.backoffUntil["repo-1"]
	w.mu.Unlock()
	if count != 1 || !backedOff {
		t.Fatalf("expected one failure and backoff, got count=%d backedOff=%v", count, backedOff)
	}
	if !until.After(time.Now()) {
		t.Fatal("backoff until should be in the future")
	}
	if state := repo.refreshSnapshot(); state.lastProbeStatus != "error" || state.lastProbeError == "" {
		t.Fatalf("refresh state = %+v, want error", state)
	}
}

func TestEnqueueUpstreamRefreshDedup(t *testing.T) {
	r, _ := setupAutoRefreshRouter(t, probeResponse(pb.RepoSyncStatus_REPO_SYNC_STATUS_ADVANCED))
	registerAutoRefreshRepo(t, r, "repo-1")

	// No worker: enqueueUpstreamRefresh falls back to direct re-ingest with dedup.
	r.enqueueUpstreamRefresh(invalidRepoURL)
	r.enqueueUpstreamRefresh(invalidRepoURL)

	// Give the (fast-failing) ingest goroutines a moment to attempt, then
	// assert only one re-ingest was scheduled for the repo.
	time.Sleep(20 * time.Millisecond)
	r.reingestDedupMu.Lock()
	seen := len(r.reingestSeen)
	r.reingestDedupMu.Unlock()
	if seen != 1 {
		t.Fatalf("expected exactly 1 dedup entry, got %d", seen)
	}
}

func TestSameRepoURL(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://github.com/o/r.git", "https://github.com/o/r", true},
		{"https://github.com/o/r.git", "https://github.com/o/r2", false},
	}
	for _, c := range cases {
		if got := sameRepoURL(c.a, c.b); got != c.want {
			t.Errorf("sameRepoURL(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestAutoRefreshStatsIncludesRepositoryTrace(t *testing.T) {
	r, w := setupAutoRefreshRouter(t, probeResponse(pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED))
	registerAutoRefreshRepo(t, r, "repo-1")

	r.mu.RLock()
	repo := r.ingestedRepos["repo-1"]
	r.mu.RUnlock()
	w.probeRepo("repo-1", repo, "test")

	stats := w.Stats()
	if stats["tracked"] != 1 {
		t.Fatalf("tracked = %v, want 1", stats["tracked"])
	}
	repos, ok := stats["repositories"].([]map[string]any)
	if !ok || len(repos) != 1 {
		t.Fatalf("repositories = %#v, want one entry", stats["repositories"])
	}
	if repos[0]["repo_id"] != "src/repo" || repos[0]["last_probe_status"] != "unchanged" {
		t.Fatalf("repository trace = %#v", repos[0])
	}
}

func TestAutoRefreshOwnerMatches(t *testing.T) {
	cases := []struct {
		owner, router string
		want          bool
	}{
		{"", "router-a", true},
		{"router-a", "router-a", true},
		{" router-a ", "router-a", true},
		{"router-a", "router-b", false},
	}
	for _, c := range cases {
		if got := autoRefreshOwnerMatches(c.owner, c.router); got != c.want {
			t.Errorf("autoRefreshOwnerMatches(%q,%q) = %v, want %v", c.owner, c.router, got, c.want)
		}
	}
}

func TestAutoRefreshShardFilter(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	for _, id := range []string{"repo-0", "repo-1", "repo-2", "repo-3"} {
		r.mu.Lock()
		r.ingestedRepos[id] = &ingestedRepo{repoID: "src/" + id, repoURL: invalidRepoURL, branch: "main", commitHash: "abc123"}
		r.mu.Unlock()
	}
	w := newAutoRefreshWorker(r, time.Minute, 2, slog.New(slog.DiscardHandler))
	w.configure(0, 2, 8, time.Hour, t.TempDir(), false, "", "")
	w.syncRepos()

	w.scheduleMu.Lock()
	defer w.scheduleMu.Unlock()
	for id := range w.entries {
		if repowatch.ShardFor(id, 2) != 0 {
			t.Fatalf("shard 0 owns repo %s in the wrong shard", id)
		}
	}
	if len(w.entries) == 0 || len(w.entries) == 4 {
		t.Fatalf("unexpected shard entry count: %d", len(w.entries))
	}
}

func TestAutoRefreshStateRoundTrip(t *testing.T) {
	stateDir := t.TempDir()
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	r.mu.Lock()
	r.ingestedRepos["repo-1"] = &ingestedRepo{
		repoID: "src/repo", repoURL: invalidRepoURL, branch: "main", commitHash: "abc123",
		refreshState: repoRefreshState{
			lastProbeStatus: "advanced",
			changesTotal:    3,
		},
	}
	r.mu.Unlock()

	w := newAutoRefreshWorker(r, time.Minute, 2, slog.New(slog.DiscardHandler))
	w.configure(0, 1, 8, time.Hour, stateDir, false, "", "")
	w.syncRepos()
	due := time.Now().Add(45 * time.Minute).Truncate(time.Second)
	w.scheduleMu.Lock()
	w.entries["repo-1"].due = due
	w.scheduleMu.Unlock()
	w.saveState()

	if _, err := os.Stat(filepath.Join(stateDir, "repowatch-0.json")); err != nil {
		t.Fatalf("state file: %v", err)
	}
	w2 := newAutoRefreshWorker(r, time.Minute, 2, slog.New(slog.DiscardHandler))
	w2.configure(0, 1, 8, time.Hour, stateDir, false, "", "")
	w2.loadState()
	w2.syncRepos()
	w2.scheduleMu.Lock()
	entry := w2.entries["repo-1"]
	w2.scheduleMu.Unlock()
	if entry == nil {
		t.Fatal("state did not restore the scheduled repo")
	}
	if entry.due.Sub(due).Abs() > time.Second {
		t.Fatalf("restored due = %v, want %v", entry.due, due)
	}
	state := r.ingestedRepos["repo-1"].refreshSnapshot()
	if state.changesTotal != 3 || state.lastProbeStatus != "advanced" {
		t.Fatalf("restored refresh state = %+v", state)
	}
}

func TestAutoRefreshProcessDueUsesMetadataOnly(t *testing.T) {
	responses := []*pb.RepoSyncProgress{{
		Repository: &pb.WorkspaceRepositoryRef{
			StorageId:   "repo-1",
			DisplayPath: "src/repo",
			RepoUrl:     invalidRepoURL,
			Branch:      "main",
			BaseCommit:  "abc123",
		},
		Status:       pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED,
		RemoteCommit: "abc123",
	}}
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	fetcherAddr, impl, cleanup := startWorkspaceSyncTestFetcher(t, responses, nil, nil)
	defer cleanup()
	if err := r.SetFetcherClient([]string{fetcherAddr}); err != nil {
		t.Fatalf("set fetcher: %v", err)
	}
	r.mu.Lock()
	r.ingestedRepos["repo-1"] = &ingestedRepo{repoID: "src/repo", repoURL: invalidRepoURL, branch: "main", commitHash: "abc123"}
	r.mu.Unlock()

	w := newAutoRefreshWorker(r, time.Minute, 2, slog.New(slog.DiscardHandler))
	w.configure(0, 1, 8, time.Hour, t.TempDir(), false, "", "")
	w.batchSem = make(chan struct{}, 1)
	w.syncRepos()
	w.scheduleMu.Lock()
	w.entries["repo-1"].due = time.Now().Add(-time.Second)
	w.scheduleMu.Unlock()
	w.processDue(context.Background())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		impl.mu.Lock()
		count := len(impl.probeRequests)
		var request *pb.ProbeWorkspaceRefreshRequest
		if count > 0 {
			request = impl.probeRequests[count-1]
		}
		impl.mu.Unlock()
		if request != nil {
			if len(request.Repositories) != 1 || !request.Repositories[0].GetMetadataOnly() {
				t.Fatalf("probe request = %+v, want metadata_only", request)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("probe request was not sent")
}

func TestAutoRefreshSchedulesTenThousandRepos(t *testing.T) {
	if testing.Short() {
		t.Skip("10k scheduler test skipped in -short mode")
	}
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	r.mu.Lock()
	for i := 0; i < 10000; i++ {
		id := fmt.Sprintf("repo-%05d", i)
		r.ingestedRepos[id] = &ingestedRepo{
			repoID:     "src/" + id,
			repoURL:    invalidRepoURL,
			branch:     "main",
			commitHash: "abc123",
		}
	}
	r.mu.Unlock()

	w := newAutoRefreshWorker(r, time.Minute, 8, slog.New(slog.DiscardHandler))
	w.configure(0, 4, 32, time.Hour, t.TempDir(), false, "", "")
	start := time.Now()
	w.syncRepos()
	elapsed := time.Since(start)
	w.scheduleMu.Lock()
	scheduled := len(w.entries)
	w.scheduleMu.Unlock()
	if scheduled < 2000 || scheduled > 3000 {
		t.Fatalf("shard 0 scheduled %d of 10k repos, want ~2500", scheduled)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("syncRepos took %s for 10k repos", elapsed)
	}
}

func TestAutoRefreshNextIntervalAdaptive(t *testing.T) {
	w := newAutoRefreshWorker(NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler)), time.Minute, 2, slog.New(slog.DiscardHandler))
	w.configure(0, 1, 8, time.Hour, t.TempDir(), false, "", "")
	now := time.Now()
	cases := []struct {
		age  time.Duration
		want time.Duration
	}{
		{10 * time.Minute, time.Minute},
		{2 * time.Hour, 5 * time.Minute},
		{2 * 24 * time.Hour, 15 * time.Minute},
		{8 * 24 * time.Hour, time.Hour},
	}
	for _, c := range cases {
		repo := &ingestedRepo{ingestedAt: now.Add(-c.age)}
		repo.refreshState.lastChangeAt = now.Add(-c.age)
		got := w.nextInterval(repo, now, false)
		if got < c.want*4/5 || got > c.want*6/5 {
			t.Errorf("age %s: interval = %s, want ~%s", c.age, got, c.want)
		}
	}
}

func TestAutoRefreshStatsTraceIsBounded(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	r.mu.Lock()
	for i := 0; i < autoRefreshTraceLimit+50; i++ {
		id := fmt.Sprintf("repo-%04d", i)
		r.ingestedRepos[id] = &ingestedRepo{repoID: "src/" + id, repoURL: invalidRepoURL, branch: "main", commitHash: "abc123"}
	}
	r.mu.Unlock()
	w := newAutoRefreshWorker(r, time.Minute, 2, slog.New(slog.DiscardHandler))
	w.configure(0, 1, 8, time.Hour, t.TempDir(), false, "", "")
	w.syncRepos()
	stats := w.Stats()
	if got := stats["tracked"].(int); got != autoRefreshTraceLimit+50 {
		t.Fatalf("tracked = %d", got)
	}
	repos := stats["repositories"].([]map[string]any)
	if len(repos) != autoRefreshTraceLimit {
		t.Fatalf("trace rows = %d, want %d", len(repos), autoRefreshTraceLimit)
	}
	if truncated, _ := stats["trace_truncated"].(bool); !truncated {
		t.Fatal("trace_truncated = false")
	}
}

func TestAutoRefreshNextIntervalUsesCommitTimeAfterRestart(t *testing.T) {
	w := newAutoRefreshWorker(NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler)), time.Minute, 2, slog.New(slog.DiscardHandler))
	w.configure(0, 1, 8, time.Hour, t.TempDir(), false, "", "")
	now := time.Now()
	repo := &ingestedRepo{ingestedAt: now, commitTime: now.Add(-8 * 24 * time.Hour)}
	got := w.nextInterval(repo, now, false)
	if got < 48*time.Minute || got > 72*time.Minute {
		t.Fatalf("interval = %s, want near max interval", got)
	}
}

func TestAutoRefreshSchedulerRunAndStop(t *testing.T) {
	responses := make([]*pb.RepoSyncProgress, 0, 20)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("repo-%02d", i)
		responses = append(responses, &pb.RepoSyncProgress{
			Repository: &pb.WorkspaceRepositoryRef{
				StorageId:   id,
				DisplayPath: "src/" + id,
				RepoUrl:     invalidRepoURL,
				Branch:      "main",
				BaseCommit:  "abc123",
			},
			Status:       pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED,
			RemoteCommit: "abc123",
		})
	}
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	fetcherAddr, impl, cleanup := startWorkspaceSyncTestFetcher(t, responses, nil, nil)
	defer cleanup()
	if err := r.SetFetcherClient([]string{fetcherAddr}); err != nil {
		t.Fatalf("set fetcher: %v", err)
	}
	r.mu.Lock()
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("repo-%02d", i)
		r.ingestedRepos[id] = &ingestedRepo{
			repoID: "src/" + id, repoURL: invalidRepoURL, branch: "main", commitHash: "abc123",
		}
	}
	r.mu.Unlock()

	w := newAutoRefreshWorker(r, 50*time.Millisecond, 2, slog.New(slog.DiscardHandler))
	w.configure(0, 1, 5, time.Second, t.TempDir(), false, "", "")
	w.Start()
	defer w.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		impl.mu.Lock()
		count := len(impl.probeRequests)
		impl.mu.Unlock()
		if count > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("scheduler did not send a probe")
}

func TestAutoRefreshStateDetectsCommitChange(t *testing.T) {
	stateDir := t.TempDir()
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	repo := &ingestedRepo{repoID: "src/repo", repoURL: invalidRepoURL, branch: "main", commitHash: "old"}
	r.mu.Lock()
	r.ingestedRepos["repo-1"] = repo
	r.mu.Unlock()

	w := newAutoRefreshWorker(r, time.Minute, 2, slog.New(slog.DiscardHandler))
	w.configure(0, 1, 8, time.Hour, stateDir, false, "", "")
	w.syncRepos()
	w.saveState()

	r.mu.Lock()
	r.ingestedRepos["repo-1"] = &ingestedRepo{repoID: "src/repo", repoURL: invalidRepoURL, branch: "main", commitHash: "new"}
	r.mu.Unlock()

	w2 := newAutoRefreshWorker(r, time.Minute, 2, slog.New(slog.DiscardHandler))
	w2.configure(0, 1, 8, time.Hour, stateDir, false, "", "")
	w2.loadState()
	w2.syncRepos()
	w2.scheduleMu.Lock()
	due := w2.entries["repo-1"].due
	w2.scheduleMu.Unlock()
	if time.Until(due) > time.Second {
		t.Fatalf("due = %v, want an immediate probe after commit change", due)
	}
}

func TestAutoRefreshReingestRequiresLease(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	repo := &ingestedRepo{repoID: "src/repo", repoURL: invalidRepoURL, branch: "main", commitHash: "abc123"}
	r.mu.Lock()
	r.ingestedRepos["repo-1"] = repo
	r.mu.Unlock()
	w := newAutoRefreshWorker(r, time.Minute, 2, slog.New(slog.DiscardHandler))
	w.configure(0, 1, 8, time.Hour, t.TempDir(), true, "test-lease", "default")
	w.leaseHeld = false
	w.applyResult("repo-1", repo, pb.RepoSyncStatus_REPO_SYNC_STATUS_ADVANCED, "", "poll", "main")
	r.reingestDedupMu.Lock()
	_, reingested := r.reingestSeen["repo-1"]
	r.reingestDedupMu.Unlock()
	if reingested {
		t.Fatal("standby worker enqueued a re-ingest")
	}

	w.leaseHeld = true
	w.applyResult("repo-1", repo, pb.RepoSyncStatus_REPO_SYNC_STATUS_ADVANCED, "", "poll", "main")
	r.reingestDedupMu.Lock()
	_, reingested = r.reingestSeen["repo-1"]
	r.reingestDedupMu.Unlock()
	if !reingested {
		t.Fatal("lease holder did not enqueue a re-ingest")
	}
}

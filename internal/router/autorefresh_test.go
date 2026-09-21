package router

import (
	"log/slog"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
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

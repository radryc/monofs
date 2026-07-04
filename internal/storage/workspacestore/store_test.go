package workspacestore

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "workspacestore-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestWALTruncatedLineRecovery(t *testing.T) {
	dir := tempDir(t)

	cfg := DefaultStoreConfig(dir)
	cfg.CompactionInterval = 24 * time.Hour
	cfg.CompactionSizeThreshold = 1 << 60

	store, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 10; i++ {
		job := &pb.WorkspaceSyncJob{
			JobId:         idFor(i),
			WorkspaceId:   "ws-test",
			State:         pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED,
			CreatedAtUnix: int64(i),
		}
		if err := store.UpsertJob(job); err != nil {
			t.Fatal(err)
		}
	}

	store.Close()

	badLine := []byte(`{"seq":11,"ts":"2026-07-03T10:00:00Z","op":"UPSERT","kind":"JOB","data":{`)
	segPath := filepath.Join(dir, "wal", "wal-000001.log")
	f, err := os.OpenFile(segPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(badLine)
	f.Close()

	cfg2 := DefaultStoreConfig(dir)
	cfg2.CompactionInterval = 24 * time.Hour
	cfg2.CompactionSizeThreshold = 1 << 60
	store2, err := New(cfg2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	if store2.JobCount() != 10 {
		t.Fatalf("expected 10 jobs after recovery, got %d", store2.JobCount())
	}

	store2.mu.RLock()
	nextSeq := store2.nextSeq
	store2.mu.RUnlock()
	if nextSeq != 11 {
		t.Fatalf("expected nextSeq=11, got %d", nextSeq)
	}

	job11 := &pb.WorkspaceSyncJob{
		JobId:         idFor(11),
		WorkspaceId:   "ws-test",
		State:         pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED,
		CreatedAtUnix: 11,
	}
	if err := store2.UpsertJob(job11); err != nil {
		t.Fatal(err)
	}
	if store2.JobCount() != 11 {
		t.Fatalf("expected 11 jobs, got %d", store2.JobCount())
	}
}

func TestContext(t *testing.T) {
	defer func() { testDir = "" }()
}

var testDir string

func TestMultiSegmentRecoveryWithCheckpoint(t *testing.T) {
	dir := tempDir(t)

	cfg := DefaultStoreConfig(dir)
	cfg.CompactionInterval = 24 * time.Hour
	cfg.CompactionSizeThreshold = 1
	cfg.FsyncEnabled = true

	store, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 200; i++ {
		job := &pb.WorkspaceSyncJob{
			JobId:         idFor(i),
			WorkspaceId:   "ws-test",
			State:         pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED,
			CreatedAtUnix: int64(i),
		}
		if err := store.UpsertJob(job); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.compact(); err != nil {
		t.Fatal(err)
	}

	store2, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	if store2.JobCount() != 200 {
		t.Fatalf("expected 200 jobs after recovery, got %d", store2.JobCount())
	}

	store2.mu.RLock()
	nextSeq := store2.nextSeq
	store2.mu.RUnlock()
	if nextSeq != 201 {
		t.Fatalf("expected nextSeq=201, got %d", nextSeq)
	}
}

func TestConcurrentWritersSerializedByMutex(t *testing.T) {
	dir := tempDir(t)

	cfg := DefaultStoreConfig(dir)
	cfg.CompactionInterval = 24 * time.Hour
	cfg.CompactionSizeThreshold = 1 << 60

	store, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const (
		numWriters  = 10
		entriesEach = 100
	)

	counter := &atomicCounter{}
	var wg sync.WaitGroup
	for w := 0; w < numWriters; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < entriesEach; j++ {
				n := counter.inc()
				job := &pb.WorkspaceSyncJob{
					JobId:         idFor(n),
					WorkspaceId:   "ws-test",
					State:         pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED,
					CreatedAtUnix: int64(n),
				}
				if err := store.UpsertJob(job); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if store.JobCount() != numWriters*entriesEach {
		t.Fatalf("expected %d jobs, got %d", numWriters*entriesEach, store.JobCount())
	}

	allJobs := store.ListJobs(nil)
	seen := make(map[string]bool)
	for _, j := range allJobs {
		if seen[j.GetJobId()] {
			t.Errorf("duplicate job %s", j.GetJobId())
		}
		seen[j.GetJobId()] = true
	}
	if len(seen) != numWriters*entriesEach {
		t.Errorf("expected %d unique jobs, got %d", numWriters*entriesEach, len(seen))
	}
}

func TestCompactionNoDoubleCountingOnReplay(t *testing.T) {
	dir := tempDir(t)

	cfg := DefaultStoreConfig(dir)
	cfg.CompactionInterval = 24 * time.Hour
	cfg.CompactionSizeThreshold = 1

	store, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 1000; i++ {
		job := &pb.WorkspaceSyncJob{
			JobId:         idFor(i),
			WorkspaceId:   "ws-test",
			State:         pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED,
			CreatedAtUnix: int64(i),
		}
		if err := store.UpsertJob(job); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.compact(); err != nil {
		t.Fatal(err)
	}

	for i := 501; i <= 1000; i++ {
		job := &pb.WorkspaceSyncJob{
			JobId:         idFor(i),
			WorkspaceId:   "ws-test",
			State:         pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED,
			CreatedAtUnix: int64(i),
		}
		if err := store.UpsertJob(job); err != nil {
			t.Fatal(err)
		}
	}

	store2, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	if store2.JobCount() != 1000 {
		t.Fatalf("expected 1000 jobs, got %d", store2.JobCount())
	}
}

func TestRecoveryFromSnapshotAfterCompaction(t *testing.T) {
	dir := tempDir(t)

	cfg := DefaultStoreConfig(dir)
	cfg.CompactionInterval = 24 * time.Hour
	cfg.CompactionSizeThreshold = 1
	cfg.FsyncEnabled = true

	store, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 20; i++ {
		if err := store.UpsertJob(&pb.WorkspaceSyncJob{
			JobId:         idFor(i),
			WorkspaceId:   "ws-snapshot",
			CreatedAtUnix: int64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.InsertLedger([]byte(`{"event":"one"}`)); err != nil {
		t.Fatal(err)
	}

	if err := store.compact(); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store2, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	if got := store2.JobCount(); got != 20 {
		t.Fatalf("expected 20 jobs after snapshot recovery, got %d", got)
	}

	ledger := make([]string, 0)
	if err := store2.ReplayLedgerEntries(func(data []byte) error {
		ledger = append(ledger, string(data))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 1 {
		t.Fatalf("expected 1 ledger entry after snapshot recovery, got %d", len(ledger))
	}
	if ledger[0] != `{"event":"one"}` {
		t.Fatalf("unexpected ledger entry: %s", ledger[0])
	}
}

func TestWALReplayIdempotent(t *testing.T) {
	dir := tempDir(t)

	cfg := DefaultStoreConfig(dir)
	cfg.CompactionInterval = 24 * time.Hour
	cfg.CompactionSizeThreshold = 1 << 60

	store, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 1000; i++ {
		job := &pb.WorkspaceSyncJob{
			JobId:         idFor(i),
			WorkspaceId:   "ws-test",
			CreatedAtUnix: int64(i),
		}
		if err := store.UpsertJob(job); err != nil {
			t.Fatal(err)
		}
	}

	originalJobs := make(map[string]*pb.WorkspaceSyncJob)
	for _, j := range store.ListJobs(nil) {
		originalJobs[j.GetJobId()] = j
	}
	store.Close()

	store2, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	jobs1 := make(map[string]string)
	for _, j := range store2.ListJobs(nil) {
		jobs1[j.GetJobId()] = j.GetJobId()
	}
	store2.Close()

	store3, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store3.Close()

	jobs2 := make(map[string]string)
	for _, j := range store3.ListJobs(nil) {
		jobs2[j.GetJobId()] = j.GetJobId()
	}

	for id := range jobs1 {
		if _, ok := jobs2[id]; !ok {
			t.Errorf("second replay missing job %s", id)
		}
	}

	if store3.JobCount() != 1000 {
		t.Fatalf("expected 1000 jobs, got %d", store3.JobCount())
	}
}

func TestProducerConsumerNoRace(t *testing.T) {
	dir := tempDir(t)

	cfg := DefaultStoreConfig(dir)
	cfg.CompactionInterval = 24 * time.Hour
	cfg.CompactionSizeThreshold = 1 << 60

	store, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for i := 1; i <= 10; i++ {
		store.UpsertJob(&pb.WorkspaceSyncJob{
			JobId:         idFor(i),
			WorkspaceId:   "ws-test",
			CreatedAtUnix: int64(i),
		})
	}

	var wg sync.WaitGroup
	done := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				jobs := store.ListJobs(nil)
				if len(jobs) < 10 {
					t.Error("reader saw fewer than 10 jobs")
				}
				ids := make(map[string]bool)
				for _, j := range jobs {
					ids[j.GetJobId()] = true
				}
				for i := 1; i <= 10; i++ {
					if !ids[idFor(i)] {
						t.Errorf("reader missing job %d", i)
					}
				}
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		counter := 11
		for {
			select {
			case <-done:
				return
			default:
				store.UpsertJob(&pb.WorkspaceSyncJob{
					JobId:         idFor(counter),
					WorkspaceId:   "ws-test",
					CreatedAtUnix: int64(counter),
				})
				counter++
			}
		}
	}()

	time.Sleep(2 * time.Second)
	close(done)
	wg.Wait()
}

func TestFullLifecycleWithCrashesAndCompaction(t *testing.T) {
	dir := tempDir(t)

	cfg := DefaultStoreConfig(dir)
	cfg.CompactionInterval = 24 * time.Hour
	cfg.CompactionSizeThreshold = 1 << 60

	store1, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 1000; i++ {
		store1.UpsertJob(&pb.WorkspaceSyncJob{
			JobId:         idFor(i),
			WorkspaceId:   "ws-lifecycle",
			CreatedAtUnix: int64(i),
		})
	}
	if err := store1.compact(); err != nil {
		t.Fatal(err)
	}
	for i := 501; i <= 1000; i++ {
		store1.UpsertJob(&pb.WorkspaceSyncJob{
			JobId:         idFor(i),
			WorkspaceId:   "ws-lifecycle",
			CreatedAtUnix: int64(i),
		})
	}
	store1.Close()

	store2, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if store2.JobCount() != 1000 {
		t.Fatalf("after first crash: expected 1000 jobs, got %d", store2.JobCount())
	}
	for i := 1001; i <= 1100; i++ {
		store2.UpsertJob(&pb.WorkspaceSyncJob{
			JobId:         idFor(i),
			WorkspaceId:   "ws-lifecycle",
			CreatedAtUnix: int64(i),
		})
	}
	if err := store2.compact(); err != nil {
		t.Fatal(err)
	}
	store2.Close()

	store3, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store3.Close()
	if store3.JobCount() != 1100 {
		t.Fatalf("after second crash: expected 1100 jobs, got %d", store3.JobCount())
	}
}

func TestListJobsFiltered(t *testing.T) {
	dir := tempDir(t)

	cfg := DefaultStoreConfig(dir)
	cfg.CompactionInterval = 24 * time.Hour
	cfg.CompactionSizeThreshold = 1 << 60

	store, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for i := 1; i <= 50; i++ {
		state := pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED
		if i%2 == 0 {
			state = pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_FAILED
		}
		store.UpsertJob(&pb.WorkspaceSyncJob{
			JobId:  idFor(i),
			State:  state,
			Action: pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_REFRESH,
		})
	}

	failedJobs := store.ListJobs(func(job *pb.WorkspaceSyncJob) bool {
		return job.GetState() == pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_FAILED
	})
	if len(failedJobs) != 25 {
		t.Fatalf("expected 25 failed jobs, got %d", len(failedJobs))
	}

	succeededJobs := store.ListJobs(func(job *pb.WorkspaceSyncJob) bool {
		return job.GetState() == pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED
	})
	if len(succeededJobs) != 25 {
		t.Fatalf("expected 25 succeeded jobs, got %d", len(succeededJobs))
	}
}

func TestNoJobsInMemoryOnlyMode(t *testing.T) {
	store, err := New(DefaultStoreConfig(""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.UpsertJob(&pb.WorkspaceSyncJob{
		JobId:       "test-job",
		WorkspaceId: "ws-test",
	})
	if store.JobCount() != 1 {
		t.Fatalf("expected 1 job in memory mode, got %d", store.JobCount())
	}
}

func TestBundleUpsertAndGet(t *testing.T) {
	dir := tempDir(t)

	cfg := DefaultStoreConfig(dir)
	cfg.CompactionInterval = 24 * time.Hour
	cfg.CompactionSizeThreshold = 1 << 60

	store, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	b := &BundleMetadata{
		BundleID:    "bundle-1",
		WorkspaceID: "ws-test",
		Kind:        "publish",
		ByteSize:    1024,
		RepoCount:   3,
	}
	if err := store.UpsertBundle(b); err != nil {
		t.Fatal(err)
	}

	got := store.GetBundle("bundle-1")
	if got == nil {
		t.Fatal("expected bundle not found")
	}
	if got.BundleID != "bundle-1" {
		t.Fatalf("expected bundle-1, got %s", got.BundleID)
	}
}

func TestAuditEventInsert(t *testing.T) {
	dir := tempDir(t)

	cfg := DefaultStoreConfig(dir)
	cfg.CompactionInterval = 24 * time.Hour
	cfg.CompactionSizeThreshold = 1 << 60

	store, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	event := &AuditEvent{
		WorkspaceID: "ws-1",
		JobID:       "job-1",
		Decision:    "allow",
		Timestamp:   time.Now().Unix(),
	}
	if err := store.InsertAudit(event); err != nil {
		t.Fatal(err)
	}

	events := store.ListAuditEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(events))
	}
	if events[0].Decision != "allow" {
		t.Fatalf("expected decision=allow, got %s", events[0].Decision)
	}
}

func TestAuditEventsSortedByTimestamp(t *testing.T) {
	dir := tempDir(t)

	cfg := DefaultStoreConfig(dir)
	cfg.CompactionInterval = 24 * time.Hour
	cfg.CompactionSizeThreshold = 1 << 60

	store, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	base := time.Now()
	events := []*AuditEvent{
		{WorkspaceID: "ws-1", JobID: "j1", Decision: "allow", Timestamp: base.Add(3 * time.Second).Unix()},
		{WorkspaceID: "ws-1", JobID: "j2", Decision: "deny", Timestamp: base.Add(1 * time.Second).Unix()},
		{WorkspaceID: "ws-1", JobID: "j3", Decision: "allow", Timestamp: base.Add(2 * time.Second).Unix()},
	}

	for _, e := range events {
		store.InsertAudit(e)
	}

	stored := store.ListAuditEvents()
	sort.Slice(stored, func(i, j int) bool { return stored[i].Timestamp < stored[j].Timestamp })

	if stored[0].JobID != "j2" {
		t.Errorf("expected j2 first, got %s", stored[0].JobID)
	}
	if stored[1].JobID != "j3" {
		t.Errorf("expected j3 second, got %s", stored[1].JobID)
	}
	if stored[2].JobID != "j1" {
		t.Errorf("expected j1 third, got %s", stored[2].JobID)
	}
}

func idFor(i int) string {
	return "job-" + string(rune('0'+i/100)) + string(rune('0'+(i/10)%10)) + string(rune('0'+i%10))
}

type atomicCounter struct {
	mu sync.Mutex
	n  int
}

func (c *atomicCounter) inc() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.n
}

package layout

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu           sync.Mutex
	ms           *MemoryManifestStore
	deleted      []string
	rewrites     [][2]any // {targetID, sourceIDs}
	stats        map[string]PackStat
	conflictOnce bool
	conflicts    int
}

func newFakeStore() *fakeStore {
	return &fakeStore{ms: NewMemoryManifestStore()}
}

func (f *fakeStore) seed(t *testing.T, shard string, packs []PackRef) {
	t.Helper()
	m := NewManifest(shard)
	for _, p := range packs {
		if err := m.AddPack(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.ms.CompareAndSwap(context.Background(), shard, "", m); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeStore) ReadManifest(ctx context.Context, shard string) (*Manifest, string, error) {
	return f.ms.Read(ctx, shard)
}

func (f *fakeStore) CompareAndSwap(ctx context.Context, shard, expected string, next *Manifest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conflictOnce && f.conflicts == 0 {
		f.conflicts++
		// Simulate another writer committing first.
		cur, etag, err := f.ms.Read(ctx, shard)
		if err == nil {
			cp := cur.Clone()
			cp.UpdatedUnix++
			_, _ = f.ms.CompareAndSwap(ctx, shard, etag, cp)
		}
		return "", ErrVersionConflict
	}
	return f.ms.CompareAndSwap(ctx, shard, expected, next)
}

func (f *fakeStore) ReadIndex(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (f *fakeStore) RewritePack(_ context.Context, _ string, targetID string, sources []PackRef) (PackRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(sources))
	var bytes, blobs int64
	for _, s := range sources {
		ids = append(ids, s.ID)
		bytes += s.Bytes
		blobs += s.Blobs
	}
	f.rewrites = append(f.rewrites, [2]any{targetID, ids})
	return PackRef{ID: targetID, Bytes: bytes, Blobs: blobs, CreatedUnix: 1, Sealed: true}, nil
}

func (f *fakeStore) DeletePack(_ context.Context, _ string, packID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, packID)
	return nil
}

func (f *fakeStore) PackStats(context.Context, string, []PackRef) (map[string]PackStat, error) {
	return f.stats, nil
}

func pack(id string, bytes int64, sealed bool) PackRef {
	return PackRef{ID: id, Bytes: bytes, Blobs: 1, CreatedUnix: 1, Sealed: sealed}
}

func TestPlanCompactionTooManyPacks(t *testing.T) {
	m := NewManifest("ab")
	_ = m.AddPack(pack("100-0000", 10, true))
	_ = m.AddPack(pack("200-0000", 20, true))
	_ = m.AddPack(pack("300-0000", 5, false))

	plan, ok := PlanCompaction(m, nil, CompactionConfig{MaxPacksPerShard: 2, DeadRatio: 0.3}, time.Now())
	if !ok {
		t.Fatal("expected a plan")
	}
	if len(plan.Sources) != 2 || plan.Sources[0].ID != "100-0000" || plan.Sources[1].ID != "200-0000" {
		t.Fatalf("unexpected sources: %+v", plan.Sources)
	}
	if plan.LiveBytes != 30 {
		t.Fatalf("live bytes = %d, want 30", plan.LiveBytes)
	}
	if plan.Drop {
		t.Fatal("unexpected drop")
	}
}

func TestPlanCompactionNoPlan(t *testing.T) {
	m := NewManifest("ab")
	_ = m.AddPack(pack("100-0000", 10, true))
	_ = m.AddPack(pack("200-0000", 5, false))
	if _, ok := PlanCompaction(m, nil, CompactionConfig{MaxPacksPerShard: 2, DeadRatio: 0.3}, time.Now()); ok {
		t.Fatal("unexpected plan")
	}
}

func TestPlanCompactionDeadHeavy(t *testing.T) {
	m := NewManifest("ab")
	_ = m.AddPack(pack("100-0000", 100, true))
	_ = m.AddPack(pack("200-0000", 5, false))
	stats := map[string]PackStat{
		"100-0000": {PackRef: pack("100-0000", 100, true), LiveBytes: 10},
	}
	plan, ok := PlanCompaction(m, stats, CompactionConfig{MaxPacksPerShard: 2, DeadRatio: 0.3}, time.Now())
	if !ok {
		t.Fatal("expected a plan")
	}
	if len(plan.Sources) != 1 || plan.Sources[0].ID != "100-0000" {
		t.Fatalf("unexpected sources: %+v", plan.Sources)
	}
	if plan.LiveBytes != 10 {
		t.Fatalf("live bytes = %d, want 10", plan.LiveBytes)
	}
}

func TestPlanCompactionDrop(t *testing.T) {
	m := NewManifest("ab")
	_ = m.AddPack(pack("100-0000", 100, true))
	_ = m.AddPack(pack("200-0000", 5, false))
	stats := map[string]PackStat{
		"100-0000": {PackRef: pack("100-0000", 100, true), LiveBytes: 0},
	}
	plan, ok := PlanCompaction(m, stats, CompactionConfig{MaxPacksPerShard: 2, DeadRatio: 0.3}, time.Now())
	if !ok || !plan.Drop {
		t.Fatalf("expected drop plan, got ok=%v plan=%+v", ok, plan)
	}
}

func TestCompactorRunShardMerges(t *testing.T) {
	store := newFakeStore()
	store.seed(t, "ab", []PackRef{
		pack("100-0000", 10, true),
		pack("200-0000", 20, true),
		pack("300-0000", 5, false),
	})
	clock := func() time.Time { return time.Unix(1000, 0) }
	c := NewCompactorWithClock(store, CompactionConfig{MaxPacksPerShard: 2}, clock)

	res, err := c.RunShard(context.Background(), "ab")
	if err != nil {
		t.Fatal(err)
	}
	if !res.PlanApplied {
		t.Fatalf("plan not applied: %+v", res)
	}
	if len(res.Retired) != 2 {
		t.Fatalf("retired = %v", res.Retired)
	}
	if len(store.rewrites) != 1 {
		t.Fatalf("rewrites = %v", store.rewrites)
	}

	m, _, err := store.ReadManifest(context.Background(), "ab")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Packs) != 2 {
		t.Fatalf("active packs = %d, want 2 (%+v)", len(m.Packs), m.Packs)
	}
	if _, ok := m.Find("300-0000"); !ok {
		t.Fatal("unsealed pack should remain active")
	}
	if len(m.Retired) != 2 {
		t.Fatalf("retired packs = %d, want 2", len(m.Retired))
	}
}

func TestCompactorDeletesExpiredRetired(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	now := time.Unix(10_000, 0)
	m := NewManifest("ab")
	_ = m.AddPack(pack("100-0000", 10, true))
	_ = m.AddPack(pack("200-0000", 5, false))
	m.Retire([]string{"100-0000"}, now.Add(-2*time.Hour))
	if _, err := store.ms.CompareAndSwap(ctx, "ab", "", m); err != nil {
		t.Fatal(err)
	}

	c := NewCompactorWithClock(store, CompactionConfig{GracePeriod: time.Hour}, func() time.Time { return now })
	res, err := c.RunShard(ctx, "ab")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != "100-0000" {
		t.Fatalf("deleted = %v", res.Deleted)
	}
	got, _, err := store.ReadManifest(ctx, "ab")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Retired) != 0 {
		t.Fatalf("retired not cleaned: %+v", got.Retired)
	}
}

func TestCompactorRetriesCASConflict(t *testing.T) {
	store := newFakeStore()
	store.conflictOnce = true
	store.seed(t, "ab", []PackRef{
		pack("100-0000", 10, true),
		pack("200-0000", 20, true),
		pack("300-0000", 5, false),
	})
	clock := func() time.Time { return time.Unix(1000, 0) }
	c := NewCompactorWithClock(store, CompactionConfig{MaxPacksPerShard: 2, RetryLimit: 3}, clock)

	res, err := c.RunShard(context.Background(), "ab")
	if err != nil {
		t.Fatalf("expected CAS retry to succeed, got %v", err)
	}
	if !res.PlanApplied {
		t.Fatalf("plan not applied after retry: %+v", res)
	}
	if store.conflicts != 1 {
		t.Fatalf("conflicts = %d, want 1", store.conflicts)
	}
}

func TestCompactorSkipsMissingManifest(t *testing.T) {
	c := NewCompactor(newFakeStore(), CompactionConfig{})
	res, err := c.RunShard(context.Background(), "ab")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped {
		t.Fatalf("expected skip, got %+v", res)
	}
}

func TestCompactorRunBounded(t *testing.T) {
	store := newFakeStore()
	store.seed(t, "00", []PackRef{pack("100-0000", 1, false)})
	c := NewCompactor(store, CompactionConfig{MaxShardsPerRun: 2})
	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.ShardsScanned != 2 {
		t.Fatalf("scanned = %d, want 2", res.ShardsScanned)
	}
}

func TestCompactorRejectsBadShard(t *testing.T) {
	c := NewCompactor(newFakeStore(), CompactionConfig{})
	if _, err := c.RunShard(context.Background(), "nope"); !errors.Is(err, ErrInvalidShard) {
		t.Fatalf("expected ErrInvalidShard, got %v", err)
	}
}

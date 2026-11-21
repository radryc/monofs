package layout

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// CompactionConfig controls when packs are merged and how aggressively retired
// packs are cleaned up. Zero values are replaced by DefaultCompactionConfig.
type CompactionConfig struct {
	// TargetBytes is the maximum live payload of a rewritten pack.
	TargetBytes int64
	// MaxPacksPerShard is the number of active packs kept before merging.
	MaxPacksPerShard int
	// DeadRatio triggers compaction of a pack when this fraction of its
	// bytes is no longer referenced.
	DeadRatio float64
	// GracePeriod keeps superseded packs readable before deletion.
	GracePeriod time.Duration
	// MaxShardsPerRun bounds work per compaction cycle.
	MaxShardsPerRun int
	// MaxBytesPerRun bounds bytes rewritten per cycle.
	MaxBytesPerRun int64
	// RetryLimit bounds manifest CAS retries.
	RetryLimit int
}

// DefaultCompactionConfig keeps at most two active packs per shard and rewrites
// at most 8 GiB per cycle.
func DefaultCompactionConfig() CompactionConfig {
	return CompactionConfig{
		TargetBytes:      512 * 1024 * 1024,
		MaxPacksPerShard: 2,
		DeadRatio:        0.30,
		GracePeriod:      time.Hour,
		MaxShardsPerRun:  ShardCount,
		MaxBytesPerRun:   8 * 1024 * 1024 * 1024,
		RetryLimit:       5,
	}
}

// PackStat is a pack plus how many of its bytes are still referenced.
type PackStat struct {
	PackRef
	// LiveBytes is the referenced payload. A negative value means unknown,
	// which is treated as fully live.
	LiveBytes int64
}

func (p PackStat) deadRatio() float64 {
	if p.Bytes <= 0 || p.LiveBytes < 0 {
		return 0
	}
	dead := p.Bytes - p.LiveBytes
	if dead <= 0 {
		return 0
	}
	return float64(dead) / float64(p.Bytes)
}

// Plan is a compaction decision for one shard.
type Plan struct {
	Shard     string
	Sources   []PackRef
	LiveBytes int64
	Reason    string
	// Drop is true when every source is fully dead and no target pack is needed.
	Drop bool
}

// PlanCompaction decides whether a shard needs compaction. It never selects
// the unsealed active pack, so writers can keep appending.
func PlanCompaction(m *Manifest, stats map[string]PackStat, cfg CompactionConfig, _ time.Time) (Plan, bool) {
	if m == nil {
		return Plan{}, false
	}
	if cfg.MaxPacksPerShard <= 0 {
		cfg.MaxPacksPerShard = DefaultCompactionConfig().MaxPacksPerShard
	}
	if cfg.DeadRatio <= 0 {
		cfg.DeadRatio = DefaultCompactionConfig().DeadRatio
	}

	var sealed []PackRef
	for _, p := range m.Packs {
		if p.Sealed {
			sealed = append(sealed, p)
		}
	}
	if len(sealed) == 0 {
		return Plan{}, false
	}

	var deadHeavy []PackRef
	var liveTotal int64
	for _, p := range sealed {
		st, ok := stats[p.ID]
		if !ok {
			st = PackStat{PackRef: p, LiveBytes: p.Bytes}
		}
		if st.LiveBytes < 0 {
			st.LiveBytes = p.Bytes
		}
		if st.deadRatio() >= cfg.DeadRatio {
			deadHeavy = append(deadHeavy, p)
		}
		liveTotal += st.LiveBytes
	}

	tooMany := len(m.Packs) > cfg.MaxPacksPerShard
	if !tooMany && len(deadHeavy) == 0 {
		return Plan{}, false
	}

	sources := sealed
	reason := fmt.Sprintf("%d active packs > %d", len(m.Packs), cfg.MaxPacksPerShard)
	if len(deadHeavy) > 0 {
		sources = deadHeavy
		reason = fmt.Sprintf("%d dead-heavy packs (dead ratio >= %.2f)", len(deadHeavy), cfg.DeadRatio)
	}

	// Recompute live bytes for the chosen sources.
	liveTotal = 0
	for _, p := range sources {
		st, ok := stats[p.ID]
		if !ok {
			st = PackStat{PackRef: p, LiveBytes: p.Bytes}
		}
		if st.LiveBytes < 0 {
			st.LiveBytes = p.Bytes
		}
		liveTotal += st.LiveBytes
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })

	return Plan{
		Shard:     m.Shard,
		Sources:   sources,
		LiveBytes: liveTotal,
		Reason:    reason,
		Drop:      liveTotal == 0,
	}, true
}

// Store is the object-store surface the compactor needs. RewritePack is the
// integration point with the packager: it reads the source packs and writes a
// new pack containing the same entries.
type Store interface {
	ReadManifest(ctx context.Context, shard string) (*Manifest, string, error)
	CompareAndSwap(ctx context.Context, shard, expectedETag string, next *Manifest) (string, error)
	// ReadIndex returns the digests stored in a pack (the sidecar contents).
	ReadIndex(ctx context.Context, shard, packID string) ([]string, error)
	// RewritePack writes targetID from sources and returns its PackRef.
	RewritePack(ctx context.Context, shard, targetID string, sources []PackRef) (PackRef, error)
	DeletePack(ctx context.Context, shard, packID string) error
}

// StatsProvider is implemented by stores that know which pack bytes are still
// referenced. Without it, the compactor treats every byte as live.
type StatsProvider interface {
	PackStats(ctx context.Context, shard string, packs []PackRef) (map[string]PackStat, error)
}

// RunResult summarizes one compaction cycle.
type RunResult struct {
	ShardsScanned  int
	PlansApplied   int
	PacksRetired   int
	PacksDeleted   int
	BytesRewritten int64
}

// ShardResult summarizes compaction of one shard.
type ShardResult struct {
	Shard          string
	Skipped        bool
	PlanApplied    bool
	Reason         string
	Retired        []string
	Deleted        []string
	BytesRewritten int64
}

// Compactor merges packs and reclaims retired packs, one bounded slice of
// shards at a time.
type Compactor struct {
	store  Store
	cfg    CompactionConfig
	now    func() time.Time
	cursor int
}

// NewCompactor returns a compactor with default config for unset fields.
func NewCompactor(store Store, cfg CompactionConfig) *Compactor {
	return NewCompactorWithClock(store, cfg, time.Now)
}

// NewCompactorWithClock returns a compactor with an injectable clock.
func NewCompactorWithClock(store Store, cfg CompactionConfig, now func() time.Time) *Compactor {
	def := DefaultCompactionConfig()
	if cfg.TargetBytes <= 0 {
		cfg.TargetBytes = def.TargetBytes
	}
	if cfg.MaxPacksPerShard <= 0 {
		cfg.MaxPacksPerShard = def.MaxPacksPerShard
	}
	if cfg.DeadRatio <= 0 {
		cfg.DeadRatio = def.DeadRatio
	}
	if cfg.GracePeriod < 0 {
		cfg.GracePeriod = 0
	}
	if cfg.MaxShardsPerRun <= 0 {
		cfg.MaxShardsPerRun = def.MaxShardsPerRun
	}
	if cfg.MaxBytesPerRun <= 0 {
		cfg.MaxBytesPerRun = def.MaxBytesPerRun
	}
	if cfg.RetryLimit <= 0 {
		cfg.RetryLimit = def.RetryLimit
	}
	if now == nil {
		now = time.Now
	}
	return &Compactor{store: store, cfg: cfg, now: now}
}

// Run compacts a bounded number of shards, resuming where the previous run
// stopped so every shard is eventually visited.
func (c *Compactor) Run(ctx context.Context) (RunResult, error) {
	var result RunResult
	if c.store == nil {
		return result, fmt.Errorf("layout: compactor has no store")
	}
	if c.cfg.MaxShardsPerRun > ShardCount {
		c.cfg.MaxShardsPerRun = ShardCount
	}
	for i := 0; i < c.cfg.MaxShardsPerRun; i++ {
		shard := fmt.Sprintf("%02x", c.cursor%ShardCount)
		c.cursor = (c.cursor + 1) % ShardCount
		result.ShardsScanned++

		sr, err := c.RunShard(ctx, shard)
		if err != nil {
			// A single bad shard must not abort the cycle.
			continue
		}
		if sr.PlanApplied {
			result.PlansApplied++
			result.PacksRetired += len(sr.Retired)
			result.BytesRewritten += sr.BytesRewritten
		}
		result.PacksDeleted += len(sr.Deleted)
		if result.BytesRewritten >= c.cfg.MaxBytesPerRun {
			break
		}
	}
	return result, nil
}

// RunShard compacts one shard. Retired-pack cleanup happens even when no merge
// is needed.
func (c *Compactor) RunShard(ctx context.Context, shard string) (ShardResult, error) {
	res := ShardResult{Shard: shard}
	if err := ValidateShard(shard); err != nil {
		return res, err
	}
	if c.store == nil {
		return res, fmt.Errorf("layout: compactor has no store")
	}
	now := c.now().UTC()

	m, etag, err := c.store.ReadManifest(ctx, shard)
	if err != nil {
		if errors.Is(err, ErrManifestNotFound) {
			res.Skipped = true
			return res, nil
		}
		return res, err
	}

	// Phase 1: delete retired packs whose grace period has elapsed.
	if ids := m.ExpiredRetired(now, c.cfg.GracePeriod); len(ids) > 0 {
		deleted := make([]string, 0, len(ids))
		for _, id := range ids {
			if err := c.store.DeletePack(ctx, shard, id); err != nil {
				continue
			}
			deleted = append(deleted, id)
		}
		if len(deleted) > 0 {
			next := m.Clone()
			for _, id := range deleted {
				next.RemoveRetired(id)
			}
			if _, err := c.store.CompareAndSwap(ctx, shard, etag, next); err == nil {
				res.Deleted = append(res.Deleted, deleted...)
				// Re-read so compaction sees the committed manifest.
				m, etag, err = c.store.ReadManifest(ctx, shard)
				if err != nil {
					return res, err
				}
			}
		}
	}

	// Phase 2: plan and apply a merge.
	stats, err := c.stats(ctx, shard, m.ActivePacks())
	if err != nil {
		return res, err
	}
	plan, ok := PlanCompaction(m, stats, c.cfg, now)
	if !ok {
		res.Skipped = true
		return res, nil
	}
	res.Reason = plan.Reason

	// Phase 3: commit. Retry on manifest conflicts.
	var ref PackRef
	if !plan.Drop {
		ref, err = c.store.RewritePack(ctx, shard, NewPackID(now, 0), plan.Sources)
		if err != nil {
			return res, err
		}
	}

	sourceIDs := make([]string, 0, len(plan.Sources))
	for _, p := range plan.Sources {
		sourceIDs = append(sourceIDs, p.ID)
	}

	for attempt := 0; attempt <= c.cfg.RetryLimit; attempt++ {
		cur, curETag, readErr := c.store.ReadManifest(ctx, shard)
		if readErr != nil {
			return res, readErr
		}
		next := cur.Clone()
		if !plan.Drop {
			if _, exists := next.Find(ref.ID); !exists {
				if err := next.AddPack(ref); err != nil {
					return res, err
				}
			}
			next.Seal(ref.ID)
		}
		retired := next.Retire(sourceIDs, now)
		_, casErr := c.store.CompareAndSwap(ctx, shard, curETag, next)
		if casErr == nil {
			res.PlanApplied = true
			res.Retired = retired
			res.BytesRewritten = plan.LiveBytes
			return res, nil
		}
		if !errors.Is(casErr, ErrVersionConflict) {
			return res, casErr
		}
	}
	return res, fmt.Errorf("layout: compaction of shard %s lost %d CAS races", shard, c.cfg.RetryLimit)
}

func (c *Compactor) stats(ctx context.Context, shard string, packs []PackRef) (map[string]PackStat, error) {
	out := make(map[string]PackStat, len(packs))
	for _, p := range packs {
		out[p.ID] = PackStat{PackRef: p, LiveBytes: p.Bytes}
	}
	if provider, ok := c.store.(StatsProvider); ok {
		got, err := provider.PackStats(ctx, shard, packs)
		if err != nil {
			return nil, err
		}
		for id, st := range got {
			out[id] = st
		}
	}
	return out, nil
}

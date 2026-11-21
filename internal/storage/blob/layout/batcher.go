package layout

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// PendingBlob is one blob waiting to be packed.
type PendingBlob struct {
	Digest string
	Size   int64
}

// Batch is a sealed group of blobs for one shard, ready to be written to a pack.
type Batch struct {
	Shard   string
	Blobs   []PendingBlob
	Bytes   int64
	Created time.Time
}

// BatchConfig controls when a builder is flushed.
type BatchConfig struct {
	// TargetBytes flushes a pack when it reaches this size.
	TargetBytes int64
	// MaxObjects flushes a pack when it holds this many blobs.
	MaxObjects int
	// FlushAfter seals a pack that has been open this long.
	FlushAfter time.Duration
	// MaxShards bounds how many shards may have an open builder at once.
	MaxShards int
}

// DefaultBatchConfig returns production defaults: large packs and few shards
// open at once, so the number of pack objects stays small.
func DefaultBatchConfig() BatchConfig {
	return BatchConfig{
		TargetBytes: 512 * 1024 * 1024,
		MaxObjects:  8192,
		FlushAfter:  2 * time.Second,
		MaxShards:   ShardCount,
	}
}

type shardBuilder struct {
	blobs   []PendingBlob
	bytes   int64
	created time.Time
}

// Batcher accumulates blobs per shard and decides when to seal them into a pack.
// It is safe for concurrent use.
type Batcher struct {
	cfg  BatchConfig
	now  func() time.Time
	mu   sync.Mutex
	open map[string]*shardBuilder
}

// NewBatcher returns a batcher using the wall clock.
func NewBatcher(cfg BatchConfig) *Batcher {
	return NewBatcherWithClock(cfg, time.Now)
}

// NewBatcherWithClock returns a batcher with an injectable clock (tests).
func NewBatcherWithClock(cfg BatchConfig, now func() time.Time) *Batcher {
	if now == nil {
		now = time.Now
	}
	if cfg.TargetBytes <= 0 {
		cfg.TargetBytes = DefaultBatchConfig().TargetBytes
	}
	if cfg.MaxObjects <= 0 {
		cfg.MaxObjects = DefaultBatchConfig().MaxObjects
	}
	if cfg.FlushAfter <= 0 {
		cfg.FlushAfter = DefaultBatchConfig().FlushAfter
	}
	if cfg.MaxShards <= 0 {
		cfg.MaxShards = ShardCount
	}
	return &Batcher{cfg: cfg, now: now, open: make(map[string]*shardBuilder)}
}

// Add registers a blob and returns any batches that became full. A blob larger
// than TargetBytes is sealed into its own batch immediately.
func (b *Batcher) Add(digest string, size int64) ([]Batch, error) {
	d, err := NormalizeDigest(digest)
	if err != nil {
		return nil, err
	}
	if size < 0 {
		size = 0
	}
	shard := d[:2]

	b.mu.Lock()
	defer b.mu.Unlock()

	// Oversized blob: flush the current builder first, then seal it alone.
	if size > b.cfg.TargetBytes {
		var out []Batch
		if cur, ok := b.open[shard]; ok {
			out = append(out, b.sealLocked(shard, cur))
		}
		out = append(out, Batch{
			Shard:   shard,
			Blobs:   []PendingBlob{{Digest: d, Size: size}},
			Bytes:   size,
			Created: b.now().UTC(),
		})
		return out, nil
	}

	cur, ok := b.open[shard]
	if !ok {
		cur = &shardBuilder{created: b.now().UTC()}
		b.open[shard] = cur
	}
	cur.blobs = append(cur.blobs, PendingBlob{Digest: d, Size: size})
	cur.bytes += size

	if cur.bytes >= b.cfg.TargetBytes || len(cur.blobs) >= b.cfg.MaxObjects {
		return []Batch{b.sealLocked(shard, cur)}, nil
	}

	// Bound the number of open shards: seal the oldest builder.
	if len(b.open) > b.cfg.MaxShards {
		oldestShard := ""
		var oldest time.Time
		for s, builder := range b.open {
			if oldestShard == "" || builder.created.Before(oldest) {
				oldestShard, oldest = s, builder.created
			}
		}
		if oldestShard != "" {
			return []Batch{b.sealLocked(oldestShard, b.open[oldestShard])}, nil
		}
	}
	return nil, nil
}

// Due seals builders older than FlushAfter.
func (b *Batcher) Due() []Batch {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now().UTC()
	var out []Batch
	for shard, builder := range b.open {
		if now.Sub(builder.created) >= b.cfg.FlushAfter {
			out = append(out, b.sealLocked(shard, builder))
		}
	}
	return out
}

// Take removes and returns the open builder for a shard.
func (b *Batcher) Take(shard string) (Batch, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	builder, ok := b.open[shard]
	if !ok {
		return Batch{}, false
	}
	return b.sealLocked(shard, builder), true
}

// FlushAll seals every open builder.
func (b *Batcher) FlushAll() []Batch {
	b.mu.Lock()
	defer b.mu.Unlock()
	shards := make([]string, 0, len(b.open))
	for s := range b.open {
		shards = append(shards, s)
	}
	sort.Strings(shards)
	out := make([]Batch, 0, len(shards))
	for _, s := range shards {
		out = append(out, b.sealLocked(s, b.open[s]))
	}
	return out
}

// OpenShards returns the shards with an open builder, sorted.
func (b *Batcher) OpenShards() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.open))
	for s := range b.open {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// PendingBytes returns the total bytes buffered across all builders.
func (b *Batcher) PendingBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	var total int64
	for _, builder := range b.open {
		total += builder.bytes
	}
	return total
}

func (b *Batcher) sealLocked(shard string, builder *shardBuilder) Batch {
	delete(b.open, shard)
	return Batch{
		Shard:   shard,
		Blobs:   append([]PendingBlob(nil), builder.blobs...),
		Bytes:   builder.bytes,
		Created: builder.created,
	}
}

// Validate checks that a batch is well formed before it is written.
func (batch Batch) Validate() error {
	if err := ValidateShard(batch.Shard); err != nil {
		return err
	}
	if len(batch.Blobs) == 0 {
		return fmt.Errorf("layout: empty batch for shard %s", batch.Shard)
	}
	var total int64
	for _, blob := range batch.Blobs {
		d, err := NormalizeDigest(blob.Digest)
		if err != nil {
			return err
		}
		if d[:2] != batch.Shard {
			return fmt.Errorf("layout: blob %s does not belong to shard %s", blob.Digest, batch.Shard)
		}
		total += blob.Size
	}
	if total != batch.Bytes {
		return fmt.Errorf("layout: batch bytes %d != sum of blobs %d", batch.Bytes, total)
	}
	return nil
}

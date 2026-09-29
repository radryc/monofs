// Package metastore provides the embedded key/value metadata store used by the
// storage nodes. It is backed by Pebble (an LSM with a sorted on-disk index and
// bounded caches), so the key space is not held in memory. "Buckets" are
// emulated with a per-bucket key prefix so callers keep the familiar
// Get/Put/Delete/prefix-scan API.
package metastore

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/cockroachdb/pebble"
)

// Sentinels mirror the previous store's errors so call sites read naturally.
var (
	// ErrKeyNotFound is returned by Get when the key does not exist.
	ErrKeyNotFound = errors.New("metastore: key not found")
	// ErrBucketNotFound is retained for compatibility; under prefix emulation a
	// missing bucket simply has no keys, so this is never returned.
	ErrBucketNotFound = errors.New("metastore: bucket not found")
	// ErrPrefixScan is retained for compatibility; scans over an empty range
	// return an empty result with a nil error.
	ErrPrefixScan = errors.New("metastore: prefix scan found nothing")
	// ErrNotFoundKey aliases ErrKeyNotFound.
	ErrNotFoundKey = ErrKeyNotFound
)

// Tx is the small transaction surface the server relies on. Reads see a
// consistent snapshot; writes are buffered and committed atomically by Update.
type Tx interface {
	Get(bucket string, key []byte) ([]byte, error)
	// Put stores value under key. ttl is accepted for API compatibility with the
	// previous store and is currently ignored.
	Put(bucket string, key, value []byte, ttl uint32) error
	Delete(bucket string, key []byte) error
	GetKeys(bucket string) ([][]byte, error)
	PrefixScanEntries(bucket string, prefix []byte, reg string, offset, limit int, includeKeys, includeValues bool) (keys, values [][]byte, err error)
}

// Options configures the store.
type Options struct {
	// Sync fsyncs each Update commit. Disable for faster (less durable) tests.
	Sync bool
	// CacheSize is the block cache size in bytes (default 512 MiB).
	CacheSize int64
	// MemTableSize is the write buffer size in bytes (default 64 MiB).
	MemTableSize uint64
}

// Store wraps a Pebble database.
type Store struct {
	db       *pebble.DB
	cache    *pebble.Cache
	sync     bool
	updateMu sync.Mutex

	prefixMu sync.RWMutex
	prefixes map[string]bucketPrefix
}

type bucketPrefix struct {
	lower []byte
	upper []byte
}

// Open opens (creating if needed) the store at dir.
func Open(dir string, opts Options) (*Store, error) {
	if opts.CacheSize <= 0 {
		opts.CacheSize = 512 << 20
	}
	if opts.MemTableSize == 0 {
		opts.MemTableSize = 64 << 20
	}
	cache := pebble.NewCache(opts.CacheSize)
	pOpts := &pebble.Options{
		Cache:                       cache,
		MemTableSize:                opts.MemTableSize,
		MemTableStopWritesThreshold: 4,
		L0CompactionThreshold:       4,
		MaxConcurrentCompactions:    func() int { return 2 },
	}
	db, err := pebble.Open(dir, pOpts)
	if err != nil {
		cache.Unref()
		return nil, fmt.Errorf("metastore: open %s: %w", dir, err)
	}
	return &Store{
		db:       db,
		cache:    cache,
		sync:     opts.Sync,
		prefixes: make(map[string]bucketPrefix),
	}, nil
}

// Close closes the store.
func (s *Store) Close() error {
	err := s.db.Close()
	s.cache.Unref()
	return err
}

// View runs fn against a consistent read snapshot.
func (s *Store) View(fn func(Tx) error) error {
	snap := s.db.NewSnapshot()
	defer snap.Close()
	return fn(&tx{store: s, snap: snap})
}

// Update runs fn against a buffered batch and commits it atomically if fn
// returns nil. Updates are serialized to preserve the previous store's
// single-writer semantics. Reads inside fn see the committed state plus this
// batch's pending writes; iteration sees committed state only (matching the
// previous store, whose scans did not observe uncommitted writes).
func (s *Store) Update(fn func(Tx) error) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()

	batch := s.db.NewIndexedBatch()
	snap := s.db.NewSnapshot()
	defer func() {
		batch.Close()
		snap.Close()
	}()

	if err := fn(&tx{store: s, snap: snap, batch: batch}); err != nil {
		return err
	}
	if s.sync {
		return batch.Commit(pebble.Sync)
	}
	return batch.Commit(pebble.NoSync)
}

// Bucket prefix helpers ------------------------------------------------------

func (s *Store) bucketRange(bucket string) bucketPrefix {
	s.prefixMu.RLock()
	bp, ok := s.prefixes[bucket]
	s.prefixMu.RUnlock()
	if ok {
		return bp
	}
	lower := make([]byte, 0, len(bucket)+1)
	lower = append(lower, bucket...)
	lower = append(lower, 0x00)
	bp = bucketPrefix{lower: lower, upper: prefixUpperBound(lower)}
	s.prefixMu.Lock()
	s.prefixes[bucket] = bp
	s.prefixMu.Unlock()
	return bp
}

// prefixUpperBound returns the smallest key greater than every key with the
// given prefix, or nil when the prefix is all 0xff (unbounded).
func prefixUpperBound(prefix []byte) []byte {
	end := make([]byte, len(prefix))
	copy(end, prefix)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

func (s *Store) fullKey(bucket string, key []byte) []byte {
	lower := s.bucketRange(bucket).lower
	out := make([]byte, 0, len(lower)+len(key))
	out = append(out, lower...)
	out = append(out, key...)
	return out
}

// tx implements Tx over a snapshot plus an optional batch.
type tx struct {
	store *Store
	snap  *pebble.Snapshot
	batch *pebble.Batch
}

func (t *tx) Get(bucket string, key []byte) ([]byte, error) {
	full := t.store.fullKey(bucket, key)
	var (
		val    []byte
		closer io.Closer
		err    error
	)
	if t.batch != nil {
		val, closer, err = t.batch.Get(full)
	} else {
		val, closer, err = t.snap.Get(full)
	}
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, ErrKeyNotFound
		}
		return nil, err
	}
	out := append([]byte(nil), val...)
	if closer != nil {
		_ = closer.Close()
	}
	return out, nil
}

func (t *tx) Put(bucket string, key, value []byte, _ uint32) error {
	if t.batch == nil {
		return errors.New("metastore: Put on read-only transaction")
	}
	return t.batch.Set(t.store.fullKey(bucket, key), value, pebble.NoSync)
}

func (t *tx) Delete(bucket string, key []byte) error {
	if t.batch == nil {
		return errors.New("metastore: Delete on read-only transaction")
	}
	return t.batch.Delete(t.store.fullKey(bucket, key), pebble.NoSync)
}

func (t *tx) GetKeys(bucket string) ([][]byte, error) {
	keys, _, err := t.PrefixScanEntries(bucket, nil, "", 0, -1, true, false)
	return keys, err
}

func (t *tx) PrefixScanEntries(bucket string, prefix []byte, reg string, offset, limit int, includeKeys, includeValues bool) (keys, values [][]byte, err error) {
	if reg != "" {
		return nil, nil, fmt.Errorf("metastore: regex prefix scan is not supported")
	}
	br := t.store.bucketRange(bucket)
	lower := br.lower
	upper := br.upper
	if len(prefix) > 0 {
		lower = make([]byte, 0, len(br.lower)+len(prefix))
		lower = append(lower, br.lower...)
		lower = append(lower, prefix...)
		upper = prefixUpperBound(lower)
	}

	iter, err := t.snap.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, nil, err
	}
	defer iter.Close()

	skipped := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		if offset > 0 && skipped < offset {
			skipped++
			continue
		}
		if includeKeys {
			// Return the full user key (bucket prefix stripped), matching the
			// previous store's scan semantics.
			k := iter.Key()
			userKey := k[len(br.lower):]
			keys = append(keys, append([]byte(nil), userKey...))
		}
		if includeValues {
			values = append(values, append([]byte(nil), iter.Value()...))
		}
		if limit > 0 {
			n := len(keys)
			if !includeKeys {
				n = len(values)
			}
			if n >= limit {
				break
			}
		}
	}
	return keys, values, nil
}

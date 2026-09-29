// Package metastore provides the embedded key/value metadata store used by the
// storage nodes. It is backed by Pebble (an LSM with a sorted on-disk index and
// bounded caches), so the key space is not held in memory. "Buckets" are
// emulated with a per-bucket key prefix so callers keep the familiar
// Get/Put/Delete/prefix-scan API.
package metastore

import (
	"bytes"
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
	// ErrStopIteration can be returned by a ScanPrefix callback to stop the
	// scan early without it being treated as an error.
	ErrStopIteration = errors.New("metastore: stop iteration")
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
	// ScanPrefix iterates keys in bucket with the given prefix, invoking fn
	// with the full user key and value. Returning ErrStopIteration from fn
	// stops iteration and yields a nil error.
	ScanPrefix(bucket string, prefix []byte, fn func(key, value []byte) error) error
	// ScanRange iterates keys in bucket within [lower, upper) user-key bounds
	// (nil = unbounded). Returning ErrStopIteration stops the scan.
	ScanRange(bucket string, lower, upper []byte, fn func(key, value []byte) error) error
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

func (t *tx) scanBounds(bucket string, lower, upper []byte) (fullLower, fullUpper []byte) {
	br := t.store.bucketRange(bucket)
	fullLower = br.lower
	if lower != nil {
		fullLower = make([]byte, 0, len(br.lower)+len(lower))
		fullLower = append(fullLower, br.lower...)
		fullLower = append(fullLower, lower...)
	}
	fullUpper = br.upper
	if upper != nil {
		fullUpper = make([]byte, 0, len(br.lower)+len(upper))
		fullUpper = append(fullUpper, br.lower...)
		fullUpper = append(fullUpper, upper...)
	}
	return fullLower, fullUpper
}

func (t *tx) ScanPrefix(bucket string, prefix []byte, fn func(key, value []byte) error) error {
	if len(prefix) == 0 {
		return t.ScanRange(bucket, nil, nil, fn)
	}
	return t.ScanRange(bucket, prefix, prefixUpperBound(prefix), fn)
}

// ScanRange iterates [lower, upper) user-key bounds, merging the committed
// snapshot with this transaction's pending batch writes (batch wins; an empty
// batch value is a deletion).
func (t *tx) ScanRange(bucket string, lower, upper []byte, fn func(key, value []byte) error) error {
	fullLower, fullUpper := t.scanBounds(bucket, lower, upper)
	iterOpts := &pebble.IterOptions{LowerBound: fullLower, UpperBound: fullUpper}
	// Callbacks receive bucket-relative (user) keys.
	prefixLen := len(t.store.bucketRange(bucket).lower)
	emit := func(key, value []byte) error {
		return fn(key[prefixLen:], value)
	}

	snapIter, err := t.snap.NewIter(iterOpts)
	if err != nil {
		return err
	}
	defer snapIter.Close()

	if t.batch == nil {
		for ok := snapIter.First(); ok; ok = snapIter.Next() {
			if err := emit(snapIter.Key(), snapIter.Value()); err != nil {
				if errors.Is(err, ErrStopIteration) {
					return nil
				}
				return err
			}
		}
		return nil
	}

	// Merge the committed snapshot with the batch so scans observe this
	// transaction's pending writes and deletes. Batch entries win; a batch
	// entry with an empty value is a deletion.
	batchIter, err := t.batch.NewIter(iterOpts)
	if err != nil {
		return err
	}
	defer batchIter.Close()

	sv := snapIter.First()
	bv := batchIter.First()
	for sv || bv {
		cmp := 1
		switch {
		case !sv:
			cmp = 1
		case !bv:
			cmp = -1
		default:
			cmp = bytes.Compare(snapIter.Key(), batchIter.Key())
		}
		if cmp < 0 {
			if err := emit(snapIter.Key(), snapIter.Value()); err != nil {
				if errors.Is(err, ErrStopIteration) {
					return nil
				}
				return err
			}
			sv = snapIter.Next()
			continue
		}
		if len(batchIter.Value()) > 0 {
			if err := emit(batchIter.Key(), batchIter.Value()); err != nil {
				if errors.Is(err, ErrStopIteration) {
					return nil
				}
				return err
			}
		}
		bv = batchIter.Next()
		if cmp == 0 {
			sv = snapIter.Next()
		}
	}
	return nil
}

func (t *tx) PrefixScanEntries(bucket string, prefix []byte, reg string, offset, limit int, includeKeys, includeValues bool) (keys, values [][]byte, err error) {
	if reg != "" {
		return nil, nil, fmt.Errorf("metastore: regex prefix scan is not supported")
	}
	skipped := 0
	err = t.ScanPrefix(bucket, prefix, func(key, value []byte) error {
		if offset > 0 && skipped < offset {
			skipped++
			return nil
		}
		if includeKeys {
			// ScanPrefix delivers bucket-relative user keys.
			keys = append(keys, append([]byte(nil), key...))
		}
		if includeValues {
			values = append(values, append([]byte(nil), value...))
		}
		n := len(keys)
		if !includeKeys {
			n = len(values)
		}
		if limit > 0 && n >= limit {
			return ErrStopIteration
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return keys, values, nil
}

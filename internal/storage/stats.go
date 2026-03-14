package storage

import (
	"sync/atomic"
	"time"
)

// AtomicStats provides atomic operations for BackendStats using atomic.Pointer
// and CompareAndSwap loops for lock-free updates.
type AtomicStats struct {
	ptr atomic.Pointer[BackendStats]
}

// NewAtomicStats creates a new AtomicStats with zeroed stats.
func NewAtomicStats() *AtomicStats {
	s := &AtomicStats{}
	s.ptr.Store(&BackendStats{})
	return s
}

// RecordSuccess records a successful fetch operation with duration and bytes.
func (s *AtomicStats) RecordSuccess(duration time.Duration, bytes int64) {
	latencyMs := duration.Milliseconds()
	for {
		old := s.ptr.Load()
		newStats := &BackendStats{
			Requests:     old.Requests + 1,
			Errors:       old.Errors,
			BytesFetched: old.BytesFetched + bytes,
			CacheHits:    old.CacheHits,
			CacheMisses:  old.CacheMisses,
			CachedItems:  old.CachedItems,
			CacheBytes:   old.CacheBytes,
			AvgLatencyMs: (old.AvgLatencyMs*float64(old.Requests) + float64(latencyMs)) / float64(old.Requests+1),
		}
		if s.ptr.CompareAndSwap(old, newStats) {
			return
		}
	}
}

// RecordFetchHit records a cache hit (successful fetch from cache).
func (s *AtomicStats) RecordFetchHit() {
	for {
		old := s.ptr.Load()
		newStats := &BackendStats{
			Requests:     old.Requests + 1,
			Errors:       old.Errors,
			BytesFetched: old.BytesFetched,
			CacheHits:    old.CacheHits + 1,
			CacheMisses:  old.CacheMisses,
			CachedItems:  old.CachedItems,
			CacheBytes:   old.CacheBytes,
			AvgLatencyMs: old.AvgLatencyMs,
		}
		if s.ptr.CompareAndSwap(old, newStats) {
			return
		}
	}
}

// RecordFetchMiss records a cache miss.
func (s *AtomicStats) RecordFetchMiss() {
	for {
		old := s.ptr.Load()
		newStats := &BackendStats{
			Requests:     old.Requests,
			Errors:       old.Errors,
			BytesFetched: old.BytesFetched,
			CacheHits:    old.CacheHits,
			CacheMisses:  old.CacheMisses + 1,
			CachedItems:  old.CachedItems,
			CacheBytes:   old.CacheBytes,
			AvgLatencyMs: old.AvgLatencyMs,
		}
		if s.ptr.CompareAndSwap(old, newStats) {
			return
		}
	}
}

// RecordError records a failed request.
func (s *AtomicStats) RecordError() {
	for {
		old := s.ptr.Load()
		newStats := &BackendStats{
			Requests:     old.Requests + 1,
			Errors:       old.Errors + 1,
			BytesFetched: old.BytesFetched,
			CacheHits:    old.CacheHits,
			CacheMisses:  old.CacheMisses,
			CachedItems:  old.CachedItems,
			CacheBytes:   old.CacheBytes,
			AvgLatencyMs: old.AvgLatencyMs,
		}
		if s.ptr.CompareAndSwap(old, newStats) {
			return
		}
	}
}

// RecordNotFound records a "not found" error (counts as error + cache miss).
func (s *AtomicStats) RecordNotFound() {
	for {
		old := s.ptr.Load()
		newStats := &BackendStats{
			Requests:     old.Requests + 1,
			Errors:       old.Errors + 1,
			BytesFetched: old.BytesFetched,
			CacheHits:    old.CacheHits,
			CacheMisses:  old.CacheMisses + 1,
			CachedItems:  old.CachedItems,
			CacheBytes:   old.CacheBytes,
			AvgLatencyMs: old.AvgLatencyMs,
		}
		if s.ptr.CompareAndSwap(old, newStats) {
			return
		}
	}
}

// GetStats returns a copy of the current stats.
func (s *AtomicStats) GetStats() BackendStats {
	return *s.ptr.Load()
}

// Store directly stores a new stats value (useful for initialization).
func (s *AtomicStats) Store(stats *BackendStats) {
	s.ptr.Store(stats)
}

// Load returns the current stats pointer (for advanced use cases).
func (s *AtomicStats) Load() *BackendStats {
	return s.ptr.Load()
}

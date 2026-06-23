package registry

import (
	"sync/atomic"
)

type Stats struct {
	Pulls        atomic.Int64
	Pushes       atomic.Int64
	CacheHits    atomic.Int64
	CacheMisses  atomic.Int64
	BytesServed  atomic.Int64
	BytesFetched atomic.Int64
	BytesStored  atomic.Int64
	BlobCount    atomic.Int64
}

func (s *Stats) Snapshot() StatsSnapshot {
	return StatsSnapshot{
		Pulls:        s.Pulls.Load(),
		Pushes:       s.Pushes.Load(),
		CacheHits:    s.CacheHits.Load(),
		CacheMisses:  s.CacheMisses.Load(),
		BytesServed:  s.BytesServed.Load(),
		BytesFetched: s.BytesFetched.Load(),
		BytesStored:  s.BytesStored.Load(),
		BlobCount:    s.BlobCount.Load(),
	}
}

type StatsSnapshot struct {
	Pulls        int64 `json:"pulls"`
	Pushes       int64 `json:"pushes"`
	CacheHits    int64 `json:"cache_hits"`
	CacheMisses  int64 `json:"cache_misses"`
	BytesServed  int64 `json:"bytes_served"`
	BytesFetched int64 `json:"bytes_fetched"`
	BytesStored  int64 `json:"bytes_stored"`
	BlobCount    int64 `json:"blob_count"`
}

type RepoStat struct {
	Name     string   `json:"name"`
	Tags     []string `json:"tags"`
	BlobSize int64    `json:"blob_size_bytes"`
}

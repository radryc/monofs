package logengine

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

type testReadSeekCloser struct {
	*strings.Reader
}

func (r testReadSeekCloser) Close() error {
	return nil
}

type stubObjectStore struct {
	reads map[string]string
}

func (s *stubObjectStore) Write(_ context.Context, _ string, _ io.Reader) error {
	return nil
}

func (s *stubObjectStore) Read(_ context.Context, path string) (io.ReadSeekCloser, error) {
	content, ok := s.reads[path]
	if !ok {
		return nil, ErrGhostChunk
	}
	return testReadSeekCloser{Reader: strings.NewReader(content)}, nil
}

func (s *stubObjectStore) ListChunks(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func TestCachedStoreReadManifestPrunesExpiredEntries(t *testing.T) {
	store := NewCachedStore(&stubObjectStore{reads: map[string]string{
		"chunks/metrics/fresh/metadata.json": "{}",
	}}, t.TempDir())
	now := time.Now()

	store.mu.Lock()
	store.chunks["chunks/metrics"] = chunkListCacheEntry{
		chunkIDs:  []string{"stale"},
		expiresAt: now.Add(-time.Minute),
	}
	store.manifests["chunks/metrics/stale/metadata.json"] = manifestCacheEntry{
		manifest:  ChunkManifest{ChunkID: "stale"},
		expiresAt: now.Add(-time.Minute),
	}
	store.nextSweep = time.Time{}
	store.mu.Unlock()

	if _, err := store.ReadManifest(context.Background(), SignalMetrics, "fresh"); err != nil {
		t.Fatalf("ReadManifest() error = %v", err)
	}

	store.mu.RLock()
	_, staleChunkExists := store.chunks["chunks/metrics"]
	_, staleManifestExists := store.manifests["chunks/metrics/stale/metadata.json"]
	_, freshManifestExists := store.manifests["chunks/metrics/fresh/metadata.json"]
	store.mu.RUnlock()

	if staleChunkExists {
		t.Fatalf("stale chunk cache entry was not pruned")
	}
	if staleManifestExists {
		t.Fatalf("stale manifest cache entry was not pruned")
	}
	if !freshManifestExists {
		t.Fatalf("fresh manifest cache entry was not stored")
	}
}
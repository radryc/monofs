package logengine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

func TestTimePrefixedChunkIDRoundTrip(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Minute)
	encoded := FormatTimePrefixedChunkID(t0, t1, "chunk-abc")
	minT, maxT, ok := ParseChunkTimeRangeFromID(encoded)
	if !ok {
		t.Fatalf("expected ParseChunkTimeRangeFromID(%q) to succeed", encoded)
	}
	if !minT.Equal(t0) || !maxT.Equal(t1) {
		t.Fatalf("parsed range = [%v, %v], want [%v, %v]", minT, maxT, t0, t1)
	}
}

func TestParseNativeChunkTimeRange(t *testing.T) {
	minT := time.Date(2026, 10, 23, 12, 0, 0, 123, time.UTC)
	maxT := minT.Add(5 * time.Minute)
	id := fmt.Sprintf("chunk-%d-%d", minT.UnixNano(), maxT.UnixNano())
	gotMin, gotMax, ok := ParseChunkTimeRangeFromID(id)
	if !ok {
		t.Fatalf("expected ParseChunkTimeRangeFromID(%q) to succeed", id)
	}
	if !gotMin.Equal(minT) || !gotMax.Equal(maxT) {
		t.Fatalf("parsed range = [%v, %v], want [%v, %v]", gotMin, gotMax, minT, maxT)
	}
	for _, invalid := range []string{"chunk-abc", "chunk-1", "chunk-2-1", "chunk-1-2-3"} {
		if _, _, ok := ParseChunkTimeRangeFromID(invalid); ok {
			t.Fatalf("expected ParseChunkTimeRangeFromID(%q) to fail", invalid)
		}
	}
}

func TestParseCompactedChunkTimeRange(t *testing.T) {
	minT := time.Date(2026, 10, 23, 12, 0, 0, 123, time.UTC)
	maxT := minT.Add(5 * time.Minute)
	id := fmt.Sprintf("compact-%d-%d-7", minT.UnixNano(), maxT.UnixNano())
	gotMin, gotMax, ok := ParseChunkTimeRangeFromID("2026/10/23/12/" + id)
	if !ok {
		t.Fatalf("expected ParseChunkTimeRangeFromID(%q) to succeed", id)
	}
	if !gotMin.Equal(minT) || !gotMax.Equal(maxT) {
		t.Fatalf("parsed range = [%v, %v], want [%v, %v]", gotMin, gotMax, minT, maxT)
	}
}

func TestCachedStoreEvictsOldestLocalIndexDirs(t *testing.T) {
	localDir := t.TempDir()
	store := NewCachedStore(&stubObjectStore{}, localDir)
	now := time.Now()

	oldestDir := filepath.Join(localDir, "idx-0")
	for i := 0; i <= maxLocalIndexDirs; i++ {
		dir := filepath.Join(localDir, fmt.Sprintf("idx-%d", i))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", dir, err)
		}
		store.touchLocalIndex(dir, now.Add(time.Duration(i)*time.Second))
	}

	if _, err := os.Stat(oldestDir); !os.IsNotExist(err) {
		t.Fatalf("expected oldest local index directory %q to be evicted, stat err = %v", oldestDir, err)
	}
}

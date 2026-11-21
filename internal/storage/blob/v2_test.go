package blob

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/radryc/monofs/internal/storage"
	"github.com/radryc/monofs/internal/storage/blob/layout"
)

type memV2Store struct {
	mu           sync.Mutex
	data         map[string][]byte
	etags        map[string]string
	seq          int
	conflictOnce bool
}

func newMemV2Store() *memV2Store {
	return &memV2Store{data: make(map[string][]byte), etags: make(map[string]string)}
}

func (m *memV2Store) Get(_ context.Context, key string) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.data[key]
	if !ok {
		return nil, "", os.ErrNotExist
	}
	return append([]byte(nil), data...), m.etags[key], nil
}

func (m *memV2Store) Put(_ context.Context, key string, data []byte, ifMatch, ifNoneMatch string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conflictOnce && strings.HasSuffix(key, layout.ManifestName) {
		m.conflictOnce = false
		return "", layout.ErrVersionConflict
	}
	if ifNoneMatch == "*" {
		if _, exists := m.data[key]; exists {
			return "", layout.ErrVersionConflict
		}
	}
	if ifMatch != "" {
		if m.etags[key] != ifMatch {
			return "", layout.ErrVersionConflict
		}
	}
	m.seq++
	etag := fmt.Sprintf("%d", m.seq)
	m.data[key] = append([]byte(nil), data...)
	m.etags[key] = etag
	return etag, nil
}

func (m *memV2Store) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	delete(m.etags, key)
	return nil
}

func newV2Backend(t *testing.T) (*BlobBackend, *memV2Store) {
	t.Helper()
	backend := newTestBlobBackend(t)
	mem := newMemV2Store()
	backend.layoutMode = "v2"
	backend.v2Objects = mem
	backend.uploadArchiveFunc = func(ctx context.Context, archivePath string) error {
		for _, path := range []string{archivePath, archivePath + archiveIndexSuffix} {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if _, err := mem.Put(ctx, backend.cloudKey(path), data, "", ""); err != nil {
				return err
			}
		}
		return nil
	}
	backend.openCloudReader = func(key string) (io.ReadCloser, error) {
		data, _, err := mem.Get(context.Background(), key)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	backend.headCloudObject = func(key string) (cloudObjectInfo, bool) {
		data, _, err := mem.Get(context.Background(), key)
		if err != nil {
			return cloudObjectInfo{}, false
		}
		return cloudObjectInfo{Size: int64(len(data))}, true
	}
	return backend, mem
}

func v2Digest(prefix, fill string) string {
	return prefix + strings.Repeat(fill, 64-len(prefix))
}

func TestV2StoreAndFetchRoundTrip(t *testing.T) {
	backend, _ := newV2Backend(t)
	id := "sha256:" + v2Digest("aa", "1")
	content := []byte("hello-v2")
	if err := backend.StoreBlob(id, content); err != nil {
		t.Fatalf("StoreBlob: %v", err)
	}

	obs := backend.Observability()
	if obs.Layout != "v2" || obs.V2Packs != 1 {
		t.Fatalf("observability = %+v", obs)
	}

	// Drop in-memory indexes so the read must resolve the v2 manifest + index.
	backend.mu.Lock()
	backend.blobIndex = map[string]archiveRef{}
	backend.mu.Unlock()
	backend.blobCacheMu.Lock()
	backend.blobCache = map[string][]byte{}
	backend.blobCacheBytes = 0
	backend.blobCacheOrder = nil
	backend.blobCacheMu.Unlock()

	got, err := backend.FetchBlob(context.Background(), &storage.FetchRequest{ContentID: id})
	if err != nil {
		t.Fatalf("FetchBlob: %v", err)
	}
	if string(got.Content) != string(content) {
		t.Fatalf("content = %q, want %q", got.Content, content)
	}
}

func TestV2BatchWriterGroupsShards(t *testing.T) {
	backend, _ := newV2Backend(t)
	writer, err := backend.NewStoreBlobBatchWriter()
	if err != nil {
		t.Fatal(err)
	}
	if !writer.v2 {
		t.Fatal("expected a v2 batch writer")
	}

	a := v2Digest("aa", "2")
	b := v2Digest("bb", "3")
	writer.AddBlob(a, []byte("a"))
	writer.AddBlob(b, []byte("b"))
	result, err := writer.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if result.Stored != 2 {
		t.Fatalf("stored = %d, want 2", result.Stored)
	}
	if result.ArchivesCreated != 2 {
		t.Fatalf("archives = %d, want 2 (one per shard)", result.ArchivesCreated)
	}
	if got := backend.Observability().V2Packs; got != 2 {
		t.Fatalf("v2 packs = %d, want 2", got)
	}

	for _, item := range []struct {
		id      string
		content string
	}{{a, "a"}, {b, "b"}} {
		got, err := backend.FetchBlob(context.Background(), &storage.FetchRequest{ContentID: item.id})
		if err != nil {
			t.Fatalf("FetchBlob(%s): %v", item.id, err)
		}
		if string(got.Content) != item.content {
			t.Fatalf("content = %q, want %q", got.Content, item.content)
		}
	}
}

func TestV2CompactorMergesPacks(t *testing.T) {
	backend, _ := newV2Backend(t)
	ids := []string{
		v2Digest("aa0", "4"),
		v2Digest("aa1", "5"),
		v2Digest("aa2", "6"),
	}
	for i, id := range ids {
		if err := backend.StoreBlob(id, []byte(fmt.Sprintf("payload-%d", i))); err != nil {
			t.Fatalf("StoreBlob(%s): %v", id, err)
		}
	}
	if got := backend.Observability().V2Packs; got != 3 {
		t.Fatalf("v2 packs = %d, want 3", got)
	}

	res, err := backend.RunCompactor(context.Background(), layout.CompactionConfig{
		TargetBytes:      1 << 20,
		MaxPacksPerShard: 2,
		DeadRatio:        0.3,
		GracePeriod:      time.Hour,
		MaxShardsPerRun:  layout.ShardCount,
		MaxBytesPerRun:   1 << 30,
		RetryLimit:       5,
	})
	if err != nil {
		t.Fatalf("RunCompactor: %v", err)
	}
	if res.PlansApplied != 1 {
		t.Fatalf("plans applied = %d, want 1", res.PlansApplied)
	}

	m, _, err := backend.ReadManifest(context.Background(), "aa")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Packs) > 2 {
		t.Fatalf("active packs = %d, want <= 2", len(m.Packs))
	}
	if len(m.Retired) != 3 {
		t.Fatalf("retired packs = %d, want 3", len(m.Retired))
	}

	backend.mu.Lock()
	backend.blobIndex = map[string]archiveRef{}
	backend.mu.Unlock()
	for i, id := range ids {
		got, err := backend.FetchBlob(context.Background(), &storage.FetchRequest{ContentID: id})
		if err != nil {
			t.Fatalf("FetchBlob(%s): %v", id, err)
		}
		if string(got.Content) != fmt.Sprintf("payload-%d", i) {
			t.Fatalf("content = %q", got.Content)
		}
	}
}

func TestV2AddPackRetriesCASConflict(t *testing.T) {
	backend, mem := newV2Backend(t)
	mem.conflictOnce = true
	id := v2Digest("cc", "7")
	if err := backend.StoreBlob(id, []byte("retry")); err != nil {
		t.Fatalf("StoreBlob should retry the CAS conflict: %v", err)
	}
	if got := backend.Observability().V2Packs; got != 1 {
		t.Fatalf("v2 packs = %d, want 1", got)
	}
}

package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/radryc/monofs/internal/storage"
)

func newTestBlobBackend(t *testing.T) *BlobBackend {
	t.Helper()
	backend := NewBlobBackend()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	if err := backend.Initialize(context.Background(), storage.BackendConfig{
		CacheDir:      t.TempDir(),
		Concurrency:   2,
		EncryptionKey: key,
	}); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("Close failed: %v", err)
		}
	})
	return backend
}

func TestBlobBackend_Initialize(t *testing.T) {
	backend := newTestBlobBackend(t)

	if backend.Type() != storage.FetchTypeBlob {
		t.Errorf("expected FetchTypeBlob, got %v", backend.Type())
	}
}

func TestBlobBackend_ArchiveIndexSidecar(t *testing.T) {
	backend := newTestBlobBackend(t)

	content := []byte("sidecar-index-content")
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	if err := backend.StoreBlob(hash, content); err != nil {
		t.Fatalf("StoreBlob() error = %v", err)
	}

	backend.mu.RLock()
	ref, ok := backend.blobIndex[hash]
	backend.mu.RUnlock()
	if !ok {
		t.Fatalf("blob %s missing from index", hash)
	}

	hashes, err := backend.readLocalArchiveIndex(ref.archivePath)
	if err != nil {
		t.Fatalf("readLocalArchiveIndex() error = %v", err)
	}
	if len(hashes) != 1 || hashes[0] != hash {
		t.Fatalf("archive index = %v, want [%s]", hashes, hash)
	}
}

func TestBlobBackend_DeleteBlobsRemovesSidecarAndInvalidates(t *testing.T) {
	backend := newTestBlobBackend(t)

	content := []byte("delete-me")
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	if err := backend.StoreBlob(hash, content); err != nil {
		t.Fatalf("StoreBlob() error = %v", err)
	}

	backend.mu.RLock()
	ref, ok := backend.blobIndex[hash]
	backend.mu.RUnlock()
	if !ok {
		t.Fatalf("blob %s missing from index", hash)
	}

	result := backend.DeleteBlobs([]string{hash}, true)
	if result.Deleted != 1 || result.ArchivesRemoved != 1 {
		t.Fatalf("DeleteBlobs() = %+v, want 1 deleted and 1 archive removed", result)
	}
	if _, err := os.Stat(ref.archivePath); !os.IsNotExist(err) {
		t.Fatalf("archive %q still exists after compaction", ref.archivePath)
	}
	if _, err := os.Stat(ref.archivePath + archiveIndexSuffix); !os.IsNotExist(err) {
		t.Fatalf("sidecar for %q still exists after compaction", ref.archivePath)
	}

	obs := backend.Observability()
	if obs.ArchiveDeletesTotal != 1 || obs.CacheInvalidationsTotal != 1 {
		t.Fatalf("observability = %+v, want archive_deletes=1 cache_invalidations=1", obs)
	}
}

func TestBlobBackend_UploadWALPersistsPendingArchives(t *testing.T) {
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archives")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatal(err)
	}

	backend := &BlobBackend{
		walPending:        make(map[string]struct{}),
		uploadArchiveFunc: func(context.Context, string) error { return nil },
	}
	if err := backend.initUploadWAL(archiveDir); err != nil {
		t.Fatalf("initUploadWAL() error = %v", err)
	}

	archivePath := filepath.Join(archiveDir, "test.pack")
	if err := os.WriteFile(archivePath, []byte("archive"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := backend.walAdd(archivePath); err != nil {
		t.Fatalf("walAdd() error = %v", err)
	}

	restored := &BlobBackend{
		walPending:        make(map[string]struct{}),
		uploadArchiveFunc: func(context.Context, string) error { return nil },
	}
	if err := restored.initUploadWAL(archiveDir); err != nil {
		t.Fatalf("restored initUploadWAL() error = %v", err)
	}
	if _, ok := restored.walPending[archivePath]; !ok {
		t.Fatalf("pending archive %q was not restored from WAL", archivePath)
	}

	if err := restored.walRemove(archivePath); err != nil {
		t.Fatalf("walRemove() error = %v", err)
	}
	final := &BlobBackend{
		walPending:        make(map[string]struct{}),
		uploadArchiveFunc: func(context.Context, string) error { return nil },
	}
	if err := final.initUploadWAL(archiveDir); err != nil {
		t.Fatalf("final initUploadWAL() error = %v", err)
	}
	if _, ok := final.walPending[archivePath]; ok {
		t.Fatalf("archive %q still pending after walRemove", archivePath)
	}
}

func TestBlobBackend_StoreBlobConcurrentDedup(t *testing.T) {
	backend := newTestBlobBackend(t)
	backend.config.Concurrency = 4

	content := []byte("same-content-for-every-writer")
	hash := fmt.Sprintf("%x", sha256.Sum256(content))

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- backend.StoreBlob(hash, content)
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("StoreBlob() error = %v", err)
		}
	}

	backend.mu.RLock()
	defer backend.mu.RUnlock()

	ref, ok := backend.blobIndex[hash]
	if !ok {
		t.Fatalf("blob %s missing from index", hash)
	}
	if got := backend.storageBlobCounts["_loose"]; got != 1 {
		t.Fatalf("storageBlobCounts[_loose] = %d, want 1", got)
	}
	if !backend.archivePaths[ref.archivePath] {
		t.Fatalf("archive path %q not tracked", ref.archivePath)
	}
}

func TestBlobBackend_FetchBlobRecoversMissingIndex(t *testing.T) {
	backend := newTestBlobBackend(t)

	content := []byte("recover-from-disk")
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	if err := backend.StoreBlob(hash, content); err != nil {
		t.Fatalf("StoreBlob() error = %v", err)
	}

	backend.mu.Lock()
	delete(backend.blobIndex, hash)
	backend.mu.Unlock()

	result, err := backend.FetchBlob(context.Background(), &storage.FetchRequest{ContentID: hash})
	if err != nil {
		t.Fatalf("FetchBlob() error = %v", err)
	}
	if string(result.Content) != string(content) {
		t.Fatalf("FetchBlob() content = %q, want %q", string(result.Content), string(content))
	}
	if !backend.HasBlob(hash) {
		t.Fatalf("expected blob %s to be reindexed after fetch", hash)
	}
}

func TestBlobBackend_FetchBlobDownloadsMissingArchiveFromCloud(t *testing.T) {
	backend := newTestBlobBackend(t)

	content := []byte("download-from-cloud")
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	if err := backend.StoreBlob(hash, content); err != nil {
		t.Fatalf("StoreBlob() error = %v", err)
	}

	backend.mu.RLock()
	ref := backend.blobIndex[hash]
	backend.mu.RUnlock()

	archiveBytes, err := os.ReadFile(ref.archivePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", ref.archivePath, err)
	}
	if err := os.Remove(ref.archivePath); err != nil {
		t.Fatalf("Remove(%q) error = %v", ref.archivePath, err)
	}
	backend.evictArchiveReader(ref.archivePath)

	wantKey := backend.cloudKey(ref.archivePath)
	backend.config.StorageType = storage.StorageTypeS3
	backend.openCloudReader = func(key string) (io.ReadCloser, error) {
		if key != wantKey {
			t.Fatalf("cloud key = %q, want %q", key, wantKey)
		}
		return io.NopCloser(bytes.NewReader(archiveBytes)), nil
	}

	result, err := backend.FetchBlob(context.Background(), &storage.FetchRequest{ContentID: hash})
	if err != nil {
		t.Fatalf("FetchBlob() error = %v", err)
	}
	if string(result.Content) != string(content) {
		t.Fatalf("FetchBlob() content = %q, want %q", string(result.Content), string(content))
	}

	restored, err := os.ReadFile(ref.archivePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) after cloud download error = %v", ref.archivePath, err)
	}
	if !bytes.Equal(restored, archiveBytes) {
		t.Fatalf("restored archive bytes differ from downloaded object")
	}
}

func TestBlobBackend_StoreBlobQueuesCloudUpload(t *testing.T) {
	backend := newTestBlobBackend(t)

	uploadStarted := make(chan string, 1)
	releaseUpload := make(chan struct{})
	t.Cleanup(func() {
		close(releaseUpload)
	})

	backend.config.StorageType = storage.StorageTypeS3
	backend.uploadArchiveFunc = func(_ context.Context, archivePath string) error {
		select {
		case uploadStarted <- archivePath:
		default:
		}
		<-releaseUpload
		return nil
	}
	backend.startCloudUploadWorkers()

	content := []byte("queued-cloud-upload")
	hash := fmt.Sprintf("%x", sha256.Sum256(content))

	done := make(chan error, 1)
	go func() {
		done <- backend.StoreBlob(hash, content)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StoreBlob() error = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("StoreBlob() blocked on cloud upload")
	}

	select {
	case archivePath := <-uploadStarted:
		if archivePath == "" {
			t.Fatal("expected queued archive path")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected cloud upload worker to receive queued archive")
	}

	if !backend.HasBlob(hash) {
		t.Fatalf("expected blob %s to be indexed before upload completed", hash)
	}
}

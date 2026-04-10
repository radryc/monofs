package blob

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"

	"github.com/radryc/monofs/internal/storage"
)

func TestBlobBackend_Initialize(t *testing.T) {
	tmpDir := t.TempDir()

	backend := NewBlobBackend()

	// Generate a valid 32-byte key
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	ctx := context.Background()
	err := backend.Initialize(ctx, storage.BackendConfig{
		CacheDir:      tmpDir,
		Concurrency:   2,
		EncryptionKey: key,
	})

	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	if backend.Type() != storage.FetchTypeBlob {
		t.Errorf("expected FetchTypeBlob, got %v", backend.Type())
	}
}

func TestBlobBackend_StoreBlobConcurrentDedup(t *testing.T) {
	tmpDir := t.TempDir()

	backend := NewBlobBackend()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	if err := backend.Initialize(context.Background(), storage.BackendConfig{
		CacheDir:      tmpDir,
		Concurrency:   4,
		EncryptionKey: key,
	}); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

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
	tmpDir := t.TempDir()

	backend := NewBlobBackend()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	if err := backend.Initialize(context.Background(), storage.BackendConfig{
		CacheDir:      tmpDir,
		Concurrency:   2,
		EncryptionKey: key,
	}); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

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

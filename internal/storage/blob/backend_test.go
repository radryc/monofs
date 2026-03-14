package blob

import (
	"context"
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

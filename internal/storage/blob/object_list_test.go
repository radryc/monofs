package blob

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/radryc/monofs/internal/storage"
)

func TestIsHexString(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"", false},
		{"abc123", true},
		{"0123456789abcdef", true},
		{"ABC123", false},
		{"sha256:abc", false},
		{"abc-123", false},
		{"abc.123", false},
	}
	for _, tc := range cases {
		if got := isHexString(tc.input); got != tc.want {
			t.Errorf("isHexString(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestBlobBackend_ListStorageObjectsLocal(t *testing.T) {
	backend := newTestBlobBackend(t)
	archiveDir := filepath.Join(backend.config.CacheDir, "archives")
	nested := filepath.Join(archiveDir, "storage-1")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "blob.pack"), []byte("archive"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, sidecar := range []string{"blob.pack.idx", "upload.wal.json", "blob.pack.tmp"} {
		if err := os.WriteFile(filepath.Join(nested, sidecar), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	objects, err := backend.ListStorageObjects(context.Background())
	if err != nil {
		t.Fatalf("ListStorageObjects: %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("objects = %+v", objects)
	}
	if objects[0].Key != "storage-1/blob.pack" {
		t.Fatalf("key = %q", objects[0].Key)
	}
	if objects[0].Size != int64(len("archive")) {
		t.Fatalf("size = %d", objects[0].Size)
	}
	if objects[0].StorageType != string(storage.StorageTypeLocal) {
		t.Fatalf("storage type = %q", objects[0].StorageType)
	}
}

func TestBlobBackend_ListStorageObjectsErrors(t *testing.T) {
	backend := newTestBlobBackend(t)

	backend.config.StorageType = storage.StorageTypeS3
	if _, err := backend.ListStorageObjects(context.Background()); err == nil {
		t.Fatal("expected an error when the S3 client is not initialized")
	}

	backend.config.StorageType = storage.StorageTypeGCS
	if _, err := backend.ListStorageObjects(context.Background()); err == nil {
		t.Fatal("expected an error when the GCS client is not initialized")
	}

	backend.config.StorageType = storage.StorageType("bogus")
	if _, err := backend.ListStorageObjects(context.Background()); err == nil {
		t.Fatal("expected an error for an unsupported storage type")
	}
}

func TestBlobBackend_ListLocalObjectsMissingDir(t *testing.T) {
	backend := NewBlobBackend()
	backend.config.CacheDir = t.TempDir()

	if _, err := backend.ListStorageObjects(context.Background()); err != nil {
		t.Fatalf("listing a missing archive dir should not fail: %v", err)
	}
}

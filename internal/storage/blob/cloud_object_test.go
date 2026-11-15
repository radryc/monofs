package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/radryc/monofs/internal/storage"
)

func TestBlobBackend_CloudArchiveMatches(t *testing.T) {
	backend := newTestBlobBackend(t)
	content := []byte("cloud-match-content")
	path := filepath.Join(t.TempDir(), "chunk-0000.pack")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	hash, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		remote cloudObjectInfo
		found  bool
		want   bool
	}{
		{"matching size and hash", cloudObjectInfo{Size: int64(len(content)), SHA256: hash}, true, true},
		{"size mismatch", cloudObjectInfo{Size: int64(len(content)) + 1, SHA256: hash}, true, false},
		{"hash mismatch", cloudObjectInfo{Size: int64(len(content)), SHA256: "deadbeef"}, true, false},
		{"legacy object without hash", cloudObjectInfo{Size: int64(len(content))}, true, true},
		{"missing object", cloudObjectInfo{}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend.headCloudObject = func(string) (cloudObjectInfo, bool) {
				return tc.remote, tc.found
			}
			if got := backend.cloudArchiveMatches(path); got != tc.want {
				t.Fatalf("cloudArchiveMatches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBlobBackend_EnsureArchiveInCloudReuploadsOnMismatch(t *testing.T) {
	backend := newTestBlobBackend(t)
	content := []byte("reupload-me")
	path := filepath.Join(t.TempDir(), "chunk-0000.pack")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	uploads := 0
	backend.uploadArchiveFunc = func(context.Context, string) error {
		uploads++
		return nil
	}
	backend.headCloudObject = func(string) (cloudObjectInfo, bool) {
		// Exists, but with stale content.
		return cloudObjectInfo{Size: int64(len(content)) + 10}, true
	}

	if err := backend.ensureArchiveInCloud(context.Background(), path); err != nil {
		t.Fatalf("ensureArchiveInCloud: %v", err)
	}
	if uploads != 1 {
		t.Fatalf("uploads = %d, want 1", uploads)
	}

	hash, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	backend.headCloudObject = func(string) (cloudObjectInfo, bool) {
		return cloudObjectInfo{Size: int64(len(content)), SHA256: hash}, true
	}
	if err := backend.ensureArchiveInCloud(context.Background(), path); err != nil {
		t.Fatalf("ensureArchiveInCloud (matching): %v", err)
	}
	if uploads != 1 {
		t.Fatalf("matching archive was re-uploaded: %d uploads", uploads)
	}
}

func TestBlobBackend_CloudBackupScanKeepsWALUntilContentMatches(t *testing.T) {
	backend := newTestBlobBackend(t)
	content := []byte("scan-content")
	archivePath := filepath.Join(backend.config.CacheDir, "archives", "scan", "chunk-0000.pack")
	if err := os.MkdirAll(filepath.Dir(archivePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, content, 0644); err != nil {
		t.Fatal(err)
	}

	backend.mu.Lock()
	backend.archivePaths[archivePath] = true
	backend.mu.Unlock()
	backend.walPending[archivePath] = struct{}{}

	// Remote object exists but is stale; upload fails.
	backend.headCloudObject = func(string) (cloudObjectInfo, bool) {
		return cloudObjectInfo{Size: int64(len(content)) + 1}, true
	}
	backend.uploadArchiveFunc = func(context.Context, string) error {
		return errors.New("upload unavailable")
	}
	backend.runCloudBackupScan()
	if _, ok := backend.walPending[archivePath]; !ok {
		t.Fatal("WAL entry was dropped although the cloud copy is stale and the upload failed")
	}

	// Upload succeeds; the scan must now clear the WAL entry.
	backend.uploadArchiveFunc = func(context.Context, string) error { return nil }
	backend.runCloudBackupScan()
	if _, ok := backend.walPending[archivePath]; ok {
		t.Fatal("WAL entry was not cleared after a successful repair upload")
	}
}

func TestBlobBackend_EnsureCloudBlobIndexUsesLister(t *testing.T) {
	backend := &BlobBackend{
		config:             storage.BackendConfig{CacheDir: t.TempDir()},
		cloudBlobIndex:     map[string]string{},
		cloudIndexPrefixes: map[string]time.Time{},
	}
	backend.openCloudReader = func(key string) (io.ReadCloser, error) {
		hash := "hash-" + filepath.Base(filepath.Dir(key))
		return io.NopCloser(bytes.NewReader([]byte(`["` + hash + `"]`))), nil
	}
	backend.listCloudObjects = func(_ context.Context, prefix string, _ int) ([]storage.StorageObject, error) {
		return []storage.StorageObject{{Key: prefix + "chunk.pack.idx"}}, nil
	}

	if err := backend.ensureCloudBlobIndex(context.Background(), "abc"); err != nil {
		t.Fatalf("ensureCloudBlobIndex: %v", err)
	}
	backend.cloudIndexMu.Lock()
	defer backend.cloudIndexMu.Unlock()
	if len(backend.cloudBlobIndex) != 3 {
		t.Fatalf("cloud index entries = %d, want 3", len(backend.cloudBlobIndex))
	}
	if backend.cloudBlobIndex["hash-_batch"] != "_batch/chunk.pack" {
		t.Fatalf("missing _batch entry: %+v", backend.cloudBlobIndex)
	}
}

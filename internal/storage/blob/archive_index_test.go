package blob

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/radryc/monofs/internal/storage"
	"github.com/radryc/packager"
)

func TestBlobBackend_IndexesPrefixedArchiveEntries(t *testing.T) {
	backend := newTestBlobBackend(t)
	content := []byte("legacy-prefixed-entry")
	bare := blobHash(content)

	dir := filepath.Join(backend.config.CacheDir, "archives", "legacy")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(dir, "chunk-0000.pack")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := packager.NewArchiveWriter(file, backend.pipeline)
	if err := writer.AddFile("sha256:"+bare, content, packager.AddFileOptions{
		Permission: 0644,
		Encrypt:    true,
	}); err != nil {
		file.Close()
		t.Fatalf("AddFile: %v", err)
	}
	if err := writer.Close(); err != nil {
		file.Close()
		t.Fatalf("Close writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := backend.indexArchive(archivePath); err != nil {
		t.Fatalf("indexArchive: %v", err)
	}
	if !backend.HasBlob(bare) {
		t.Fatalf("bare hash %q was not indexed", bare)
	}
	if backend.HasBlob("sha256:" + bare) {
		t.Fatal("prefixed entry name was used as the index key")
	}

	fetched, err := backend.FetchBlob(context.Background(), &storage.FetchRequest{ContentID: bare})
	if err != nil {
		t.Fatalf("FetchBlob: %v", err)
	}
	if string(fetched.Content) != string(content) {
		t.Fatalf("content = %q, want %q", fetched.Content, content)
	}

	hashes, err := backend.archiveHashes(archivePath)
	if err != nil {
		t.Fatalf("archiveHashes: %v", err)
	}
	if len(hashes) != 1 || hashes[0] != bare {
		t.Fatalf("archiveHashes = %v, want [%s]", hashes, bare)
	}
}

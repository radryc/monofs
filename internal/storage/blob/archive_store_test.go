package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/radryc/monofs/internal/storage"
	"github.com/radryc/packager"
)

func blobHash(content []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(content))
}

func buildTestArchive(t *testing.T, backend *BlobBackend, entries map[string][]byte) []byte {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "archive-*.pack")
	if err != nil {
		t.Fatal(err)
	}
	writer := packager.NewArchiveWriter(tmp, backend.pipeline)
	// Iterate deterministically for reproducibility.
	hashes := make([]string, 0, len(entries))
	for hash := range entries {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	for _, hash := range hashes {
		if err := writer.AddFile(hash, entries[hash], packager.AddFileOptions{Permission: 0644, Encrypt: true}); err != nil {
			tmp.Close()
			t.Fatalf("AddFile(%s): %v", hash, err)
		}
	}
	if err := writer.Close(); err != nil {
		tmp.Close()
		t.Fatalf("Close writer: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBlobBackend_StoreArchiveReingestReplacesIndex(t *testing.T) {
	backend := newTestBlobBackend(t)
	one := []byte("reingest-one")
	two := []byte("reingest-two")
	oneHash := blobHash(one)
	twoHash := blobHash(two)

	first := buildTestArchive(t, backend, map[string][]byte{oneHash: one})
	written, count, err := backend.StoreArchive("reingest", 0, bytes.NewReader(first))
	if err != nil {
		t.Fatalf("StoreArchive first: %v", err)
	}
	if written == 0 || count != 1 {
		t.Fatalf("first StoreArchive = %d bytes, %d blobs", written, count)
	}
	if !backend.HasBlob(oneHash) {
		t.Fatal("first blob not indexed")
	}

	second := buildTestArchive(t, backend, map[string][]byte{twoHash: two})
	_, count, err = backend.StoreArchive("reingest", 0, bytes.NewReader(second))
	if err != nil {
		t.Fatalf("StoreArchive second: %v", err)
	}
	if count != 1 {
		t.Fatalf("second StoreArchive count = %d, want 1", count)
	}
	if backend.HasBlob(oneHash) {
		t.Fatal("blob from the replaced archive is still indexed")
	}
	if !backend.HasBlob(twoHash) {
		t.Fatal("replacement blob not indexed")
	}
	if got := backend.StorageStats()["reingest"]; got != 1 {
		t.Fatalf("storage blob count = %d, want 1", got)
	}
	if backend.ArchiveCount() != 1 {
		t.Fatalf("archive count = %d, want 1", backend.ArchiveCount())
	}
	if stats := backend.Stats(); stats.CachedItems != 1 {
		t.Fatalf("CachedItems = %d, want 1", stats.CachedItems)
	}
}

func TestBlobBackend_StoreArchiveRejectsCorruptInput(t *testing.T) {
	backend := newTestBlobBackend(t)
	_, _, err := backend.StoreArchive("corrupt", 0, bytes.NewReader([]byte("not an archive")))
	if err == nil {
		t.Fatal("expected an error for corrupt archive input")
	}
	archivePath := filepath.Join(backend.config.CacheDir, "archives", "corrupt", "chunk-0000.pack")
	if _, statErr := os.Stat(archivePath); !os.IsNotExist(statErr) {
		t.Fatalf("corrupt archive was left on disk: %v", statErr)
	}
	if backend.HasBlob(blobHash([]byte("not an archive"))) {
		t.Fatal("corrupt archive indexed a blob")
	}
}

func TestBlobBackend_StoreBlobBatchWriter(t *testing.T) {
	backend := newTestBlobBackend(t)
	one := []byte("blob-one")
	two := []byte("blob-two")
	oneHash := blobHash(one)
	twoHash := blobHash(two)

	writer, err := backend.NewStoreBlobBatchWriter()
	if err != nil {
		t.Fatalf("NewStoreBlobBatchWriter: %v", err)
	}

	writer.AddBlob("", one)
	writer.AddBlob(oneHash, nil)
	writer.AddBlob(oneHash, one)
	writer.AddBlob(twoHash, two)

	result, err := writer.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if result.Stored != 2 || result.Failed != 2 {
		t.Fatalf("result = %+v, want 2 stored / 2 failed", result)
	}
	if result.ArchivesCreated != 1 || result.ArchiveBytes == 0 {
		t.Fatalf("result = %+v", result)
	}

	if !backend.HasBlob(oneHash) || !backend.HasBlob(twoHash) {
		t.Fatal("batch writer did not index its blobs")
	}
	for content, hash := range map[string]string{string(one): oneHash, string(two): twoHash} {
		fetched, err := backend.FetchBlob(context.Background(), &storage.FetchRequest{ContentID: hash})
		if err != nil {
			t.Fatalf("FetchBlob(%s): %v", hash, err)
		}
		if string(fetched.Content) != content {
			t.Fatalf("content = %q, want %q", fetched.Content, content)
		}
	}

	// Finish is idempotent and does not seal an empty archive.
	again, err := writer.Finish()
	if err != nil {
		t.Fatalf("second Finish: %v", err)
	}
	if again.ArchivesCreated != 1 || again.Stored != 2 {
		t.Fatalf("second Finish = %+v", again)
	}

	// A new writer skips blobs that are already indexed.
	second, err := backend.NewStoreBlobBatchWriter()
	if err != nil {
		t.Fatalf("second NewStoreBlobBatchWriter: %v", err)
	}
	second.AddBlob(oneHash, one)
	skipped, err := second.Finish()
	if err != nil {
		t.Fatalf("second writer Finish: %v", err)
	}
	if skipped.Skipped != 1 || skipped.ArchivesCreated != 0 {
		t.Fatalf("second writer result = %+v", skipped)
	}
}

func TestBlobBackend_StoreBlobBatchWriterEmpty(t *testing.T) {
	backend := newTestBlobBackend(t)
	writer, err := backend.NewStoreBlobBatchWriter()
	if err != nil {
		t.Fatalf("NewStoreBlobBatchWriter: %v", err)
	}
	result, err := writer.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if result.ArchivesCreated != 0 || result.Stored != 0 {
		t.Fatalf("result = %+v", result)
	}
	if backend.ArchiveCount() != 0 {
		t.Fatalf("archive count = %d, want 0", backend.ArchiveCount())
	}
}

func TestBlobBackend_DeleteBlobsPartialArchive(t *testing.T) {
	backend := newTestBlobBackend(t)
	one := []byte("blob-one")
	two := []byte("blob-two")
	oneHash := blobHash(one)
	twoHash := blobHash(two)

	writer, err := backend.NewStoreBlobBatchWriter()
	if err != nil {
		t.Fatalf("NewStoreBlobBatchWriter: %v", err)
	}
	writer.AddBlob(oneHash, one)
	writer.AddBlob(twoHash, two)
	if _, err := writer.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	result := backend.DeleteBlobs([]string{oneHash}, true)
	if result.Deleted != 1 || result.ArchivesRemoved != 0 {
		t.Fatalf("partial delete = %+v", result)
	}
	if backend.HasBlob(oneHash) {
		t.Fatal("deleted blob still indexed")
	}
	if !backend.HasBlob(twoHash) {
		t.Fatal("sibling blob was removed with a partial delete")
	}

	result = backend.DeleteBlobs([]string{"missing"}, true)
	if result.NotFound != 1 {
		t.Fatalf("missing delete = %+v", result)
	}

	result = backend.DeleteBlobs([]string{twoHash}, true)
	if result.Deleted != 1 || result.ArchivesRemoved != 1 {
		t.Fatalf("final delete = %+v", result)
	}
	if backend.ArchiveCount() != 0 {
		t.Fatalf("archive count = %d, want 0", backend.ArchiveCount())
	}
}

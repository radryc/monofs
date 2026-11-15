package blob

import (
	"testing"
)

func TestBlobBackend_StatsCountsBlobsNotArchives(t *testing.T) {
	backend := newTestBlobBackend(t)

	one := []byte("observability-one")
	two := []byte("observability-two")
	oneHash := blobHash(one)
	twoHash := blobHash(two)

	if err := backend.StoreBlob(oneHash, one); err != nil {
		t.Fatalf("StoreBlob: %v", err)
	}

	writer, err := backend.NewStoreBlobBatchWriter()
	if err != nil {
		t.Fatalf("NewStoreBlobBatchWriter: %v", err)
	}
	writer.AddBlob(twoHash, two)
	if _, err := writer.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	stats := backend.Stats()
	if stats.CachedItems != 2 {
		t.Fatalf("CachedItems = %d, want 2 blobs", stats.CachedItems)
	}

	obs := backend.Observability()
	if obs.Archives != 2 {
		t.Fatalf("Archives = %d, want 2", obs.Archives)
	}
	if obs.Blobs != 2 {
		t.Fatalf("Blobs = %d, want 2", obs.Blobs)
	}
	if backend.ArchiveCount() != 2 {
		t.Fatalf("ArchiveCount = %d, want 2", backend.ArchiveCount())
	}
	if backend.StorageIDCount() != 2 {
		t.Fatalf("StorageIDCount = %d, want 2", backend.StorageIDCount())
	}
}

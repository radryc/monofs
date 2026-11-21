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

func TestResolveBlobLayoutMode(t *testing.T) {
	t.Setenv("MONOFS_BLOB_LAYOUT_V2", "")
	if got := resolveBlobLayoutMode(); got != "v2" {
		t.Fatalf("default layout mode = %q, want v2", got)
	}
	t.Setenv("MONOFS_BLOB_LAYOUT_V2", "true")
	if got := resolveBlobLayoutMode(); got != "v2" {
		t.Fatalf("layout mode = %q, want v2", got)
	}
	t.Setenv("MONOFS_BLOB_LAYOUT_V2", "false")
	if got := resolveBlobLayoutMode(); got != "legacy" {
		t.Fatalf("layout mode = %q, want legacy", got)
	}
	t.Setenv("MONOFS_BLOB_LAYOUT_V2", "0")
	if got := resolveBlobLayoutMode(); got != "legacy" {
		t.Fatalf("layout mode = %q, want legacy", got)
	}
}

func TestBlobBackend_RecordCompactorRun(t *testing.T) {
	backend := newTestBlobBackend(t)
	if got := backend.LayoutMode(); got != "legacy" {
		t.Fatalf("LayoutMode = %q, want legacy", got)
	}
	backend.SetBatchStats(4096, 3)
	backend.RecordCompactorRun(16, 2, 5, 4, 1<<20, nil)

	obs := backend.Observability()
	if obs.BatchPendingBytes != 4096 || obs.BatchOpenShards != 3 {
		t.Fatalf("batch stats = %d/%d", obs.BatchPendingBytes, obs.BatchOpenShards)
	}
	if obs.CompactorRunsTotal != 1 || obs.CompactorShardsTotal != 16 || obs.CompactorPlansTotal != 2 {
		t.Fatalf("compactor counters = %+v", obs)
	}
	if obs.CompactorPacksRetiredTotal != 5 || obs.CompactorPacksDeletedTotal != 4 {
		t.Fatalf("compactor packs = %+v", obs)
	}
	if obs.CompactorBytesRewrittenTotal != 1<<20 || obs.CompactorLastRunUnix == 0 {
		t.Fatalf("compactor run = %+v", obs)
	}
	if obs.CompactorLastError != "" {
		t.Fatalf("unexpected compactor error: %q", obs.CompactorLastError)
	}

	backend.RecordCompactorRun(0, 0, 0, 0, 0, errTest)
	if got := backend.Observability().CompactorLastError; got != errTest.Error() {
		t.Fatalf("compactor error = %q, want %q", got, errTest.Error())
	}
}

var errTest = &testError{}

type testError struct{}

func (*testError) Error() string { return "compaction failed" }

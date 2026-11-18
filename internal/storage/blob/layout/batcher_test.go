package layout

import (
	"strings"
	"testing"
	"time"
)

func digestFor(shard string) string {
	return shard + strings.Repeat("0", 62)
}

func TestBatcherFlushByBytes(t *testing.T) {
	b := NewBatcher(BatchConfig{TargetBytes: 10, MaxObjects: 100, FlushAfter: time.Hour, MaxShards: 4})
	d := digestFor("ab")

	batches, err := b.Add(d, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 0 {
		t.Fatalf("unexpected early flush: %v", batches)
	}
	batches, err = b.Add(d, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 {
		t.Fatalf("expected 1 batch, got %d", len(batches))
	}
	if batches[0].Shard != "ab" || batches[0].Bytes != 12 || len(batches[0].Blobs) != 2 {
		t.Fatalf("unexpected batch: %+v", batches[0])
	}
	if err := batches[0].Validate(); err != nil {
		t.Fatalf("batch invalid: %v", err)
	}
}

func TestBatcherFlushByObjects(t *testing.T) {
	b := NewBatcher(BatchConfig{TargetBytes: 1 << 20, MaxObjects: 2, FlushAfter: time.Hour, MaxShards: 4})
	d := digestFor("ab")
	if batches, _ := b.Add(d, 1); len(batches) != 0 {
		t.Fatalf("unexpected early flush: %v", batches)
	}
	batches, _ := b.Add(d, 1)
	if len(batches) != 1 || len(batches[0].Blobs) != 2 {
		t.Fatalf("expected 2-object batch, got %+v", batches)
	}
}

func TestBatcherOversizedBlobSealedAlone(t *testing.T) {
	b := NewBatcher(BatchConfig{TargetBytes: 10, MaxObjects: 100, FlushAfter: time.Hour, MaxShards: 4})
	d := digestFor("ab")
	if _, err := b.Add(d, 5); err != nil {
		t.Fatal(err)
	}
	batches, err := b.Add(d, 11)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 {
		t.Fatalf("expected current + oversized batches, got %d", len(batches))
	}
	if batches[0].Bytes != 5 || batches[1].Bytes != 11 || len(batches[1].Blobs) != 1 {
		t.Fatalf("unexpected batches: %+v", batches)
	}
}

func TestBatcherDueByAge(t *testing.T) {
	now := time.Unix(1000, 0)
	b := NewBatcherWithClock(BatchConfig{
		TargetBytes: 1 << 20, MaxObjects: 100, FlushAfter: time.Second, MaxShards: 4,
	}, func() time.Time { return now })

	if _, err := b.Add(digestFor("ab"), 1); err != nil {
		t.Fatal(err)
	}
	if got := b.Due(); len(got) != 0 {
		t.Fatalf("flushed too early: %v", got)
	}
	now = now.Add(2 * time.Second)
	got := b.Due()
	if len(got) != 1 {
		t.Fatalf("expected 1 due batch, got %d", len(got))
	}
	if len(b.OpenShards()) != 0 {
		t.Fatalf("builder not removed: %v", b.OpenShards())
	}
}

func TestBatcherBoundsOpenShards(t *testing.T) {
	now := time.Unix(1000, 0)
	b := NewBatcherWithClock(BatchConfig{
		TargetBytes: 1 << 20, MaxObjects: 100, FlushAfter: time.Hour, MaxShards: 1,
	}, func() time.Time { return now })

	if _, err := b.Add(digestFor("ab"), 1); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	batches, err := b.Add(digestFor("cd"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 || batches[0].Shard != "ab" {
		t.Fatalf("expected oldest shard ab to flush, got %+v", batches)
	}
}

func TestBatcherFlushAllAndPending(t *testing.T) {
	b := NewBatcher(DefaultBatchConfig())
	if _, err := b.Add(digestFor("ab"), 4); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Add(digestFor("cd"), 6); err != nil {
		t.Fatal(err)
	}
	if got := b.PendingBytes(); got != 10 {
		t.Fatalf("pending bytes = %d, want 10", got)
	}
	batches := b.FlushAll()
	if len(batches) != 2 {
		t.Fatalf("expected 2 batches, got %d", len(batches))
	}
	if batches[0].Shard != "ab" || batches[1].Shard != "cd" {
		t.Fatalf("unexpected order: %+v", batches)
	}
	if got := b.PendingBytes(); got != 0 {
		t.Fatalf("pending bytes after flush = %d", got)
	}
}

func TestBatcherRejectsInvalidDigest(t *testing.T) {
	b := NewBatcher(DefaultBatchConfig())
	if _, err := b.Add("nope", 1); err == nil {
		t.Fatal("expected invalid digest error")
	}
}

func TestBatchValidateRejectsWrongShard(t *testing.T) {
	batch := Batch{Shard: "ab", Blobs: []PendingBlob{{Digest: digestFor("cd"), Size: 1}}, Bytes: 1}
	if err := batch.Validate(); err == nil {
		t.Fatal("expected shard mismatch error")
	}
}

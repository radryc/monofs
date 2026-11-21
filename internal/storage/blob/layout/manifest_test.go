package layout

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestManifestPackLifecycle(t *testing.T) {
	m := NewManifest("ab")
	if err := m.Validate(); err != nil {
		t.Fatalf("empty manifest invalid: %v", err)
	}

	p1 := PackRef{ID: "100-0000", Bytes: 100, Blobs: 2, CreatedUnix: 1, Sealed: false}
	if err := m.AddPack(p1); err != nil {
		t.Fatal(err)
	}
	if err := m.AddPack(p1); err == nil {
		t.Fatal("expected duplicate pack error")
	}
	if _, ok := m.UnsealedPack(); !ok {
		t.Fatal("expected unsealed pack")
	}

	m.Seal(p1.ID)
	if _, ok := m.UnsealedPack(); ok {
		t.Fatal("expected no unsealed pack after Seal")
	}

	now := time.Unix(1000, 0)
	if got := m.Retire([]string{p1.ID, "missing"}, now); len(got) != 1 || got[0] != p1.ID {
		t.Fatalf("retired = %v", got)
	}
	if len(m.Packs) != 0 || len(m.Retired) != 1 {
		t.Fatalf("packs=%d retired=%d", len(m.Packs), len(m.Retired))
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest with retired pack invalid: %v", err)
	}

	if got := m.ExpiredRetired(now.Add(30*time.Minute), time.Hour); len(got) != 0 {
		t.Fatalf("retired too early: %v", got)
	}
	if got := m.ExpiredRetired(now.Add(2*time.Hour), time.Hour); len(got) != 1 || got[0] != p1.ID {
		t.Fatalf("expected expired %s, got %v", p1.ID, got)
	}

	m.RemoveRetired(p1.ID)
	if len(m.Retired) != 0 {
		t.Fatalf("retired not removed: %v", m.Retired)
	}
}

func TestManifestCloneIsDeep(t *testing.T) {
	m := NewManifest("ab")
	_ = m.AddPack(PackRef{ID: "1-0000", Bytes: 1})
	cp := m.Clone()
	cp.Packs[0].Bytes = 99
	if m.Packs[0].Bytes != 1 {
		t.Fatal("clone mutated original")
	}
}

func TestMemoryManifestStoreCAS(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryManifestStore()

	if _, _, err := s.Read(ctx, "ab"); !errors.Is(err, ErrManifestNotFound) {
		t.Fatalf("expected ErrManifestNotFound, got %v", err)
	}

	m := NewManifest("ab")
	etag, err := s.CompareAndSwap(ctx, "ab", "", m)
	if err != nil {
		t.Fatal(err)
	}
	if etag != "1" {
		t.Fatalf("etag = %q, want 1", etag)
	}

	got, gotETag, err := s.Read(ctx, "ab")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || gotETag != "1" {
		t.Fatalf("version=%d etag=%q", got.Version, gotETag)
	}

	// Stale etag must conflict.
	if _, err := s.CompareAndSwap(ctx, "ab", "", m); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}

	next := got.Clone()
	next.UpdatedUnix++
	etag2, err := s.CompareAndSwap(ctx, "ab", gotETag, next)
	if err != nil {
		t.Fatal(err)
	}
	if etag2 != "2" {
		t.Fatalf("etag2 = %q, want 2", etag2)
	}
}

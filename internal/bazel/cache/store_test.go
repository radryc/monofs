package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNewStore(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(DefaultStoreOptions(dir))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	// Directories should exist.
	for _, sub := range []string{"ac", "cas"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err != nil {
			t.Errorf("%s/ dir missing: %v", sub, err)
		}
	}
}

func TestPutGetAC(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(DefaultStoreOptions(dir))
	ctx := context.Background()

	digest := DigestString([]byte("action-result-data"))
	data := []byte("hello ac")
	if err := s.PutAC(ctx, digest, data); err != nil {
		t.Fatalf("PutAC: %v", err)
	}

	got, err := s.GetAC(ctx, digest)
	if err != nil {
		t.Fatalf("GetAC: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("GetAC: got %q, want %q", got, data)
	}
}

func TestGetACNotFound(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(DefaultStoreOptions(dir))
	ctx := context.Background()

	_, err := s.GetAC(ctx, "nonexistent/0")
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestHasAC(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(DefaultStoreOptions(dir))
	ctx := context.Background()

	digest := DigestString([]byte("test"))

	exists, _ := s.HasAC(ctx, digest)
	if exists {
		t.Error("HasAC should be false before put")
	}

	s.PutAC(ctx, digest, []byte("x"))
	exists, _ = s.HasAC(ctx, digest)
	if !exists {
		t.Error("HasAC should be true after put")
	}
}

func TestPutGetCAS(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(DefaultStoreOptions(dir))
	ctx := context.Background()

	raw := []byte("build output data")
	digest := DigestString(raw)

	if err := s.PutCAS(ctx, digest, raw); err != nil {
		t.Fatalf("PutCAS: %v", err)
	}

	got, size, err := s.GetCAS(ctx, digest)
	if err != nil {
		t.Fatalf("GetCAS: %v", err)
	}
	if int64(len(raw)) != size {
		t.Errorf("size: got %d, want %d", size, len(raw))
	}
	if string(got) != string(raw) {
		t.Errorf("data mismatch")
	}
}

func TestPutCASDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(DefaultStoreOptions(dir))
	ctx := context.Background()

	// Claim a digest that doesn't match the data.
	err := s.PutCAS(ctx, "aaaa/4", []byte("different"))
	if err == nil {
		t.Error("expected digest mismatch error")
	}
}

func TestPutCASBlobTooLarge(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultStoreOptions(dir)
	opts.MaxCASBlobSize = 10
	s, _ := NewStore(opts)
	ctx := context.Background()

	raw := make([]byte, 100)
	digest := DigestString(raw)
	err := s.PutCAS(ctx, digest, raw)
	if err == nil {
		t.Error("expected ErrBlobTooLarge")
	}
}

func TestPutCASNoValidate(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(DefaultStoreOptions(dir))
	ctx := context.Background()

	// Put with mismatched digest via NoValidate.
	err := s.PutCASNoValidate(ctx, "fake-digest/3", []byte("abc"))
	if err != nil {
		t.Fatalf("PutCASNoValidate: %v", err)
	}

	got, _, err := s.GetCAS(ctx, "fake-digest/3")
	if err != nil {
		t.Fatalf("GetCAS after NoValidate: %v", err)
	}
	if string(got) != "abc" {
		t.Errorf("got %q, want abc", got)
	}
}

func TestGetCASStream(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(DefaultStoreOptions(dir))
	ctx := context.Background()

	raw := []byte("streaming test data")
	digest := DigestString(raw)
	s.PutCAS(ctx, digest, raw)

	rc, size, err := s.GetCASStream(ctx, digest)
	if err != nil {
		t.Fatalf("GetCASStream: %v", err)
	}
	defer rc.Close()

	if size != int64(len(raw)) {
		t.Errorf("size: got %d, want %d", size, len(raw))
	}

	buf := make([]byte, len(raw)+10)
	n, err := rc.Read(buf)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if n != len(raw) || string(buf[:n]) != string(raw) {
		t.Errorf("read mismatch: got %q", buf[:n])
	}
}

func TestDeleteCAS(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(DefaultStoreOptions(dir))
	ctx := context.Background()

	raw := []byte("delete me")
	digest := DigestString(raw)
	s.PutCAS(ctx, digest, raw)

	if err := s.DeleteCAS(ctx, digest); err != nil {
		t.Fatalf("DeleteCAS: %v", err)
	}

	_, _, err := s.GetCAS(ctx, digest)
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestStats(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(DefaultStoreOptions(dir))
	ctx := context.Background()

	// Empty
	acEntries, acBytes, _ := s.ACStats(ctx)
	if acEntries != 0 || acBytes != 0 {
		t.Errorf("empty AC: entries=%d bytes=%d", acEntries, acBytes)
	}

	s.PutAC(ctx, DigestString([]byte("a")), []byte("a"))
	s.PutAC(ctx, DigestString([]byte("bb")), []byte("bb"))

	acEntries, acBytes, _ = s.ACStats(ctx)
	if acEntries != 2 {
		t.Errorf("AC entries: got %d, want 2", acEntries)
	}
	if acBytes < 2 {
		t.Errorf("AC bytes: got %d, want >= 2", acBytes)
	}
}

func TestParseDigest(t *testing.T) {
	tests := []struct {
		input    string
		hash     string
		size     int64
		hasError bool
	}{
		{"abc123/42", "abc123", 42, false},
		{"abc123/0", "abc123", 0, false},
		{"abc/def/123", "abc", 0, true}, // no size after second slash for ParseDigest
		{"no-slash", "", 0, true},
		{"abc/-1", "", 0, true},
	}
	for _, tt := range tests {
		hash, size, err := ParseDigest(tt.input)
		if tt.hasError {
			if err == nil {
				t.Errorf("ParseDigest(%q): expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDigest(%q): %v", tt.input, err)
			continue
		}
		if hash != tt.hash || size != tt.size {
			t.Errorf("ParseDigest(%q) = (%q, %d), want (%q, %d)",
				tt.input, hash, size, tt.hash, tt.size)
		}
	}
}

func TestDigestStringRoundTrip(t *testing.T) {
	data := []byte("hello world")
	ds := DigestString(data)
	hash, size, err := ParseDigest(ds)
	if err != nil {
		t.Fatalf("ParseDigest: %v", err)
	}
	if size != int64(len(data)) {
		t.Errorf("size: got %d, want %d", size, len(data))
	}
	// hash should be non-empty hex.
	if len(hash) != 64 {
		t.Errorf("hash length: got %d, want 64", len(hash))
	}
}

func TestListCAS(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(DefaultStoreOptions(dir))
	ctx := context.Background()

	s.PutCAS(ctx, DigestString([]byte("a")), []byte("a"))
	s.PutCAS(ctx, DigestString([]byte("b")), []byte("b"))

	digests, err := s.ListCAS(ctx)
	if err != nil {
		t.Fatalf("ListCAS: %v", err)
	}
	if len(digests) != 2 {
		t.Errorf("ListCAS: got %d digests, want 2", len(digests))
	}
}

func TestStoreDirEmpty(t *testing.T) {
	_, err := NewStore(StoreOptions{Dir: ""})
	if err == nil {
		t.Error("expected error for empty dir")
	}
}

func TestCASBlobAge(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(DefaultStoreOptions(dir))
	ctx := context.Background()

	raw := []byte("old blob")
	digest := DigestString(raw)
	s.PutCAS(ctx, digest, raw)

	age, err := s.CASBlobAge(ctx, digest)
	if err != nil {
		t.Fatalf("CASBlobAge: %v", err)
	}
	if age.IsZero() {
		t.Error("age should not be zero")
	}
}

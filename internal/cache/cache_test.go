package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setupTestCache(t *testing.T) (*Cache, string) {
	tmpDir, err := os.MkdirTemp("", "monofs-cache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	cache, err := New(tmpDir, nil)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("failed to create cache: %v", err)
	}

	return cache, tmpDir
}

func TestNew(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer os.RemoveAll(tmpDir)
	defer cache.Close()

	if cache == nil {
		t.Fatal("New returned nil cache")
	}
}

func TestPutAttr(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer os.RemoveAll(tmpDir)
	defer cache.Close()

	entry := &AttrEntry{
		Ino:   123,
		Mode:  0644,
		Size:  1024,
		Mtime: time.Now().Unix(),
	}

	err := cache.PutAttr("/test/file.txt", entry)
	if err != nil {
		t.Fatalf("PutAttr failed: %v", err)
	}
}

func TestGetAttr(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer os.RemoveAll(tmpDir)
	defer cache.Close()

	entry := &AttrEntry{
		Ino:   123,
		Mode:  0644,
		Size:  1024,
		Mtime: time.Now().Unix(),
	}

	err := cache.PutAttr("/test/file.txt", entry)
	if err != nil {
		t.Fatalf("PutAttr failed: %v", err)
	}

	// Get the cached attribute
	cachedAttr, err := cache.GetAttr("/test/file.txt")
	if err != nil {
		t.Fatalf("GetAttr failed: %v", err)
	}

	if cachedAttr.Ino != entry.Ino {
		t.Errorf("Ino mismatch: expected %d, got %d", entry.Ino, cachedAttr.Ino)
	}
	if cachedAttr.Mode != entry.Mode {
		t.Errorf("Mode mismatch: expected %d, got %d", entry.Mode, cachedAttr.Mode)
	}
	if cachedAttr.Size != entry.Size {
		t.Errorf("Size mismatch: expected %d, got %d", entry.Size, cachedAttr.Size)
	}
}

func TestGetAttrNotFound(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer os.RemoveAll(tmpDir)
	defer cache.Close()

	_, err := cache.GetAttr("/nonexistent")
	if err == nil {
		t.Error("GetAttr: expected error for non-existent entry")
	}
}

func TestPutDir(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer os.RemoveAll(tmpDir)
	defer cache.Close()

	entries := []DirEntry{
		{Name: "file1.txt", Mode: 0644, Ino: 1},
		{Name: "file2.txt", Mode: 0644, Ino: 2},
		{Name: "subdir", Mode: 0755, Ino: 3},
	}

	err := cache.PutDir("/test", entries)
	if err != nil {
		t.Fatalf("PutDir failed: %v", err)
	}
}

func TestGetDir(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer os.RemoveAll(tmpDir)
	defer cache.Close()

	entries := []DirEntry{
		{Name: "file1.txt", Mode: 0644, Ino: 1},
		{Name: "file2.txt", Mode: 0644, Ino: 2},
	}

	err := cache.PutDir("/test", entries)
	if err != nil {
		t.Fatalf("PutDir failed: %v", err)
	}

	// Get the cached directory
	cachedEntries, err := cache.GetDir("/test")
	if err != nil {
		t.Fatalf("GetDir failed: %v", err)
	}

	if len(cachedEntries) != len(entries) {
		t.Errorf("entry count mismatch: expected %d, got %d", len(entries), len(cachedEntries))
	}
}

func TestGetDirNotFound(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer os.RemoveAll(tmpDir)
	defer cache.Close()

	_, err := cache.GetDir("/nonexistent")
	if err == nil {
		t.Error("GetDir: expected error for non-existent directory")
	}
}

func TestInvalidate(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer os.RemoveAll(tmpDir)
	defer cache.Close()

	// Put both attr and dir
	entry := &AttrEntry{Ino: 123, Mode: 0644}
	entries := []DirEntry{{Name: "test", Mode: 0644, Ino: 1}}

	cache.PutAttr("/test", entry)
	cache.PutDir("/test", entries)

	// Invalidate
	cache.Invalidate("/test")

	// Verify both are gone
	_, err := cache.GetAttr("/test")
	if err == nil {
		t.Error("attr still found after invalidation")
	}
	_, err = cache.GetDir("/test")
	if err == nil {
		t.Error("dir still found after invalidation")
	}
}

func TestClose(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer os.RemoveAll(tmpDir)

	err := cache.Close()
	if err != nil {
		t.Errorf("Close failed: %v", err)
	}
}

func BenchmarkPutAttr(b *testing.B) {
	cache, tmpDir := setupTestCache(&testing.T{})
	defer os.RemoveAll(tmpDir)
	defer cache.Close()

	entry := &AttrEntry{
		Ino:  123,
		Mode: 0644,
		Size: 1024,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		path := filepath.Join("/bench", string(rune(i%100)))
		cache.PutAttr(path, entry)
	}
}

func BenchmarkGetAttr(b *testing.B) {
	cache, tmpDir := setupTestCache(&testing.T{})
	defer os.RemoveAll(tmpDir)
	defer cache.Close()

	// Pre-populate cache
	entry := &AttrEntry{Ino: 123, Mode: 0644}
	for i := 0; i < 100; i++ {
		path := filepath.Join("/bench", string(rune(i)))
		cache.PutAttr(path, entry)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		path := filepath.Join("/bench", string(rune(i%100)))
		cache.GetAttr(path)
	}
}

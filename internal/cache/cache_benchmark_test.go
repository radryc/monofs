package cache

import (
	"fmt"
	"os"
	"testing"
)

func setupBenchCache(b *testing.B) (*Cache, func()) {
	b.Helper()
	tmpDir, err := os.MkdirTemp("", "monofs-cache-bench-*")
	if err != nil {
		b.Fatalf("MkdirTemp: %v", err)
	}
	c, err := New(tmpDir, nil)
	if err != nil {
		os.RemoveAll(tmpDir)
		b.Fatalf("New: %v", err)
	}
	return c, func() {
		c.Close()
		os.RemoveAll(tmpDir)
	}
}

func BenchmarkPutDir(b *testing.B) {
	c, cleanup := setupBenchCache(b)
	defer cleanup()

	entries := make([]DirEntry, 10)
	for i := range entries {
		entries[i] = DirEntry{Name: fmt.Sprintf("file%d.txt", i), Mode: 0644, Ino: uint64(i + 1)}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		path := fmt.Sprintf("/bench/dir%d", i%50)
		_ = c.PutDir(path, entries)
	}
}

func BenchmarkGetDir(b *testing.B) {
	c, cleanup := setupBenchCache(b)
	defer cleanup()

	entries := make([]DirEntry, 10)
	for i := range entries {
		entries[i] = DirEntry{Name: fmt.Sprintf("file%d.txt", i), Mode: 0644, Ino: uint64(i + 1)}
	}
	for i := 0; i < 50; i++ {
		_ = c.PutDir(fmt.Sprintf("/bench/dir%d", i), entries)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		path := fmt.Sprintf("/bench/dir%d", i%50)
		_, _ = c.GetDir(path)
	}
}

func BenchmarkInvalidatePrefix(b *testing.B) {
	c, cleanup := setupBenchCache(b)
	defer cleanup()

	entry := &AttrEntry{Ino: 1, Mode: 0644, Size: 256}
	for i := 0; i < 100; i++ {
		_ = c.PutAttr(fmt.Sprintf("/repo/path/file%d.go", i), entry)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Re-populate then invalidate to avoid exhausting the dataset
		if i%10 == 0 {
			for j := 0; j < 10; j++ {
				_ = c.PutAttr(fmt.Sprintf("/repo/path/file%d.go", j), entry)
			}
		}
		c.Invalidate(fmt.Sprintf("/repo/path/file%d.go", i%100))
	}
}

func BenchmarkPutAttrLargeTree(b *testing.B) {
	c, cleanup := setupBenchCache(b)
	defer cleanup()

	entry := &AttrEntry{Ino: 42, Mode: 0644, Size: 512}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		path := fmt.Sprintf("/repo/%d/%d/file.go", i%20, i%50)
		_ = c.PutAttr(path, entry)
	}
}

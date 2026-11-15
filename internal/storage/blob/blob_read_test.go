package blob

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPathWithinDir(t *testing.T) {
	dir := filepath.Join(string(filepath.Separator), "archives", "abc")
	cases := []struct {
		path string
		want bool
	}{
		{dir, true},
		{filepath.Join(dir, "file.pack"), true},
		{filepath.Join(string(filepath.Separator), "archives", "abcd"), false},
		{filepath.Join(string(filepath.Separator), "archives", "abcd", "file.pack"), false},
		{filepath.Join(string(filepath.Separator), "archives"), false},
	}
	for _, tc := range cases {
		if got := pathWithinDir(tc.path, dir); got != tc.want {
			t.Errorf("pathWithinDir(%q, %q) = %v, want %v", tc.path, dir, got, tc.want)
		}
	}
}

func TestBlobBackend_CleanupStoragePrefixIsolation(t *testing.T) {
	backend := newTestBlobBackend(t)
	archiveDir := filepath.Join(backend.config.CacheDir, "archives")
	abcDir := filepath.Join(archiveDir, "abc")
	abcdDir := filepath.Join(archiveDir, "abcd")
	for _, dir := range []string{abcDir, abcdDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	abcArchive := filepath.Join(abcDir, "blob.pack")
	abcdArchive := filepath.Join(abcdDir, "blob.pack")
	for _, path := range []string{abcArchive, abcdArchive} {
		if err := os.WriteFile(path, []byte("archive"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	backend.mu.Lock()
	backend.blobIndex["abc-hash"] = archiveRef{archivePath: abcArchive, entryPath: "abc-hash"}
	backend.blobIndex["abcd-hash"] = archiveRef{archivePath: abcdArchive, entryPath: "abcd-hash"}
	backend.archivePaths[abcArchive] = true
	backend.archivePaths[abcdArchive] = true
	backend.storageIDs["abc"] = true
	backend.storageIDs["abcd"] = true
	backend.storageBlobCounts["abc"] = 1
	backend.storageBlobCounts["abcd"] = 1
	backend.mu.Unlock()

	if err := backend.Cleanup(context.Background(), "abc"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	backend.mu.RLock()
	defer backend.mu.RUnlock()
	if _, ok := backend.blobIndex["abc-hash"]; ok {
		t.Error("abc blob still indexed")
	}
	if _, ok := backend.blobIndex["abcd-hash"]; !ok {
		t.Error("Cleanup removed a sibling storage ID with a shared prefix")
	}
	if backend.archivePaths[abcArchive] {
		t.Error("abc archive still tracked")
	}
	if !backend.archivePaths[abcdArchive] {
		t.Error("abcd archive was untracked")
	}
	if backend.storageIDs["abc"] {
		t.Error("abc storage ID still tracked")
	}
	if !backend.storageIDs["abcd"] {
		t.Error("abcd storage ID was removed")
	}
	if _, ok := backend.storageBlobCounts["abc"]; ok {
		t.Error("abc blob count still present")
	}
	if backend.storageBlobCounts["abcd"] != 1 {
		t.Error("abcd blob count was removed")
	}

	if _, err := os.Stat(abcDir); !os.IsNotExist(err) {
		t.Errorf("abc archive dir still exists: %v", err)
	}
	if _, err := os.Stat(abcdDir); err != nil {
		t.Errorf("abcd archive dir was removed: %v", err)
	}
}

func TestBlobBackend_CloseIdempotent(t *testing.T) {
	backend := newTestBlobBackend(t)
	if err := backend.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if backend.archiveCache != nil {
		t.Fatal("archive cache was not cleared")
	}
}

func TestBlobBackend_QueueCloudUploadDuringClose(t *testing.T) {
	backend := newTestBlobBackend(t)
	backend.uploadArchiveFunc = func(context.Context, string) error { return nil }
	backend.startCloudUploadWorkers()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A producer racing Close may fail, but must never panic with
			// "send on closed channel".
			_ = backend.queueCloudUpload(fmt.Sprintf("/tmp/archive-%d.pack", i))
		}(i)
	}

	closed := make(chan error, 1)
	go func() { closed <- backend.Close() }()

	wg.Wait()
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := backend.queueCloudUpload("/tmp/after-close.pack"); err == nil {
		t.Fatal("queueCloudUpload accepted work after Close")
	}
}

func TestApplyFetchRange(t *testing.T) {
	data := []byte("0123456789")
	cases := []struct {
		name string
		cfg  map[string]string
		want string
	}{
		{"full", nil, "0123456789"},
		{"offset", map[string]string{"offset": "3"}, "3456789"},
		{"length", map[string]string{"length": "4"}, "0123"},
		{"offset and length", map[string]string{"offset": "2", "length": "3"}, "234"},
		{"negative values ignored", map[string]string{"offset": "-2", "length": "-3"}, "0123456789"},
		{"invalid values ignored", map[string]string{"offset": "x", "length": "y"}, "0123456789"},
		{"offset at end", map[string]string{"offset": "10"}, ""},
		{"offset past end", map[string]string{"offset": "20"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyFetchRange(data, tc.cfg)
			if string(got) != tc.want {
				t.Fatalf("applyFetchRange = %q, want %q", got, tc.want)
			}
			if got == nil {
				t.Fatal("applyFetchRange returned nil; callers should receive an empty slice")
			}
		})
	}
}

func FuzzApplyFetchRange(f *testing.F) {
	f.Add("0123456789", "3", "4")
	f.Add("", "0", "0")
	f.Add("abc", "99", "2")
	f.Add("abc", "-1", "-1")
	f.Fuzz(func(t *testing.T, data, offset, length string) {
		got := applyFetchRange([]byte(data), map[string]string{"offset": offset, "length": length})
		if len(got) > len(data) {
			t.Fatalf("result grew from %d to %d bytes", len(data), len(got))
		}
	})
}

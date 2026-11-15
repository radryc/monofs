package guardian

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func commitVersions(t *testing.T, store *Store, path string, count int) []string {
	t.Helper()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		version, err := store.Commit(Commit{
			LogicalPath:     path,
			BatchRevisionID: fmt.Sprintf("rev-%d", i),
			Content:         []byte(fmt.Sprintf("v%d", i)),
			CommittedAt:     int64(i + 1),
		})
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		ids = append(ids, version.GetVersionId())
	}
	return ids
}

func TestStoreListPagination(t *testing.T) {
	factories := map[string]func(t *testing.T) *Store{
		"memory": func(t *testing.T) *Store {
			store, err := NewStore("")
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			t.Cleanup(store.Close)
			return store
		},
		"persistent": func(t *testing.T) *Store {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			t.Cleanup(store.Close)
			return store
		},
	}

	for name, newStore := range factories {
		t.Run(name, func(t *testing.T) {
			store := newStore(t)
			path := "/partitions/demo/config.yaml"
			ids := commitVersions(t, store, path, 5)

			page, next, err := store.List(path, 2, "")
			if err != nil {
				t.Fatalf("List page 1: %v", err)
			}
			if len(page) != 2 || next != "2" {
				t.Fatalf("page 1 = %d entries next=%q", len(page), next)
			}
			// History is newest first.
			if page[0].GetVersionId() != ids[4] || page[1].GetVersionId() != ids[3] {
				t.Fatalf("page 1 ids = %q, %q", page[0].GetVersionId(), page[1].GetVersionId())
			}

			page, next, err = store.List(path, 2, next)
			if err != nil {
				t.Fatalf("List page 2: %v", err)
			}
			if len(page) != 2 || next != "4" {
				t.Fatalf("page 2 = %d entries next=%q", len(page), next)
			}

			page, next, err = store.List(path, 2, next)
			if err != nil {
				t.Fatalf("List page 3: %v", err)
			}
			if len(page) != 1 || next != "" {
				t.Fatalf("page 3 = %d entries next=%q", len(page), next)
			}

			// An offset beyond the end is empty, not an error.
			page, next, err = store.List(path, 2, "99")
			if err != nil || len(page) != 0 || next != "" {
				t.Fatalf("out-of-range page = %d next=%q err=%v", len(page), next, err)
			}

			// pageSize <= 0 falls back to the default and returns everything.
			page, _, err = store.List(path, 0, "")
			if err != nil || len(page) != 5 {
				t.Fatalf("default page size = %d err=%v", len(page), err)
			}

			if page, _, err := store.List(path, 2, " 2 "); err != nil || len(page) != 2 {
				t.Fatalf("padded page token = %d entries, err=%v", len(page), err)
			}

			for _, token := range []string{"abc", "-1"} {
				if _, _, err := store.List(path, 2, token); err == nil {
					t.Fatalf("token %q should be rejected", token)
				}
			}

			if _, _, err := store.List("/partitions/missing/config.yaml", 2, ""); err != nil {
				t.Fatalf("missing path List error = %v", err)
			}
		})
	}
}

func TestStoreListEphemeralHistory(t *testing.T) {
	store, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	path := "/.queues/p/task-1.json"
	first, err := store.Commit(Commit{LogicalPath: path, Content: []byte("one"), CommittedAt: 1})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	versions, next, err := store.List(path, 10, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(versions) != 1 || versions[0].GetVersionId() != first.GetVersionId() || next != "" {
		t.Fatalf("ephemeral list = %+v next=%q", versions, next)
	}
	if _, _, err := store.List(path, 10, "1"); err != nil {
		t.Fatalf("offset listing: %v", err)
	}
}

func TestStoreGetVersionKinds(t *testing.T) {
	factories := map[string]func(t *testing.T) *Store{
		"memory": func(t *testing.T) *Store {
			store, err := NewStore("")
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			t.Cleanup(store.Close)
			return store
		},
		"persistent": func(t *testing.T) *Store {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			t.Cleanup(store.Close)
			return store
		},
	}

	for name, newStore := range factories {
		t.Run(name, func(t *testing.T) {
			store := newStore(t)
			path := "/partitions/demo/config.yaml"

			created, err := store.Commit(Commit{LogicalPath: path, Content: []byte("body"), CommittedAt: 1})
			if err != nil {
				t.Fatalf("commit: %v", err)
			}
			version, content, available, found, err := store.Get(path, created.GetVersionId())
			if err != nil || !found || !available || string(content) != "body" {
				t.Fatalf("Get = %+v %q available=%v found=%v err=%v", version, content, available, found, err)
			}
			// Returned content must be a copy.
			content[0] = 'X'
			if _, again, _, _, _ := store.Get(path, created.GetVersionId()); string(again) != "body" {
				t.Fatalf("Get returned aliased content: %q", again)
			}

			if _, _, _, found, err := store.Get(path, "ver-missing"); err != nil || found {
				t.Fatalf("missing version = found=%v err=%v", found, err)
			}

			empty, err := store.Commit(Commit{LogicalPath: path, Content: []byte{}, CommittedAt: 2})
			if err != nil {
				t.Fatalf("commit empty: %v", err)
			}
			version, content, available, found, err = store.Get(path, empty.GetVersionId())
			if err != nil || !found {
				t.Fatalf("empty Get = %+v found=%v err=%v", version, found, err)
			}
			if !available || len(content) != 0 {
				t.Fatalf("empty Get available=%v content=%q", available, content)
			}

			tombstone, err := store.Commit(Commit{LogicalPath: path, Tombstone: true, CommittedAt: 3})
			if err != nil {
				t.Fatalf("commit tombstone: %v", err)
			}
			version, content, available, found, err = store.Get(path, tombstone.GetVersionId())
			if err != nil || !found || available || content != nil || !version.GetTombstone() {
				t.Fatalf("tombstone Get = %+v %q available=%v found=%v err=%v", version, content, available, found, err)
			}
		})
	}
}

func TestStoreCacheContent(t *testing.T) {
	store, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	path := "/partitions/demo/config.yaml"

	if _, ok := store.CachedContent(path, "v1"); ok {
		t.Fatal("unexpected cache hit")
	}

	// Empty content is identified by its SHA and is never cached.
	store.CacheContent(path, "empty", nil)
	if _, ok := store.CachedContent(path, "empty"); ok {
		t.Fatal("empty content should not be cached")
	}

	store.CacheContent(path, "v1", []byte("one"))
	store.CacheContent(path, "v1", []byte("one"))
	store.mu.RLock()
	defer store.mu.RUnlock()
	if len(store.contentCache) != 1 {
		t.Fatalf("cache entries = %d, want 1", len(store.contentCache))
	}
	if got := store.contentBytes; got != 3 {
		t.Fatalf("content bytes = %d, want 3", got)
	}
}

func TestStoreListCurrentUnderPrefixLimitAndOrder(t *testing.T) {
	store, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	for _, path := range []string{
		"/partitions/p/b.yaml",
		"/partitions/p/a.yaml",
		"/partitions/p/c.yaml",
		"/partitions/other/d.yaml",
	} {
		if _, err := store.Commit(Commit{LogicalPath: path, Content: []byte("x"), CommittedAt: 1}); err != nil {
			t.Fatalf("commit %s: %v", path, err)
		}
	}

	// The prefix without a trailing slash must be normalised.
	versions := store.ListCurrentUnderPrefix("/partitions/p", 2)
	if len(versions) != 2 {
		t.Fatalf("limit = %d, want 2", len(versions))
	}
	if versions[0].GetLogicalPath() != "/partitions/p/a.yaml" || versions[1].GetLogicalPath() != "/partitions/p/b.yaml" {
		t.Fatalf("prefix result order = %q, %q", versions[0].GetLogicalPath(), versions[1].GetLogicalPath())
	}

	// An empty prefix lists everything, sorted.
	all := store.ListCurrentUnderPrefix("", 10)
	if len(all) != 4 {
		t.Fatalf("all = %d, want 4", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].GetLogicalPath() > all[i].GetLogicalPath() {
			t.Fatalf("result not sorted: %q > %q", all[i-1].GetLogicalPath(), all[i].GetLogicalPath())
		}
	}
}

func TestStoreCommitBatchIsAtomic(t *testing.T) {
	factories := map[string]func(t *testing.T) *Store{
		"memory": func(t *testing.T) *Store {
			store, err := NewStore("")
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			t.Cleanup(store.Close)
			return store
		},
		"persistent": func(t *testing.T) *Store {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			t.Cleanup(store.Close)
			return store
		},
	}

	for name, newStore := range factories {
		t.Run(name, func(t *testing.T) {
			store := newStore(t)
			existing := "/partitions/demo/existing.yaml"
			fresh := "/partitions/demo/fresh.yaml"
			if _, err := store.Commit(Commit{LogicalPath: existing, Content: []byte("x"), CommittedAt: 1}); err != nil {
				t.Fatalf("seed commit: %v", err)
			}
			seqBefore := store.seq

			_, err := store.CommitBatch([]Commit{
				{LogicalPath: fresh, Content: []byte("new"), CommittedAt: 2},
				{LogicalPath: existing, Content: []byte("y"), ExpectedVersionID: "absent", CommittedAt: 3},
			})
			if !errors.Is(err, ErrExists) {
				t.Fatalf("CommitBatch error = %v, want ErrExists", err)
			}
			if _, ok := store.CurrentVersion(fresh); ok {
				t.Fatal("earlier change in a failed batch was committed")
			}
			if store.seq != seqBefore {
				t.Fatalf("seq = %d after failed batch, want %d", store.seq, seqBefore)
			}
		})
	}
}

func TestStoreCommitBatchDoesNotApplyEphemeralOnFailure(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	assetPath := "/partitions/demo/existing.yaml"
	if _, err := store.Commit(Commit{LogicalPath: assetPath, Content: []byte("x"), CommittedAt: 1}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	machineryPath := "/.queues/p/task-1.json"
	_, err = store.CommitBatch([]Commit{
		{LogicalPath: machineryPath, Content: []byte("task"), CommittedAt: 2},
		{LogicalPath: assetPath, Content: []byte("y"), ExpectedVersionID: "absent", CommittedAt: 3},
	})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("CommitBatch error = %v, want ErrExists", err)
	}
	if _, ok := store.CurrentVersion(machineryPath); ok {
		t.Fatal("machinery change was applied even though the asset transaction failed")
	}
}

func TestStoreNewStoreErrors(t *testing.T) {
	t.Run("state dir is a file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewStore(file); err == nil {
			t.Fatal("expected an error when the state dir is a regular file")
		}
	})

	t.Run("corrupt legacy snapshot", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "guardian_versions.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewStore(dir); err == nil {
			t.Fatal("expected an error for a corrupt legacy snapshot")
		}
	})
}

func TestIsAssetPathExtended(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"", true},
		{"/", true},
		{"/partitions/p/config.yaml", true},
		{"/partitions/p/.state/partition.json", true},
		{"partitions/p/.state/partition.json", true},
		{"/partitions/p/.state/other.json", true},
		{"/partitions/p/.git/config", false},
		{"/partitions/p/.hidden/file", false},
		{"/.registry", true},
		{"/.access/envs/dev.yaml", true},
		{"/.queues", false},
		{"/.archive/x", false},
	}
	for _, tc := range cases {
		if got := IsAssetPath(tc.path); got != tc.want {
			t.Errorf("IsAssetPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestVersionIDStability(t *testing.T) {
	first := VersionID("/partitions/p/config.yaml", "sha", "rev", 1, 1, false)
	if again := VersionID("/partitions/p/config.yaml", "sha", "rev", 1, 1, false); again != first {
		t.Fatalf("VersionID is not deterministic: %q != %q", again, first)
	}
	variants := []string{
		VersionID("/partitions/p/config.yaml", "sha", "rev", 2, 1, false),
		VersionID("/partitions/p/config.yaml", "sha", "rev", 1, 2, false),
		VersionID("/partitions/p/config.yaml", "sha", "rev", 1, 1, true),
		VersionID("/partitions/p/other.yaml", "sha", "rev", 1, 1, false),
	}
	for _, variant := range variants {
		if variant == first {
			t.Fatalf("VersionID did not change for a different input: %q", variant)
		}
	}
}

func TestRecordToProtoNil(t *testing.T) {
	var record *Record
	if got := record.ToProto(); got != nil {
		t.Fatalf("ToProto on nil = %+v", got)
	}
}

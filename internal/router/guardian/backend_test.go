package guardian

import (
	"context"
	"errors"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
)

// fakeBackend records calls and returns scripted results so the Store's
// backend routing, fallback and ordering contract can be tested without a KVS.
type fakeBackend struct {
	currentFn func(ctx context.Context, logicalPath string) (*Record, bool, error)
	commitFn  func(ctx context.Context, changes []Commit) ([]*pb.GuardianFileVersion, error)
	listFn    func(ctx context.Context, logicalPath string, pageSize int32, pageToken string) ([]*pb.GuardianFileVersion, string, error)
	getFn     func(ctx context.Context, logicalPath, versionID string) (*pb.GuardianFileVersion, []byte, bool, bool, error)

	commitCalls   int
	committed     []Commit
	currentCalls  int
	listCalls     int
	getCalls      int
	closed        bool
	lastPageSize  int32
	lastPageToken string
}

func (f *fakeBackend) Current(ctx context.Context, logicalPath string) (*Record, bool, error) {
	f.currentCalls++
	if f.currentFn != nil {
		return f.currentFn(ctx, logicalPath)
	}
	return nil, false, nil
}

func (f *fakeBackend) CommitBatch(ctx context.Context, changes []Commit) ([]*pb.GuardianFileVersion, error) {
	f.commitCalls++
	f.committed = append(f.committed, changes...)
	if f.commitFn != nil {
		return f.commitFn(ctx, changes)
	}
	versions := make([]*pb.GuardianFileVersion, len(changes))
	for i, change := range changes {
		versions[i] = &pb.GuardianFileVersion{
			LogicalPath: change.LogicalPath,
			VersionId:   "backend-" + change.LogicalPath,
		}
	}
	return versions, nil
}

func (f *fakeBackend) List(ctx context.Context, logicalPath string, pageSize int32, pageToken string) ([]*pb.GuardianFileVersion, string, error) {
	f.listCalls++
	f.lastPageSize = pageSize
	f.lastPageToken = pageToken
	if f.listFn != nil {
		return f.listFn(ctx, logicalPath, pageSize, pageToken)
	}
	return nil, "", nil
}

func (f *fakeBackend) Get(ctx context.Context, logicalPath, versionID string) (*pb.GuardianFileVersion, []byte, bool, bool, error) {
	f.getCalls++
	if f.getFn != nil {
		return f.getFn(ctx, logicalPath, versionID)
	}
	return nil, nil, false, false, nil
}

func (f *fakeBackend) Close() error {
	f.closed = true
	return nil
}

func newBackendTestStore(t *testing.T, backend Backend, fallback bool) *Store {
	t.Helper()
	store, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)
	store.UseBackend(backend, fallback)
	return store
}

func TestStoreBackendRoutesAssetPaths(t *testing.T) {
	backend := &fakeBackend{}
	store := newBackendTestStore(t, backend, false)

	version, err := store.Commit(Commit{
		LogicalPath: "/partitions/demo/intents/app.yaml",
		Content:     []byte("v1"),
		CommittedAt: 1,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if backend.commitCalls != 1 || len(backend.committed) != 1 {
		t.Fatalf("backend commit calls = %d, changes = %d", backend.commitCalls, len(backend.committed))
	}
	if version.GetVersionId() != "backend-/partitions/demo/intents/app.yaml" {
		t.Fatalf("version = %q", version.GetVersionId())
	}

	// The local store must not have recorded the asset version.
	if _, ok := store.CurrentVersion("/partitions/demo/intents/app.yaml"); ok {
		t.Fatal("asset path was also committed locally")
	}
}

func TestStoreBackendKeepsMachineryLocal(t *testing.T) {
	backend := &fakeBackend{}
	store := newBackendTestStore(t, backend, false)

	changes := []Commit{
		{LogicalPath: "/.queues/p/task-1.json", Content: []byte("q"), CommittedAt: 1},
		{LogicalPath: "/partitions/demo/config.yaml", Content: []byte("a"), CommittedAt: 2},
		{LogicalPath: "/.queues/p/task-2.json", Content: []byte("r"), CommittedAt: 3},
	}
	versions, err := store.CommitBatch(changes)
	if err != nil {
		t.Fatalf("CommitBatch: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("versions = %d, want 3", len(versions))
	}
	if len(backend.committed) != 1 || backend.committed[0].LogicalPath != "/partitions/demo/config.yaml" {
		t.Fatalf("backend changes = %+v", backend.committed)
	}
	// Ordering must follow the input batch, not the split.
	if versions[0].GetVersionId() == "" || versions[1].GetVersionId() != "backend-/partitions/demo/config.yaml" || versions[2].GetVersionId() == "" {
		t.Fatalf("versions out of order: %+v", versions)
	}
	if _, ok := store.CurrentVersion("/.queues/p/task-1.json"); !ok {
		t.Fatal("machinery path was not committed locally")
	}
}

func TestStoreBackendFallback(t *testing.T) {
	backendErr := errors.New("kvs unavailable")
	assetPath := "/partitions/demo/intents/app.yaml"

	t.Run("disabled surfaces the error", func(t *testing.T) {
		backend := &fakeBackend{
			commitFn: func(context.Context, []Commit) ([]*pb.GuardianFileVersion, error) {
				return nil, backendErr
			},
			currentFn: func(context.Context, string) (*Record, bool, error) { return nil, false, backendErr },
			listFn: func(context.Context, string, int32, string) ([]*pb.GuardianFileVersion, string, error) {
				return nil, "", backendErr
			},
			getFn: func(context.Context, string, string) (*pb.GuardianFileVersion, []byte, bool, bool, error) {
				return nil, nil, false, false, backendErr
			},
		}
		store := newBackendTestStore(t, backend, false)

		if _, err := store.CommitBatch([]Commit{{LogicalPath: assetPath, CommittedAt: 1}}); !errors.Is(err, backendErr) {
			t.Fatalf("CommitBatch error = %v, want %v", err, backendErr)
		}
		if _, _, err := store.CurrentVersionWithError(assetPath); !errors.Is(err, backendErr) {
			t.Fatalf("CurrentVersionWithError error = %v", err)
		}
		if _, _, err := store.List(assetPath, 10, ""); !errors.Is(err, backendErr) {
			t.Fatalf("List error = %v", err)
		}
		if _, _, _, _, err := store.Get(assetPath, "v1"); !errors.Is(err, backendErr) {
			t.Fatalf("Get error = %v", err)
		}
	})

	t.Run("enabled serves from local", func(t *testing.T) {
		backend := &fakeBackend{
			commitFn: func(context.Context, []Commit) ([]*pb.GuardianFileVersion, error) {
				return nil, backendErr
			},
			currentFn: func(context.Context, string) (*Record, bool, error) { return nil, false, backendErr },
			listFn: func(context.Context, string, int32, string) ([]*pb.GuardianFileVersion, string, error) {
				return nil, "", backendErr
			},
			getFn: func(context.Context, string, string) (*pb.GuardianFileVersion, []byte, bool, bool, error) {
				return nil, nil, false, false, backendErr
			},
		}
		store := newBackendTestStore(t, backend, true)

		version, err := store.Commit(Commit{LogicalPath: assetPath, Content: []byte("v1"), CommittedAt: 1})
		if err != nil {
			t.Fatalf("Commit with fallback: %v", err)
		}
		current, ok := store.CurrentVersion(assetPath)
		if !ok || current.VersionID != version.GetVersionId() {
			t.Fatalf("local current = %+v, ok=%v", current, ok)
		}
		if _, _, err := store.List(assetPath, 10, ""); err != nil {
			t.Fatalf("List with fallback: %v", err)
		}
		if _, _, available, found, err := store.Get(assetPath, version.GetVersionId()); err != nil || !found || !available {
			t.Fatalf("Get with fallback = available=%v found=%v err=%v", available, found, err)
		}
	})
}

func TestStoreCommitBatchRejectsShortBackendResponse(t *testing.T) {
	backend := &fakeBackend{
		commitFn: func(context.Context, []Commit) ([]*pb.GuardianFileVersion, error) {
			return nil, nil
		},
	}

	store := newBackendTestStore(t, backend, false)
	if _, err := store.CommitBatch([]Commit{{LogicalPath: "/partitions/demo/config.yaml", CommittedAt: 1}}); err == nil {
		t.Fatal("expected an error for a short backend response")
	}
	if _, ok := store.CurrentVersion("/partitions/demo/config.yaml"); ok {
		t.Fatal("asset should not be committed locally when fallback is disabled")
	}
}

func TestStoreUseBackendNilRestoresLocal(t *testing.T) {
	backend := &fakeBackend{}
	store := newBackendTestStore(t, backend, false)
	store.UseBackend(nil, false)

	path := "/partitions/demo/config.yaml"
	if _, err := store.Commit(Commit{LogicalPath: path, Content: []byte("x"), CommittedAt: 1}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if backend.commitCalls != 0 {
		t.Fatalf("backend called %d times after being disabled", backend.commitCalls)
	}
	if _, ok := store.CurrentVersion(path); !ok {
		t.Fatal("local commit missing after disabling backend")
	}
}

func TestStoreCurrentVersionHydratesCachedContent(t *testing.T) {
	backend := &fakeBackend{
		currentFn: func(context.Context, string) (*Record, bool, error) {
			return &Record{LogicalPath: "/partitions/demo/config.yaml", VersionID: "ver-1"}, true, nil
		},
	}
	store := newBackendTestStore(t, backend, false)

	content := []byte("shared content")
	store.CacheContent("/partitions/demo/config.yaml", "ver-1", content)

	current, ok := store.CurrentVersion("/partitions/demo/config.yaml")
	if !ok || string(current.Content) != string(content) {
		t.Fatalf("current = %+v, ok=%v, want cached content", current, ok)
	}

	// CachedContent must return a copy.
	current.Content[0] = 'X'
	if cached, _ := store.CachedContent("/partitions/demo/config.yaml", "ver-1"); string(cached) != string(content) {
		t.Fatalf("cache was mutated through the returned slice: %q", cached)
	}
}

func TestStoreListAndGetDelegateToBackend(t *testing.T) {
	backend := &fakeBackend{
		listFn: func(_ context.Context, _ string, pageSize int32, pageToken string) ([]*pb.GuardianFileVersion, string, error) {
			return []*pb.GuardianFileVersion{{LogicalPath: "/partitions/demo/config.yaml", VersionId: "v2"}}, "next", nil
		},
		getFn: func(context.Context, string, string) (*pb.GuardianFileVersion, []byte, bool, bool, error) {
			return &pb.GuardianFileVersion{LogicalPath: "/partitions/demo/config.yaml", VersionId: "v2"}, []byte("body"), true, true, nil
		},
	}
	store := newBackendTestStore(t, backend, false)

	versions, next, err := store.List("/partitions/demo/config.yaml", 7, "2")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(versions) != 1 || versions[0].GetVersionId() != "v2" || next != "next" {
		t.Fatalf("List = %+v next=%q", versions, next)
	}
	if backend.lastPageSize != 7 || backend.lastPageToken != "2" {
		t.Fatalf("backend got pageSize=%d token=%q", backend.lastPageSize, backend.lastPageToken)
	}

	version, content, available, found, err := store.Get("/partitions/demo/config.yaml", "v2")
	if err != nil || !found || !available || string(content) != "body" || version.GetVersionId() != "v2" {
		t.Fatalf("Get = %+v %q available=%v found=%v err=%v", version, content, available, found, err)
	}
}

func TestStoreBackendClosePropagates(t *testing.T) {
	backend := &fakeBackend{}
	store, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	store.UseBackend(backend, false)
	store.Close()
	if !backend.closed {
		t.Fatal("Close did not close the backend")
	}
	// Second Close must be safe.
	store.Close()
}

package guardian

import (
	"testing"
	"time"
)

func TestGuardianVersionStoreEphemeralTombstoneEvictsMemory(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	path := "/.queues/aws-1/tasks/task-1.json"
	nowUnix := time.Now().UTC().Unix()
	v1, err := store.Commit(Commit{
		LogicalPath:     path,
		BatchRevisionID: "rev-1",
		Content:         []byte(`{"id":"task-1"}`),
		CommittedAt:     nowUnix,
	})
	if err != nil {
		t.Fatalf("commit create: %v", err)
	}
	if _, ok := store.CurrentVersion(path); !ok {
		t.Fatalf("expected ephemeral entry to exist before delete")
	}

	// Sub-second overwrite with identical content and CommittedAt must produce a distinct VersionID.
	v2, err := store.Commit(Commit{
		LogicalPath:     path,
		BatchRevisionID: "rev-2",
		Content:         []byte(`{"id":"task-1"}`),
		CommittedAt:     nowUnix,
	})
	if err != nil {
		t.Fatalf("commit update: %v", err)
	}
	if v1.GetVersionId() == v2.GetVersionId() {
		t.Fatalf("expected distinct VersionIDs on sub-second commit, got %q", v1.GetVersionId())
	}

	// Tombstone commit on machinery path must evict from s.ephemeral so 50K pushers never leak RAM.
	if _, err := store.Commit(Commit{
		LogicalPath:     path,
		BatchRevisionID: "rev-3",
		Tombstone:       true,
		CommittedAt:     nowUnix,
	}); err != nil {
		t.Fatalf("commit tombstone: %v", err)
	}
	store.mu.RLock()
	_, exists := store.ephemeral[path]
	store.mu.RUnlock()
	if exists {
		t.Fatalf("expected tombstoned machinery path %s to be evicted from s.ephemeral", path)
	}
}

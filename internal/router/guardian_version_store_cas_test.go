package router

import (
	"errors"
	"testing"
)

func TestGuardianVersionStoreExpectedVersionCAS(t *testing.T) {
	store, err := newGuardianVersionStore(t.TempDir())
	if err != nil {
		t.Fatalf("newGuardianVersionStore: %v", err)
	}
	defer store.close()

	path := "/partitions/demo/.state/intents/app.json"
	first, err := store.commit(guardianVersionCommit{
		LogicalPath:       path,
		DisplayPath:       "guardian/demo",
		StorageID:         "storage-demo",
		Content:           []byte("v1"),
		ExpectedVersionID: "absent",
		CommittedAt:       1,
	})
	if err != nil {
		t.Fatalf("absent create: %v", err)
	}

	if _, err := store.commit(guardianVersionCommit{
		LogicalPath:       path,
		Content:           []byte("v2"),
		ExpectedVersionID: "absent",
		CommittedAt:       2,
	}); !errors.Is(err, errGuardianVersionExists) {
		t.Fatalf("second absent create error = %v, want exists", err)
	}

	if _, err := store.commit(guardianVersionCommit{
		LogicalPath:       path,
		Content:           []byte("v2"),
		ExpectedVersionID: "wrong-version",
		CommittedAt:       3,
	}); !errors.Is(err, errGuardianVersionConflict) {
		t.Fatalf("wrong expected version error = %v, want conflict", err)
	}

	second, err := store.commit(guardianVersionCommit{
		LogicalPath:       path,
		Content:           []byte("v2"),
		ExpectedVersionID: first.GetVersionId(),
		CommittedAt:       4,
	})
	if err != nil {
		t.Fatalf("matching expected version: %v", err)
	}

	if _, err := store.commit(guardianVersionCommit{
		LogicalPath:       path,
		Tombstone:         true,
		ExpectedVersionID: second.GetVersionId(),
		CommittedAt:       5,
	}); err != nil {
		t.Fatalf("tombstone: %v", err)
	}

	if _, err := store.commit(guardianVersionCommit{
		LogicalPath:       path,
		Content:           []byte("v3"),
		ExpectedVersionID: "absent",
		CommittedAt:       6,
	}); err != nil {
		t.Fatalf("absent create after tombstone: %v", err)
	}
}

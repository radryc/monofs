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

func TestIsGuardianAssetPathUsesSharedBackendForControlPlaneState(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/partitions/demo/config.yaml", true},
		{"/partitions/demo/intents/app.yaml", true},
		{"/partitions/demo/.state/intents/app.json", true},
		{"/partitions/demo/.state/partition.json", true},
		{"/.registry/pushers/aws-1.json", true},
		{"/.access/envs/dev.yaml", true},
		{"/.queues/k8s-main/task_1.json", false},
		{"/.queues/k8s-main/.claims/task_1.json", false},
		{"/.archive/demo/app/rev_1/manifest.yaml", false},
	}
	for _, tc := range cases {
		if got := isGuardianAssetPath(tc.path); got != tc.want {
			t.Errorf("isGuardianAssetPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

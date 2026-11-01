package router

import "testing"

func TestGuardianVersionListCurrentUnderPrefix(t *testing.T) {
	store, err := newGuardianVersionStore("")
	if err != nil {
		t.Fatalf("newGuardianVersionStore: %v", err)
	}

	commits := []guardianVersionCommit{
		{LogicalPath: "/.queues/pipeline/run-1/tasks/a.json", Content: []byte(`{"a":1}`), CommittedAt: 1},
		{LogicalPath: "/.queues/pipeline/run-1/tasks/b.json", Content: []byte(`{"b":1}`), CommittedAt: 2},
		{LogicalPath: "/.queues/pipeline/run-2/tasks/c.json", Content: []byte(`{"c":1}`), CommittedAt: 3},
		{LogicalPath: "/partitions/p1/assets/intent.yaml", Content: []byte("kind: Intent"), CommittedAt: 4},
	}
	for _, commit := range commits {
		if _, err := store.commit(commit); err != nil {
			t.Fatalf("commit %s: %v", commit.LogicalPath, err)
		}
	}

	versions := store.listCurrentUnderPrefix("/.queues/pipeline/run-1/tasks", 10)
	if len(versions) != 2 {
		t.Fatalf("prefix result count = %d, want 2: %+v", len(versions), versions)
	}
	if versions[0].GetLogicalPath() != "/.queues/pipeline/run-1/tasks/a.json" ||
		versions[1].GetLogicalPath() != "/.queues/pipeline/run-1/tasks/b.json" {
		t.Fatalf("prefix result paths = %q, %q", versions[0].GetLogicalPath(), versions[1].GetLogicalPath())
	}

	if _, err := store.commit(guardianVersionCommit{
		LogicalPath: "/.queues/pipeline/run-1/tasks/a.json",
		Tombstone:   true,
		CommittedAt: 5,
	}); err != nil {
		t.Fatalf("tombstone commit: %v", err)
	}
	versions = store.listCurrentUnderPrefix("/.queues/pipeline/run-1/tasks", 10)
	if len(versions) != 1 || versions[0].GetLogicalPath() != "/.queues/pipeline/run-1/tasks/b.json" {
		t.Fatalf("after tombstone = %+v, want only b.json", versions)
	}

	assets := store.listCurrentUnderPrefix("/partitions", 10)
	if len(assets) != 1 || assets[0].GetLogicalPath() != "/partitions/p1/assets/intent.yaml" {
		t.Fatalf("asset prefix = %+v", assets)
	}
}

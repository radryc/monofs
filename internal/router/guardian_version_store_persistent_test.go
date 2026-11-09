package router

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGuardianVersionStorePersistentRoundTrip(t *testing.T) {
	stateDir := t.TempDir()
	store, err := newGuardianVersionStore(stateDir)
	if err != nil {
		t.Fatalf("newGuardianVersionStore: %v", err)
	}

	logicalPath := "/partitions/demo/intents/api.yaml"
	content := []byte("kind: Intent\n")
	first, err := store.commit(guardianVersionCommit{
		LogicalPath:     logicalPath,
		DisplayPath:     "guardian/demo",
		StorageID:       "storage-demo",
		BatchRevisionID: "rev-1",
		Content:         content,
		CommittedAt:     time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	current, ok := store.currentVersion(logicalPath)
	if !ok || current.VersionID != first.GetVersionId() {
		t.Fatalf("current version = %+v, ok=%v", current, ok)
	}
	version, gotContent, contentAvailable, ok, err := store.get(logicalPath, first.GetVersionId())
	if !ok || version.GetVersionId() != first.GetVersionId() {
		t.Fatalf("get version = %+v, ok=%v", version, ok)
	}
	if string(gotContent) != string(content) || !contentAvailable {
		t.Fatalf("content = %q, available=%v, want %q", string(gotContent), contentAvailable, string(content))
	}

	store.close()

	reopened, err := newGuardianVersionStore(stateDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.close()

	current, ok = reopened.currentVersion(logicalPath)
	if !ok || current.VersionID != first.GetVersionId() {
		t.Fatalf("persisted current version = %+v, ok=%v", current, ok)
	}
	versions, _, err := reopened.list(logicalPath, 10, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(versions) != 1 || versions[0].GetVersionId() != first.GetVersionId() {
		t.Fatalf("persisted versions = %+v", versions)
	}
	if _, _, contentAvailable, ok, _ := reopened.get(logicalPath, first.GetVersionId()); !ok {
		t.Fatal("persisted version not found by id")
	} else if contentAvailable {
		t.Fatal("persisted version should not report cached content after reopen")
	}
}

func TestGuardianVersionStoreImportsLegacySnapshot(t *testing.T) {
	stateDir := t.TempDir()
	legacy := legacyGuardianVersionSnapshot{
		Records: map[string][]*storedGuardianFileVersion{
			"/partitions/demo/config.yaml": {
				{
					LogicalPath: "/partitions/demo/config.yaml",
					DisplayPath: "guardian/demo",
					StorageID:   "storage-demo",
					VersionID:   "ver_legacy_1",
					CommittedAt: 1,
				},
			},
		},
		Current: map[string]string{
			"/partitions/demo/config.yaml": "ver_legacy_1",
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "guardian_versions.json"), data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	store, err := newGuardianVersionStore(stateDir)
	if err != nil {
		t.Fatalf("newGuardianVersionStore: %v", err)
	}
	defer store.close()

	current, ok := store.currentVersion("/partitions/demo/config.yaml")
	if !ok || current.VersionID != "ver_legacy_1" {
		t.Fatalf("imported current = %+v, ok=%v", current, ok)
	}
	versions, _, err := store.list("/partitions/demo/config.yaml", 10, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(versions) != 1 || versions[0].GetVersionId() != "ver_legacy_1" {
		t.Fatalf("imported versions = %+v", versions)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "guardian_versions.json.migrated")); err != nil {
		t.Fatalf("legacy snapshot was not renamed: %v", err)
	}
}

func TestGuardianVersionStorePersistentTrimsHistory(t *testing.T) {
	store, err := newGuardianVersionStore(t.TempDir())
	if err != nil {
		t.Fatalf("newGuardianVersionStore: %v", err)
	}
	defer store.close()

	logicalPath := "/partitions/demo/config.yaml"
	now := time.Now().Unix()
	var last string
	for i := 0; i < guardianVersionsPerPathHistory+5; i++ {
		version, err := store.commit(guardianVersionCommit{
			LogicalPath:     logicalPath,
			BatchRevisionID: fmt.Sprintf("rev-%d", i),
			Content:         []byte(fmt.Sprintf("version-%d", i)),
			CommittedAt:     now + int64(i),
		})
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		last = version.GetVersionId()
	}
	versions, _, err := store.list(logicalPath, 100, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(versions) != guardianVersionsPerPathHistory {
		t.Fatalf("history length = %d, want %d", len(versions), guardianVersionsPerPathHistory)
	}
	if versions[0].GetVersionId() != last {
		t.Fatalf("newest version = %q, want %q", versions[0].GetVersionId(), last)
	}
}

func TestGuardianVersionStorePersistentPrefixListing(t *testing.T) {
	store, err := newGuardianVersionStore(t.TempDir())
	if err != nil {
		t.Fatalf("newGuardianVersionStore: %v", err)
	}
	defer store.close()

	paths := []string{
		"/partitions/a/config.yaml",
		"/partitions/a/intents/api.yaml",
		"/partitions/b/config.yaml",
	}
	for _, path := range paths {
		if _, err := store.commit(guardianVersionCommit{LogicalPath: path, Content: []byte("x"), CommittedAt: 1}); err != nil {
			t.Fatalf("commit %s: %v", path, err)
		}
	}
	versions := store.listCurrentUnderPrefix("/partitions/a", 100)
	if len(versions) != 2 {
		t.Fatalf("prefix listing = %d, want 2: %+v", len(versions), versions)
	}
}

package fuse

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestOverlayDB_VCSStatePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()

	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB failed: %v", err)
	}

	stagedAt := time.Date(2026, 5, 20, 10, 11, 12, 0, time.UTC)
	pushedAt := time.Date(2026, 5, 20, 10, 15, 0, 0, time.UTC)
	createdAt := time.Date(2026, 5, 20, 10, 12, 0, 0, time.UTC)
	mappingCreatedAt := time.Date(2026, 5, 20, 10, 13, 0, 0, time.UTC)

	staged := StagedIndexEntry{
		Path:                "github.com/acme/service-a/file.go",
		RepositoryStorageID: "repo-a",
		RepositoryPath:      "github.com/acme/service-a",
		ChangeType:          ChangeModify,
		StagedAt:            stagedAt,
		Content:             []byte("package servicea\n"),
		Mode:                0o644,
		LocalPath:           "/tmp/overlay/service-a/file.go",
	}
	if err := odb.PutStagedEntry(staged.Path, staged); err != nil {
		t.Fatalf("PutStagedEntry failed: %v", err)
	}

	commit := LocalVirtualCommit{
		ID:            "local-commit-1",
		LogicalBranch: "feature/session-vcs",
		Message:       "first local commit",
		AuthorName:    "Test User",
		AuthorEmail:   "test@example.com",
		PrincipalID:   "principal-a",
		CreatedAt:     createdAt,
		Repositories: []LocalCommitRepository{{
			StorageID:   "repo-a",
			DisplayPath: "github.com/acme/service-a",
			RepoURL:     "https://github.com/acme/service-a.git",
			Branch:      "main",
			BaseCommit:  "abc123",
			Operations: []LocalCommitOperation{{
				Kind:    "upsert",
				Path:    "file.go",
				Mode:    0o644,
				Content: []byte("package servicea\n"),
			}},
		}},
		Pushed:    true,
		PushJobID: "job-1",
		PushedAt:  pushedAt,
	}
	if err := odb.PutLocalVirtualCommit(commit); err != nil {
		t.Fatalf("PutLocalVirtualCommit failed: %v", err)
	}

	if err := odb.SetCurrentLogicalBranch("feature/session-vcs"); err != nil {
		t.Fatalf("SetCurrentLogicalBranch failed: %v", err)
	}

	mapping := SessionBranchMapping{
		PrincipalID:      "principal-a",
		LogicalBranch:    "feature/session-vcs",
		StorageID:        "repo-a",
		DisplayPath:      "github.com/acme/service-a",
		OriginalBranch:   "main",
		ActualBranch:     "feature/session-vcs-20260520",
		LastPushedCommit: "def456",
		CreatedAt:        mappingCreatedAt,
	}
	if err := odb.PutBranchMapping(mapping); err != nil {
		t.Fatalf("PutBranchMapping failed: %v", err)
	}

	if err := odb.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	odb, err = OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("reopen OpenOverlayDB failed: %v", err)
	}
	defer odb.Close()

	gotStaged, found, err := odb.GetStagedEntry(staged.Path)
	if err != nil {
		t.Fatalf("GetStagedEntry failed: %v", err)
	}
	if !found {
		t.Fatal("expected staged entry after reopen")
	}
	if !reflect.DeepEqual(gotStaged, staged) {
		t.Fatalf("staged entry mismatch after reopen: got %+v want %+v", gotStaged, staged)
	}

	gotCommit, found, err := odb.GetLocalVirtualCommit(commit.ID)
	if err != nil {
		t.Fatalf("GetLocalVirtualCommit failed: %v", err)
	}
	if !found {
		t.Fatal("expected local virtual commit after reopen")
	}
	if !reflect.DeepEqual(gotCommit, commit) {
		t.Fatalf("local virtual commit mismatch after reopen: got %+v want %+v", gotCommit, commit)
	}

	branch, found, err := odb.GetCurrentLogicalBranch()
	if err != nil {
		t.Fatalf("GetCurrentLogicalBranch failed: %v", err)
	}
	if !found || branch != "feature/session-vcs" {
		t.Fatalf("current logical branch = (%q, %v), want (feature/session-vcs, true)", branch, found)
	}

	gotMapping, found, err := odb.GetBranchMapping(mapping.PrincipalID, mapping.LogicalBranch, mapping.StorageID)
	if err != nil {
		t.Fatalf("GetBranchMapping failed: %v", err)
	}
	if !found {
		t.Fatal("expected branch mapping after reopen")
	}
	if !reflect.DeepEqual(gotMapping, mapping) {
		t.Fatalf("branch mapping mismatch after reopen: got %+v want %+v", gotMapping, mapping)
	}

	if got := odb.StagedEntryCount(); got != 1 {
		t.Fatalf("StagedEntryCount() = %d, want 1", got)
	}
	if got := odb.LocalVirtualCommitCount(); got != 1 {
		t.Fatalf("LocalVirtualCommitCount() = %d, want 1", got)
	}
	if got := odb.BranchMappingCount(); got != 1 {
		t.Fatalf("BranchMappingCount() = %d, want 1", got)
	}
}

func TestSessionManager_RecoverSessionRestoresVCSState(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	sessionID, _, _, ok := sm.GetSessionInfo()
	if !ok {
		t.Fatal("expected active session")
	}

	staged := StagedIndexEntry{
		Path:                "github.com/acme/service-b/handler.go",
		RepositoryStorageID: "repo-b",
		RepositoryPath:      "github.com/acme/service-b",
		ChangeType:          ChangeCreate,
		StagedAt:            time.Date(2026, 5, 20, 11, 0, 0, 0, time.UTC),
		Content:             []byte("package serviceb\n"),
		Mode:                0o644,
		LocalPath:           filepath.Join(sm.current.BasePath, "github.com/acme/service-b/handler.go"),
	}
	if err := sm.PutStagedEntry(staged); err != nil {
		t.Fatalf("PutStagedEntry failed: %v", err)
	}

	commit := LocalVirtualCommit{
		ID:            "local-commit-2",
		LogicalBranch: "feature/recovery",
		Message:       "recovered commit",
		PrincipalID:   "principal-b",
		CreatedAt:     time.Date(2026, 5, 20, 11, 1, 0, 0, time.UTC),
	}
	if err := sm.PutLocalVirtualCommit(commit); err != nil {
		t.Fatalf("PutLocalVirtualCommit failed: %v", err)
	}

	if err := sm.SetCurrentLogicalBranch("feature/recovery"); err != nil {
		t.Fatalf("SetCurrentLogicalBranch failed: %v", err)
	}

	mapping := SessionBranchMapping{
		PrincipalID:    "principal-b",
		LogicalBranch:  "feature/recovery",
		StorageID:      "repo-b",
		DisplayPath:    "github.com/acme/service-b",
		OriginalBranch: "main",
		ActualBranch:   "feature/recovery-20260520",
		CreatedAt:      time.Date(2026, 5, 20, 11, 2, 0, 0, time.UTC),
	}
	if err := sm.PutBranchMapping(mapping); err != nil {
		t.Fatalf("PutBranchMapping failed: %v", err)
	}

	if err := sm.db.Close(); err != nil {
		t.Fatalf("close original db failed: %v", err)
	}

	recovered, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("recovering NewSessionManager failed: %v", err)
	}
	defer func() {
		if recovered.db != nil {
			_ = recovered.db.Close()
		}
	}()

	recoveredID, _, _, ok := recovered.GetSessionInfo()
	if !ok {
		t.Fatal("expected recovered active session")
	}
	if recoveredID != sessionID {
		t.Fatalf("recovered session id = %q, want %q", recoveredID, sessionID)
	}

	gotStaged, found, err := recovered.GetStagedEntry(staged.Path)
	if err != nil {
		t.Fatalf("recovered GetStagedEntry failed: %v", err)
	}
	if !found || !reflect.DeepEqual(gotStaged, staged) {
		t.Fatalf("recovered staged entry = %+v, found=%v, want %+v", gotStaged, found, staged)
	}

	commits, err := recovered.ListLocalVirtualCommits()
	if err != nil {
		t.Fatalf("ListLocalVirtualCommits failed: %v", err)
	}
	if len(commits) != 1 || !reflect.DeepEqual(commits[0], commit) {
		t.Fatalf("recovered commits = %+v, want [%+v]", commits, commit)
	}

	branch, found, err := recovered.GetCurrentLogicalBranch()
	if err != nil {
		t.Fatalf("GetCurrentLogicalBranch failed: %v", err)
	}
	if !found || branch != "feature/recovery" {
		t.Fatalf("recovered current branch = (%q, %v), want (feature/recovery, true)", branch, found)
	}

	gotMapping, found, err := recovered.GetBranchMapping(mapping.PrincipalID, mapping.LogicalBranch, mapping.StorageID)
	if err != nil {
		t.Fatalf("GetBranchMapping failed: %v", err)
	}
	if !found || !reflect.DeepEqual(gotMapping, mapping) {
		t.Fatalf("recovered branch mapping = %+v, found=%v, want %+v", gotMapping, found, mapping)
	}
}

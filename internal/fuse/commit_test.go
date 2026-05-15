package fuse

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	monoclient "github.com/radryc/monofs/internal/client"
)

func TestCommitManagerRejectsDependencyChangesBeforeCommit(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	writeTrackedCommitFile(t, sessionMgr, "dependency/go/mod/cache/download/example.com/@v/v1.0.0.mod", "module example.com\n", ChangeCreate)

	commitMgr := NewCommitManager(sessionMgr, &mockClient{}, testLogger())
	if _, err := commitMgr.CommitChanges(context.Background()); err == nil || !strings.Contains(err.Error(), "monofs-session push") {
		t.Fatalf("CommitChanges() error = %v, want dependency push guidance", err)
	}
	if sessionMgr.GetCurrentSession() == nil {
		t.Fatal("session should remain active after rejected dependency commit")
	}
}

func TestCommitManagerCommitsVirtualMonorepoByRepository(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	writeTrackedCommitFile(t, sessionMgr, "github.com/acme/monofs/main.go", "package main\n", ChangeModify)
	writeTrackedCommitFile(t, sessionMgr, "github.com/acme/guardian/README.md", "guardian repo\n", ChangeCreate)
	writeTrackedCommitFile(t, sessionMgr, "github.com/acme/doctor/app.go", "doctor repo\n", ChangeCreate)

	mockCli := &commitApplierMockClient{
		mockClient: &mockClient{
			workspaceRepos: []monoclient.WorkspaceRepository{
				{StorageID: "repo-monofs", DisplayPath: "github.com/acme/monofs"},
				{StorageID: "repo-guardian", DisplayPath: "github.com/acme/guardian"},
				{StorageID: "repo-doctor", DisplayPath: "github.com/acme/doctor"},
			},
		},
	}
	commitMgr := NewCommitManager(sessionMgr, mockCli, testLogger())
	commitMgr.SetWorkspaceManifest(NewWorkspaceManifest(mockCli))

	result, err := commitMgr.CommitChanges(context.Background())
	if err != nil {
		t.Fatalf("CommitChanges() error = %v", err)
	}
	if !result.Success {
		t.Fatalf("CommitChanges() success = false, errors = %v", result.Errors)
	}
	if result.FilesProcessed != 3 || result.FilesUploaded != 3 || result.FilesFailed != 0 {
		t.Fatalf("CommitChanges() result = %+v, want processed/uploaded=3 and failed=0", result)
	}
	if result.Repositories != 3 {
		t.Fatalf("CommitChanges() repositories = %d, want 3", result.Repositories)
	}
	if sessionMgr.GetCurrentSession() != nil {
		t.Fatal("session should be archived after successful virtual monorepo commit")
	}

	gotRepos := make([]string, 0, len(mockCli.applied))
	gotPaths := make([]string, 0, len(mockCli.applied))
	for _, applied := range mockCli.applied {
		gotRepos = append(gotRepos, applied.repo.DisplayPath)
		for _, change := range applied.changes {
			gotPaths = append(gotPaths, applied.repo.DisplayPath+":"+change.Path)
		}
	}
	sort.Strings(gotRepos)
	sort.Strings(gotPaths)
	if strings.Join(gotRepos, ",") != "github.com/acme/doctor,github.com/acme/guardian,github.com/acme/monofs" {
		t.Fatalf("applied repos = %v", gotRepos)
	}
	if strings.Join(gotPaths, ",") != "github.com/acme/doctor:app.go,github.com/acme/guardian:README.md,github.com/acme/monofs:main.go" {
		t.Fatalf("applied repo-relative paths = %v", gotPaths)
	}
}

type commitApplierMockClient struct {
	*mockClient
	applied []appliedRepositoryChanges
}

type appliedRepositoryChanges struct {
	repo    monoclient.WorkspaceRepository
	changes []monoclient.RepositoryChange
}

func (m *commitApplierMockClient) ApplyRepositoryChanges(ctx context.Context, repo monoclient.WorkspaceRepository, changes []monoclient.RepositoryChange) (*monoclient.ApplyRepositoryChangesResult, error) {
	copyChanges := append([]monoclient.RepositoryChange(nil), changes...)
	m.applied = append(m.applied, appliedRepositoryChanges{repo: repo, changes: copyChanges})
	return &monoclient.ApplyRepositoryChangesResult{FilesUpserted: len(changes)}, nil
}

func writeTrackedCommitFile(t *testing.T, sessionMgr *SessionManager, monofsPath, content string, changeType ChangeType) {
	t.Helper()

	localPath, err := sessionMgr.GetLocalPath(monofsPath)
	if err != nil {
		t.Fatalf("GetLocalPath(%q) error = %v", monofsPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", filepath.Dir(localPath), err)
	}
	if err := os.WriteFile(localPath, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", localPath, err)
	}
	if err := sessionMgr.TrackChange(changeType, monofsPath, ""); err != nil {
		t.Fatalf("TrackChange(%q) error = %v", monofsPath, err)
	}
}

package fuse

import (
	"context"
	"os"
	"path/filepath"
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

func TestCommitManagerRejectsVirtualMonorepoPublishUntilImplemented(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	writeTrackedCommitFile(t, sessionMgr, "github.com/acme/monofs/main.go", "package main\n", ChangeModify)
	writeTrackedCommitFile(t, sessionMgr, "github.com/acme/guardian/README.md", "guardian repo\n", ChangeCreate)
	writeTrackedCommitFile(t, sessionMgr, "github.com/acme/doctor/app.go", "doctor repo\n", ChangeCreate)

	mockCli := &mockClient{
		workspaceRepos: []monoclient.WorkspaceRepository{
			{StorageID: "repo-monofs", DisplayPath: "github.com/acme/monofs"},
			{StorageID: "repo-guardian", DisplayPath: "github.com/acme/guardian"},
			{StorageID: "repo-doctor", DisplayPath: "github.com/acme/doctor"},
		},
	}
	commitMgr := NewCommitManager(sessionMgr, mockCli, testLogger())
	commitMgr.SetWorkspaceManifest(NewWorkspaceManifest(mockCli))

	_, err = commitMgr.CommitChanges(context.Background())
	if err == nil {
		t.Fatal("CommitChanges() error = nil, want virtual monorepo publish rejection")
	}
	for _, want := range []string{
		"virtual monorepo publish is not implemented yet",
		"github.com/acme/monofs",
		"github.com/acme/guardian",
		"github.com/acme/doctor",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("CommitChanges() error = %q, want substring %q", err.Error(), want)
		}
	}
	if sessionMgr.GetCurrentSession() == nil {
		t.Fatal("session should remain active after rejected virtual monorepo commit")
	}
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
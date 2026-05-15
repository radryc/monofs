package fuse

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	monoclient "github.com/radryc/monofs/internal/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSessionSocketStatusUsesWorkspaceManifest(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)

	writeTrackedSessionFile(t, sessionMgr, "github.com/acme/monofs/main.go", "package main\n", ChangeModify)
	writeTrackedSessionFile(t, sessionMgr, "github.com/acme/guardian/README.md", "guardian repo\n", ChangeCreate)
	writeTrackedSessionFile(t, sessionMgr, "github.com/acme/doctor/app.go", "doctor repo\n", ChangeCreate)
	writeTrackedSessionFile(t, sessionMgr, "dependency/go/mod/cache/download/example.com/@v/v1.0.0.mod", "module example.com\n", ChangeCreate)
	writeTrackedSessionFile(t, sessionMgr, "guardian/control.txt", "hidden\n", ChangeCreate)

	resp := handler.handleStatus(true)
	if !resp.Success {
		t.Fatalf("handleStatus() error = %s", resp.Error)
	}
	if resp.Changes != 3 {
		t.Fatalf("handleStatus() Changes = %d, want 3", resp.Changes)
	}
	if resp.BlobChanges != 1 {
		t.Fatalf("handleStatus() BlobChanges = %d, want 1", resp.BlobChanges)
	}
	if resp.ExcludedChanges != 1 {
		t.Fatalf("handleStatus() ExcludedChanges = %d, want 1", resp.ExcludedChanges)
	}

	repos := make(map[string]bool)
	for _, change := range resp.ChangeList {
		repos[change.Repository] = true
		if strings.HasPrefix(change.Path, "guardian/") {
			t.Fatalf("excluded guardian namespace leaked into workspace status: %+v", change)
		}
	}
	for _, want := range []string{
		"github.com/acme/monofs",
		"github.com/acme/guardian",
		"github.com/acme/doctor",
	} {
		if !repos[want] {
			t.Fatalf("workspace repo %q missing from status output: %+v", want, resp.ChangeList)
		}
	}
	if len(resp.BlobChangeList) != 1 || !strings.HasPrefix(resp.BlobChangeList[0].Path, "dependency/") {
		t.Fatalf("blob status output = %+v, want one dependency change", resp.BlobChangeList)
	}
}

func TestSessionSocketDiffUsesWorkspaceManifest(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)
	handler.diffReader = DiffReaderFunc(func(ctx context.Context, path string) ([]byte, error) {
		switch path {
		case "github.com/acme/monofs/main.go", "github.com/acme/doctor/app.go":
			return []byte("old\n"), nil
		default:
			return nil, status.Error(codes.NotFound, "missing")
		}
	})

	writeTrackedSessionFile(t, sessionMgr, "github.com/acme/monofs/main.go", "new\n", ChangeModify)
	writeTrackedSessionFile(t, sessionMgr, "github.com/acme/guardian/README.md", "guardian repo\n", ChangeCreate)
	writeTrackedSessionFile(t, sessionMgr, "github.com/acme/doctor/app.go", "doctor repo\n", ChangeModify)
	writeTrackedSessionFile(t, sessionMgr, "dependency/go/mod/cache/download/example.com/@v/v1.0.0.mod", "module example.com\n", ChangeCreate)
	writeTrackedSessionFile(t, sessionMgr, "guardian/control.txt", "hidden\n", ChangeCreate)

	resp := handler.handleDiff("", true)
	if !resp.Success {
		t.Fatalf("handleDiff() error = %s", resp.Error)
	}
	if resp.Changes != 3 {
		t.Fatalf("handleDiff() Changes = %d, want 3", resp.Changes)
	}
	if resp.BlobChanges != 1 {
		t.Fatalf("handleDiff() BlobChanges = %d, want 1", resp.BlobChanges)
	}
	if resp.ExcludedChanges != 1 {
		t.Fatalf("handleDiff() ExcludedChanges = %d, want 1", resp.ExcludedChanges)
	}

	repos := make(map[string]bool)
	for _, diff := range resp.DiffData {
		repos[diff.Repository] = true
		if strings.HasPrefix(diff.Path, "guardian/") {
			t.Fatalf("excluded guardian namespace leaked into workspace diff: %+v", diff)
		}
	}
	for _, want := range []string{
		"github.com/acme/monofs",
		"github.com/acme/guardian",
		"github.com/acme/doctor",
	} {
		if !repos[want] {
			t.Fatalf("workspace repo %q missing from diff output: %+v", want, resp.DiffData)
		}
	}
	if len(resp.BlobDiffData) != 1 || !strings.HasPrefix(resp.BlobDiffData[0].Path, "dependency/") {
		t.Fatalf("blob diff output = %+v, want one dependency diff", resp.BlobDiffData)
	}
}

func newVirtualMonorepoSessionHandler(t *testing.T) (*SessionSocketHandler, *SessionManager) {
	t.Helper()

	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	root := NewRootWithSession(&mockClient{
		workspaceRepos: []monoclient.WorkspaceRepository{
			{StorageID: "repo-monofs", DisplayPath: "github.com/acme/monofs"},
			{StorageID: "repo-guardian", DisplayPath: "github.com/acme/guardian"},
			{StorageID: "repo-doctor", DisplayPath: "github.com/acme/doctor"},
			{StorageID: "dep-cache", DisplayPath: "dependency/go/mod/cache"},
		},
	}, nil, sessionMgr, testLogger())
	if err := root.EnableVirtualMonorepo(); err != nil {
		t.Fatalf("EnableVirtualMonorepo() error = %v", err)
	}

	handler := &SessionSocketHandler{
		sessionMgr: sessionMgr,
		rootNode:   root,
		diffReader: DiffReaderFunc(func(ctx context.Context, path string) ([]byte, error) {
			return nil, status.Error(codes.NotFound, "missing")
		}),
		logger: testLogger(),
		ctx:    context.Background(),
	}
	return handler, sessionMgr
}

func writeTrackedSessionFile(t *testing.T, sessionMgr *SessionManager, monofsPath, content string, changeType ChangeType) {
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
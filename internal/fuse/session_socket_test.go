package fuse

import (
	"context"
	"os"
	"path/filepath"
	"sort"
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

func TestSessionSocketPullRefreshesIncludedWorkspaceRepos(t *testing.T) {
	handler, _ := newVirtualMonorepoSessionHandler(t)
	refresher := &workspaceRefreshMock{
		result: &monoclient.WorkspaceRefreshResult{Requested: 3, Refreshed: 3},
	}
	handler.SetWorkspaceRefresher(refresher)

	resp := handler.handlePull()
	if !resp.Success {
		t.Fatalf("handlePull() error = %s", resp.Error)
	}
	if resp.Changes != 3 {
		t.Fatalf("handlePull() Changes = %d, want 3", resp.Changes)
	}
	if len(refresher.calls) != 1 {
		t.Fatalf("refresh calls = %d, want 1", len(refresher.calls))
	}
	got := make([]string, 0, len(refresher.calls[0]))
	for _, repo := range refresher.calls[0] {
		got = append(got, repo.DisplayPath)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "github.com/acme/doctor,github.com/acme/guardian,github.com/acme/monofs" {
		t.Fatalf("pull repos = %v", got)
	}
}

func TestSessionSocketBranchListsIncludedWorkspaceRefs(t *testing.T) {
	handler, _ := newVirtualMonorepoSessionHandler(t)

	resp := handler.handleBranch()
	if !resp.Success {
		t.Fatalf("handleBranch() error = %s", resp.Error)
	}
	if len(resp.WorkspaceRefs) != 3 {
		t.Fatalf("handleBranch() refs = %d, want 3", len(resp.WorkspaceRefs))
	}

	got := make([]string, 0, len(resp.WorkspaceRefs))
	for _, ref := range resp.WorkspaceRefs {
		if strings.HasPrefix(ref.DisplayPath, "dependency/") {
			t.Fatalf("dependency repo leaked into branch output: %+v", ref)
		}
		got = append(got, ref.DisplayPath+"@"+ref.Ref+"#"+ref.CommitHash)
	}
	want := []string{
		"github.com/acme/doctor@release/2026-05#ccccccc3",
		"github.com/acme/guardian@main#bbbbbbb2",
		"github.com/acme/monofs@main#aaaaaaa1",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("handleBranch() refs = %v, want %v", got, want)
	}
}

func TestSessionSocketPullRejectsDirtyWorkspace(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)
	refresher := &workspaceRefreshMock{
		result: &monoclient.WorkspaceRefreshResult{Requested: 3, Refreshed: 3},
	}
	handler.SetWorkspaceRefresher(refresher)
	writeTrackedSessionFile(t, sessionMgr, "github.com/acme/monofs/main.go", "package main\n", ChangeModify)

	resp := handler.handlePull()
	if resp.Success {
		t.Fatal("handlePull() success = true, want rejection for dirty workspace")
	}
	if !strings.Contains(resp.Error, "local changes pending") {
		t.Fatalf("handlePull() error = %q, want pending-changes guidance", resp.Error)
	}
	if len(refresher.calls) != 0 {
		t.Fatalf("refresh calls = %d, want 0", len(refresher.calls))
	}
}

type workspaceRefreshMock struct {
	result *monoclient.WorkspaceRefreshResult
	err    error
	calls  [][]monoclient.WorkspaceRepository
}

func (m *workspaceRefreshMock) RefreshWorkspaceRepositories(ctx context.Context, repos []monoclient.WorkspaceRepository) (*monoclient.WorkspaceRefreshResult, error) {
	copyRepos := append([]monoclient.WorkspaceRepository(nil), repos...)
	m.calls = append(m.calls, copyRepos)
	return m.result, m.err
}

func newVirtualMonorepoSessionHandler(t *testing.T) (*SessionSocketHandler, *SessionManager) {
	t.Helper()

	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	root := NewRootWithSession(&mockClient{
		workspaceRepos: []monoclient.WorkspaceRepository{
			{StorageID: "repo-monofs", DisplayPath: "github.com/acme/monofs", Ref: "main", CommitHash: "aaaaaaa1"},
			{StorageID: "repo-guardian", DisplayPath: "github.com/acme/guardian", Ref: "main", CommitHash: "bbbbbbb2"},
			{StorageID: "repo-doctor", DisplayPath: "github.com/acme/doctor", Ref: "release/2026-05", CommitHash: "ccccccc3"},
			{StorageID: "dep-cache", DisplayPath: "dependency/go/mod/cache", Ref: "blob", CommitHash: "ddddddd4"},
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

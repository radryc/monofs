package fuse

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	monoclient "github.com/radryc/monofs/internal/client"
	"github.com/radryc/monofs/internal/workspacebundle"
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
	if resp.UnstagedChanges != 3 {
		t.Fatalf("handleStatus() UnstagedChanges = %d, want 3", resp.UnstagedChanges)
	}
	if resp.StagedChanges != 0 {
		t.Fatalf("handleStatus() StagedChanges = %d, want 0", resp.StagedChanges)
	}
	if resp.PendingCommits != 0 {
		t.Fatalf("handleStatus() PendingCommits = %d, want 0", resp.PendingCommits)
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

func TestSessionSocketAddStagesWorkspaceSnapshot(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)
	const path = "github.com/acme/monofs/main.go"

	writeTrackedSessionFile(t, sessionMgr, path, "package main\n", ChangeModify)

	resp := handler.handleAdd([]string{path})
	if !resp.Success {
		t.Fatalf("handleAdd() error = %s", resp.Error)
	}
	if resp.StagedChanges != 1 {
		t.Fatalf("handleAdd() StagedChanges = %d, want 1", resp.StagedChanges)
	}

	staged, found, err := sessionMgr.GetStagedEntry(path)
	if err != nil {
		t.Fatalf("GetStagedEntry() error = %v", err)
	}
	if !found {
		t.Fatal("expected staged entry")
	}
	if string(staged.Content) != "package main\n" {
		t.Fatalf("staged content = %q, want original snapshot", string(staged.Content))
	}
	if staged.RepositoryPath != "github.com/acme/monofs" || staged.RepositoryStorageID != "repo-monofs" {
		t.Fatalf("staged repository metadata = %+v", staged)
	}

	localPath, err := sessionMgr.GetLocalPath(path)
	if err != nil {
		t.Fatalf("GetLocalPath() error = %v", err)
	}
	if err := os.WriteFile(localPath, []byte("package main\n// changed after add\n"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := sessionMgr.TrackChange(ChangeModify, path, ""); err != nil {
		t.Fatalf("TrackChange() error = %v", err)
	}

	staged, found, err = sessionMgr.GetStagedEntry(path)
	if err != nil {
		t.Fatalf("GetStagedEntry() after mutation error = %v", err)
	}
	if !found {
		t.Fatal("expected staged entry after mutation")
	}
	if string(staged.Content) != "package main\n" {
		t.Fatalf("staged content mutated to %q, want original snapshot", string(staged.Content))
	}

	statusResp := handler.handleStatus(false)
	if !statusResp.Success {
		t.Fatalf("handleStatus() error = %s", statusResp.Error)
	}
	if statusResp.Changes != 1 {
		t.Fatalf("handleStatus() Changes = %d, want 1", statusResp.Changes)
	}
	if statusResp.UnstagedChanges != 1 {
		t.Fatalf("handleStatus() UnstagedChanges = %d, want 1", statusResp.UnstagedChanges)
	}
	if statusResp.StagedChanges != 1 {
		t.Fatalf("handleStatus() StagedChanges = %d, want 1", statusResp.StagedChanges)
	}
	if len(statusResp.ChangeList) != 1 || statusResp.ChangeList[0].Path != path {
		t.Fatalf("unstaged ChangeList = %+v, want %q", statusResp.ChangeList, path)
	}
	if len(statusResp.StagedChangeList) != 1 || statusResp.StagedChangeList[0].Path != path {
		t.Fatalf("staged ChangeList = %+v, want %q", statusResp.StagedChangeList, path)
	}
}

func TestSessionSocketAddRejectsDependencyChanges(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)
	depPath := "dependency/go/mod/cache/download/example.com/@v/v1.0.0.mod"

	writeTrackedSessionFile(t, sessionMgr, depPath, "module example.com\n", ChangeCreate)

	resp := handler.handleAdd([]string{depPath})
	if resp.Success {
		t.Fatal("handleAdd() success = true, want dependency rejection")
	}
	if !strings.Contains(resp.Error, "dependency changes") {
		t.Fatalf("handleAdd() error = %q, want dependency guidance", resp.Error)
	}
}

func TestSessionSocketCommitCreatesPendingLocalCommit(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)
	handler.commitMgr = NewCommitManager(sessionMgr, &mockClient{workspaceRepos: testWorkspaceRepositories()}, testLogger())
	handler.commitMgr.SetWorkspaceManifest(handler.rootNode.WorkspaceManifest())
	handler.commitMgr.SetPrincipalID("principal-session")
	const path = "github.com/acme/monofs/main.go"

	writeTrackedSessionFile(t, sessionMgr, path, "package main\n", ChangeModify)
	if resp := handler.handleAdd([]string{path}); !resp.Success {
		t.Fatalf("handleAdd() error = %s", resp.Error)
	}

	resp := handler.handleCommit(SessionRequest{
		Action:               "commit",
		LogicalCommitMessage: "checkpoint",
		AuthorName:           "Test User",
		AuthorEmail:          "test@example.com",
	})
	if !resp.Success {
		t.Fatalf("handleCommit() error = %s", resp.Error)
	}
	if !strings.Contains(resp.Message, "created local commit") {
		t.Fatalf("handleCommit() message = %q, want local commit summary", resp.Message)
	}

	statusResp := handler.handleStatus(false)
	if !statusResp.Success {
		t.Fatalf("handleStatus() error = %s", statusResp.Error)
	}
	if statusResp.UnstagedChanges != 0 {
		t.Fatalf("handleStatus() UnstagedChanges = %d, want 0", statusResp.UnstagedChanges)
	}
	if statusResp.StagedChanges != 0 {
		t.Fatalf("handleStatus() StagedChanges = %d, want 0", statusResp.StagedChanges)
	}
	if statusResp.PendingCommits != 1 {
		t.Fatalf("handleStatus() PendingCommits = %d, want 1", statusResp.PendingCommits)
	}
	if len(statusResp.PendingCommitList) != 1 || statusResp.PendingCommitList[0].Message != "checkpoint" {
		t.Fatalf("pending commits = %+v, want one checkpoint commit", statusResp.PendingCommitList)
	}
	localCommits, err := sessionMgr.ListLocalVirtualCommits()
	if err != nil {
		t.Fatalf("ListLocalVirtualCommits() error = %v", err)
	}
	if len(localCommits) != 1 || localCommits[0].PrincipalID != "principal-session" {
		t.Fatalf("stored local commits = %+v, want principal metadata", localCommits)
	}
}

func TestSessionSocketLogListsLocalCommitsWithMetadata(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)
	baseTime := time.Date(2026, time.May, 20, 10, 0, 0, 0, time.UTC)

	if err := sessionMgr.PutLocalVirtualCommit(LocalVirtualCommit{
		ID:            "local-1",
		Message:       "first checkpoint",
		LogicalBranch: "feature/demo",
		AuthorName:    "Test User",
		AuthorEmail:   "test@example.com",
		PrincipalID:   "principal-a",
		CreatedAt:     baseTime,
		Repositories: []LocalCommitRepository{{
			StorageID:   "repo-monofs",
			DisplayPath: "github.com/acme/monofs",
			Operations: []LocalCommitOperation{{
				Kind: "upsert",
				Path: "main.go",
			}},
		}},
	}); err != nil {
		t.Fatalf("PutLocalVirtualCommit(local-1) error = %v", err)
	}
	if err := sessionMgr.PutLocalVirtualCommit(LocalVirtualCommit{
		ID:            "local-2",
		ParentID:      "local-1",
		Message:       "second checkpoint",
		LogicalBranch: "feature/demo",
		PrincipalID:   "principal-a",
		CreatedAt:     baseTime.Add(time.Minute),
		Repositories: []LocalCommitRepository{
			{
				StorageID:   "repo-guardian",
				DisplayPath: "github.com/acme/guardian",
				Operations: []LocalCommitOperation{{
					Kind: "upsert",
					Path: "README.md",
				}},
			},
			{
				StorageID:   "repo-monofs",
				DisplayPath: "github.com/acme/monofs",
				Operations: []LocalCommitOperation{
					{Kind: "upsert", Path: "main.go"},
					{Kind: "delete", Path: "old.go"},
				},
			},
		},
	}); err != nil {
		t.Fatalf("PutLocalVirtualCommit(local-2) error = %v", err)
	}

	resp := handler.handleLog(SessionRequest{})
	if !resp.Success {
		t.Fatalf("handleLog() error = %s", resp.Error)
	}
	if len(resp.LocalCommitList) != 2 {
		t.Fatalf("LocalCommitList length = %d, want 2", len(resp.LocalCommitList))
	}
	if resp.LocalCommitList[0].ID != "local-2" || resp.LocalCommitList[1].ID != "local-1" {
		t.Fatalf("LocalCommitList order = %+v, want newest first", resp.LocalCommitList)
	}
	if resp.LocalCommitList[0].ParentID != "local-1" || resp.LocalCommitList[0].RepositoryCount != 2 || resp.LocalCommitList[0].OperationCount != 3 {
		t.Fatalf("newest LocalCommitInfo = %+v, want parent and aggregated counts", resp.LocalCommitList[0])
	}
	if resp.LocalCommitList[1].AuthorName != "Test User" || resp.LocalCommitList[1].AuthorEmail != "test@example.com" || resp.LocalCommitList[1].PrincipalID != "principal-a" {
		t.Fatalf("oldest LocalCommitInfo = %+v, want author and principal metadata", resp.LocalCommitList[1])
	}
}

func TestSessionSocketPushUsesCurrentLogicalBranchCommits(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)
	pushClient := &commitPublisherMockClient{
		mockClient: &mockClient{workspaceRepos: testWorkspaceRepositories()},
		sourcePushResult: &monoclient.WorkspaceSourcePushResult{
			Job: &pb.WorkspaceSyncJob{
				JobId: "job-push-1",
				State: pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED,
				Repositories: []*pb.WorkspaceSyncRepositoryResult{{
					StorageId:    "repo-monofs",
					DisplayPath:  "github.com/acme/monofs",
					Branch:       "main",
					TargetBranch: "feature/demo",
					PushedCommit: "1234567890ab",
					Status:       pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_PUBLISHED,
				}},
			},
		},
	}
	handler.commitMgr = NewCommitManager(sessionMgr, pushClient, testLogger())
	handler.commitMgr.SetWorkspaceManifest(handler.rootNode.WorkspaceManifest())
	handler.commitMgr.SetPrincipalID("principal-test")
	if err := sessionMgr.SetCurrentLogicalBranch("feature/demo"); err != nil {
		t.Fatalf("SetCurrentLogicalBranch() error = %v", err)
	}
	for _, commit := range []LocalVirtualCommit{
		{
			ID:            "local-1",
			LogicalBranch: "feature/demo",
			Message:       "first",
			CreatedAt:     time.Now().Add(-time.Minute),
			Repositories: []LocalCommitRepository{{
				StorageID:   "repo-monofs",
				DisplayPath: "github.com/acme/monofs",
				Branch:      "main",
				BaseCommit:  "aaaaaaa1",
				Operations:  []LocalCommitOperation{{Kind: workspacebundle.OperationUpsert, Path: "main.go", Mode: 0o644, Content: []byte("package main\n")}},
			}},
		},
		{
			ID:            "local-2",
			LogicalBranch: "release/demo",
			Message:       "other",
			CreatedAt:     time.Now(),
			Repositories: []LocalCommitRepository{{
				StorageID:   "repo-guardian",
				DisplayPath: "github.com/acme/guardian",
				Branch:      "main",
				BaseCommit:  "bbbbbbb2",
				Operations:  []LocalCommitOperation{{Kind: workspacebundle.OperationUpsert, Path: "README.md", Mode: 0o644, Content: []byte("guardian\n")}},
			}},
		},
	} {
		if err := sessionMgr.PutLocalVirtualCommit(commit); err != nil {
			t.Fatalf("PutLocalVirtualCommit(%q) error = %v", commit.ID, err)
		}
	}

	resp := handler.handlePushSource(SessionRequest{Action: "push"})
	if !resp.Success {
		t.Fatalf("handlePushSource() error = %s", resp.Error)
	}
	if !strings.Contains(resp.Message, "pushed 1 local commit") {
		t.Fatalf("handlePushSource() message = %q, want push summary", resp.Message)
	}
	commits, err := sessionMgr.ListLocalVirtualCommits()
	if err != nil {
		t.Fatalf("ListLocalVirtualCommits() error = %v", err)
	}
	byID := make(map[string]LocalVirtualCommit, len(commits))
	for _, commit := range commits {
		byID[commit.ID] = commit
	}
	if !byID["local-1"].Pushed {
		t.Fatalf("local-1 after push = %+v, want pushed", byID["local-1"])
	}
	if byID["local-2"].Pushed {
		t.Fatalf("local-2 after push = %+v, want other branch pending", byID["local-2"])
	}
}

func TestParseSourcePushMode(t *testing.T) {
	tests := []struct {
		input   string
		want    pb.SourcePushMode
		wantErr bool
	}{
		{"", pb.SourcePushMode_SOURCE_PUSH_MODE_UNSPECIFIED, false},
		{"squash", pb.SourcePushMode_SOURCE_PUSH_MODE_SQUASH, false},
		{"preserve", pb.SourcePushMode_SOURCE_PUSH_MODE_PRESERVE, false},
		{"PRESERVE", pb.SourcePushMode_SOURCE_PUSH_MODE_PRESERVE, false},
		{"  squash  ", pb.SourcePushMode_SOURCE_PUSH_MODE_SQUASH, false},
		{"bogus", pb.SourcePushMode_SOURCE_PUSH_MODE_UNSPECIFIED, true},
	}

	for _, tt := range tests {
		got, err := parseSourcePushMode(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseSourcePushMode(%q) expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSourcePushMode(%q) error = %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseSourcePushMode(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestSessionSocketPushModePreferenceFlow(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)
	pushClient := &commitPublisherMockClient{
		mockClient: &mockClient{workspaceRepos: testWorkspaceRepositories()},
		sourcePushResult: &monoclient.WorkspaceSourcePushResult{
			Job: &pb.WorkspaceSyncJob{
				JobId: "job-mode-1",
				State: pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED,
				Repositories: []*pb.WorkspaceSyncRepositoryResult{{
					StorageId:   "repo-monofs",
					DisplayPath: "github.com/acme/monofs",
					Branch:      "main",
					Status:      pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_PUBLISHED,
				}},
			},
		},
	}
	handler.commitMgr = NewCommitManager(sessionMgr, pushClient, testLogger())
	handler.commitMgr.SetWorkspaceManifest(handler.rootNode.WorkspaceManifest())
	handler.commitMgr.SetPrincipalID("principal-mode")
	const path = "github.com/acme/monofs/main.go"

	writeTrackedSessionFile(t, sessionMgr, path, "package main\n", ChangeModify)
	if resp := handler.handleAdd([]string{path}); !resp.Success {
		t.Fatalf("handleAdd() error = %s", resp.Error)
	}

	// Commit with an explicit push-mode preference.
	commitResp := handler.handleCommit(SessionRequest{
		Action:               "commit",
		LogicalCommitMessage: "checkpoint",
		AuthorName:           "Test User",
		AuthorEmail:          "test@example.com",
		SourcePushMode:       "preserve",
	})
	if !commitResp.Success {
		t.Fatalf("handleCommit() error = %s", commitResp.Error)
	}
	if got := handler.preferredPushModeLocked(); got != pb.SourcePushMode_SOURCE_PUSH_MODE_PRESERVE {
		t.Fatalf("preferred push mode = %v, want preserve", got)
	}

	// Push without an explicit flag consumes the commit-time preference.
	pushResp := handler.handlePushSource(SessionRequest{Action: "push"})
	if !pushResp.Success {
		t.Fatalf("handlePushSource() error = %s", pushResp.Error)
	}
	if len(pushClient.sourcePushBundles) != 1 {
		t.Fatalf("source push bundles = %d, want 1", len(pushClient.sourcePushBundles))
	}
	if got := pushClient.sourcePushBundles[0].pushMode; got != pb.SourcePushMode_SOURCE_PUSH_MODE_PRESERVE {
		t.Fatalf("push mode used = %v, want preserve", got)
	}
}

func TestSessionSocketPushRejectsInvalidPushMode(t *testing.T) {
	handler, _ := newVirtualMonorepoSessionHandler(t)
	handler.commitMgr = NewCommitManager(nil, nil, testLogger())

	resp := handler.handlePushSource(SessionRequest{Action: "push", SourcePushMode: "bogus"})
	if resp.Success {
		t.Fatal("handlePushSource() should reject an invalid push mode")
	}
	if !strings.Contains(resp.Error, "invalid push mode") {
		t.Fatalf("handlePushSource() error = %q, want invalid push mode", resp.Error)
	}

	commitResp := handler.handleCommit(SessionRequest{Action: "commit", SourcePushMode: "bogus"})
	if commitResp.Success {
		t.Fatal("handleCommit() should reject an invalid push mode")
	}
}

func TestSessionSocketRemoveStagesWorkspaceDelete(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)
	const path = "github.com/acme/monofs/main.go"

	writeTrackedSessionFile(t, sessionMgr, path, "package main\n", ChangeModify)
	if resp := handler.handleAdd([]string{path}); !resp.Success {
		t.Fatalf("handleAdd() error = %s", resp.Error)
	}

	localPath, err := sessionMgr.GetLocalPath(path)
	if err != nil {
		t.Fatalf("GetLocalPath() error = %v", err)
	}

	resp := handler.handleRemove([]string{path})
	if !resp.Success {
		t.Fatalf("handleRemove() error = %s", resp.Error)
	}
	if resp.StagedChanges != 1 {
		t.Fatalf("handleRemove() StagedChanges = %d, want 1", resp.StagedChanges)
	}
	if _, statErr := os.Stat(localPath); !os.IsNotExist(statErr) {
		t.Fatalf("Stat(%q) error = %v, want not-exist", localPath, statErr)
	}

	staged, found, err := sessionMgr.GetStagedEntry(path)
	if err != nil {
		t.Fatalf("GetStagedEntry() error = %v", err)
	}
	if !found {
		t.Fatal("expected staged delete entry")
	}
	if staged.ChangeType != ChangeDelete {
		t.Fatalf("staged ChangeType = %s, want %s", staged.ChangeType, ChangeDelete)
	}

	statusResp := handler.handleStatus(false)
	if !statusResp.Success {
		t.Fatalf("handleStatus() error = %s", statusResp.Error)
	}
	if statusResp.UnstagedChanges != 0 {
		t.Fatalf("handleStatus() UnstagedChanges = %d, want 0", statusResp.UnstagedChanges)
	}
	if statusResp.StagedChanges != 1 {
		t.Fatalf("handleStatus() StagedChanges = %d, want 1", statusResp.StagedChanges)
	}
	if len(statusResp.StagedChangeList) != 1 || statusResp.StagedChangeList[0].Type != string(ChangeDelete) {
		t.Fatalf("staged status = %+v, want one delete", statusResp.StagedChangeList)
	}
}

func TestSessionSocketRemoveSessionOnlyCreateClearsStage(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)
	const path = "github.com/acme/monofs/new.txt"

	writeTrackedSessionFile(t, sessionMgr, path, "hello\n", ChangeCreate)
	if resp := handler.handleAdd([]string{path}); !resp.Success {
		t.Fatalf("handleAdd() error = %s", resp.Error)
	}

	resp := handler.handleRemove([]string{path})
	if !resp.Success {
		t.Fatalf("handleRemove() error = %s", resp.Error)
	}
	if resp.StagedChanges != 0 {
		t.Fatalf("handleRemove() StagedChanges = %d, want 0", resp.StagedChanges)
	}

	_, found, err := sessionMgr.GetStagedEntry(path)
	if err != nil {
		t.Fatalf("GetStagedEntry() error = %v", err)
	}
	if found {
		t.Fatal("staged entry should be cleared after removing session-only create")
	}

	statusResp := handler.handleStatus(false)
	if !statusResp.Success {
		t.Fatalf("handleStatus() error = %s", statusResp.Error)
	}
	if statusResp.Changes != 0 || statusResp.UnstagedChanges != 0 || statusResp.StagedChanges != 0 {
		t.Fatalf("handleStatus() = %+v, want clean session", statusResp)
	}
}

func TestSessionSocketRemoveDirectoryStagesRmdirAndClearsDescendants(t *testing.T) {
	baseClient := &mockClient{workspaceRepos: testWorkspaceRepositories()}
	client := &sessionSocketGetAttrClient{
		mockClient: baseClient,
		getattrFunc: func(ctx context.Context, path string) (*pb.GetAttrResponse, error) {
			if path == "github.com/acme/monofs/pkg" {
				return &pb.GetAttrResponse{
					Found: true,
					Mode:  0755 | uint32(syscall.S_IFDIR),
					Ino:   2,
					Nlink: 1,
					Uid:   1000,
					Gid:   1000,
				}, nil
			}
			return baseClient.GetAttr(ctx, path)
		},
	}
	handler, sessionMgr := newVirtualMonorepoSessionHandlerWithClient(t, client)
	const dirPath = "github.com/acme/monofs/pkg"

	writeTrackedSessionFile(t, sessionMgr, dirPath+"/main.go", "package pkg\n", ChangeModify)
	writeTrackedSessionFile(t, sessionMgr, dirPath+"/extra.go", "package pkg\n", ChangeCreate)
	if resp := handler.handleAdd([]string{dirPath + "/main.go"}); !resp.Success {
		t.Fatalf("handleAdd() error = %s", resp.Error)
	}

	resp := handler.handleRemove([]string{dirPath})
	if !resp.Success {
		t.Fatalf("handleRemove() error = %s", resp.Error)
	}
	if resp.StagedChanges != 1 {
		t.Fatalf("handleRemove() StagedChanges = %d, want 1", resp.StagedChanges)
	}

	localDir, err := sessionMgr.GetLocalPath(dirPath)
	if err != nil {
		t.Fatalf("GetLocalPath() error = %v", err)
	}
	if _, statErr := os.Stat(localDir); !os.IsNotExist(statErr) {
		t.Fatalf("Stat(%q) error = %v, want not-exist", localDir, statErr)
	}
	if _, found, err := sessionMgr.GetStagedEntry(dirPath + "/main.go"); err != nil {
		t.Fatalf("GetStagedEntry(child) error = %v", err)
	} else if found {
		t.Fatal("child staged entry should be removed when parent directory is removed")
	}

	staged, found, err := sessionMgr.GetStagedEntry(dirPath)
	if err != nil {
		t.Fatalf("GetStagedEntry(dir) error = %v", err)
	}
	if !found {
		t.Fatal("expected staged directory delete entry")
	}
	if staged.ChangeType != ChangeRmdir {
		t.Fatalf("staged ChangeType = %s, want %s", staged.ChangeType, ChangeRmdir)
	}

	statusResp := handler.handleStatus(false)
	if !statusResp.Success {
		t.Fatalf("handleStatus() error = %s", statusResp.Error)
	}
	if statusResp.UnstagedChanges != 0 || statusResp.StagedChanges != 1 {
		t.Fatalf("status counts = %+v, want one staged directory removal", statusResp)
	}
	if len(statusResp.StagedChangeList) != 1 || statusResp.StagedChangeList[0].Type != string(ChangeRmdir) {
		t.Fatalf("staged status = %+v, want one rmdir", statusResp.StagedChangeList)
	}
}

func TestSessionSocketRemoveRejectsDependencyChanges(t *testing.T) {
	handler, _ := newVirtualMonorepoSessionHandler(t)

	resp := handler.handleRemove([]string{"dependency/go/mod/cache/download/example.com/@v/v1.0.0.mod"})
	if resp.Success {
		t.Fatal("handleRemove() success = true, want dependency rejection")
	}
	if !strings.Contains(resp.Error, "dependency path") {
		t.Fatalf("handleRemove() error = %q, want dependency guidance", resp.Error)
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

func TestSessionSocketRefsListsIncludedWorkspaceRefs(t *testing.T) {
	handler, _ := newVirtualMonorepoSessionHandler(t)

	resp := handler.handleRefs()
	if !resp.Success {
		t.Fatalf("handleRefs() error = %s", resp.Error)
	}
	if len(resp.WorkspaceRefs) != 3 {
		t.Fatalf("handleRefs() refs = %d, want 3", len(resp.WorkspaceRefs))
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
		t.Fatalf("handleRefs() refs = %v, want %v", got, want)
	}
}

func TestSessionSocketBranchCreateAndSwitchUseLogicalBranchState(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)

	createResp := handler.handleBranch(SessionRequest{Action: "branch", BranchOp: "create", BranchName: "feature/demo"})
	if !createResp.Success {
		t.Fatalf("handleBranch(create) error = %s", createResp.Error)
	}
	if !strings.Contains(createResp.Message, "created and switched") {
		t.Fatalf("handleBranch(create) message = %q, want create summary", createResp.Message)
	}
	branchName, found, err := sessionMgr.GetCurrentLogicalBranch()
	if err != nil {
		t.Fatalf("GetCurrentLogicalBranch() error = %v", err)
	}
	if !found || branchName != "feature/demo" {
		t.Fatalf("current branch = (%q, %v), want (feature/demo, true)", branchName, found)
	}
	mappings, err := sessionMgr.ListBranchMappings()
	if err != nil {
		t.Fatalf("ListBranchMappings() error = %v", err)
	}
	if len(mappings) != 3 {
		t.Fatalf("branch mappings = %d, want 3 visible repo markers", len(mappings))
	}
	for _, mapping := range mappings {
		if mapping.PrincipalID != "principal-test" || mapping.LogicalBranch != "feature/demo" || mapping.ActualBranch != "" {
			t.Fatalf("unexpected mapping after create: %+v", mapping)
		}
	}

	if resp := handler.handleBranch(SessionRequest{Action: "branch", BranchOp: "create", BranchName: "release/demo"}); !resp.Success {
		t.Fatalf("handleBranch(create second) error = %s", resp.Error)
	}

	switchResp := handler.handleBranch(SessionRequest{Action: "branch", BranchOp: "switch", BranchName: "feature/demo"})
	if !switchResp.Success {
		t.Fatalf("handleBranch(switch) error = %s", switchResp.Error)
	}
	if !strings.Contains(switchResp.Message, "switched to logical branch feature/demo") {
		t.Fatalf("handleBranch(switch) message = %q, want switch summary", switchResp.Message)
	}

	showResp := handler.handleBranch(SessionRequest{Action: "branch", BranchOp: "show"})
	if !showResp.Success {
		t.Fatalf("handleBranch(show) error = %s", showResp.Error)
	}
	if showResp.CurrentBranch != "feature/demo" {
		t.Fatalf("handleBranch(show) CurrentBranch = %q, want feature/demo", showResp.CurrentBranch)
	}
	if len(showResp.BranchList) != 2 {
		t.Fatalf("handleBranch(show) BranchList = %+v, want two known branches", showResp.BranchList)
	}
	if !showResp.BranchList[0].Current || showResp.BranchList[0].Name != "feature/demo" {
		t.Fatalf("handleBranch(show) first branch = %+v, want current feature/demo", showResp.BranchList[0])
	}
}

func TestSessionSocketBranchShowIncludesCurrentBranchMappings(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)
	if err := sessionMgr.SetCurrentLogicalBranch("feature/demo"); err != nil {
		t.Fatalf("SetCurrentLogicalBranch() error = %v", err)
	}
	if err := sessionMgr.PutBranchMapping(SessionBranchMapping{
		PrincipalID:      "principal-test",
		LogicalBranch:    "feature/demo",
		StorageID:        "repo-monofs",
		DisplayPath:      "github.com/acme/monofs",
		OriginalBranch:   "main",
		ActualBranch:     "feature/demo-20260520",
		LastPushedCommit: "abc123def456",
	}); err != nil {
		t.Fatalf("PutBranchMapping() error = %v", err)
	}

	resp := handler.handleBranch(SessionRequest{Action: "branch", BranchOp: "show"})
	if !resp.Success {
		t.Fatalf("handleBranch(show) error = %s", resp.Error)
	}
	if len(resp.BranchMappings) != 1 {
		t.Fatalf("handleBranch(show) BranchMappings = %+v, want one mapping", resp.BranchMappings)
	}
	if resp.BranchMappings[0].ActualBranch != "feature/demo-20260520" {
		t.Fatalf("handleBranch(show) mapping = %+v, want actual branch", resp.BranchMappings[0])
	}
}

func TestSessionSocketBranchSwitchRejectsUnknownBranch(t *testing.T) {
	handler, _ := newVirtualMonorepoSessionHandler(t)

	resp := handler.handleBranch(SessionRequest{Action: "branch", BranchOp: "switch", BranchName: "missing/branch"})
	if resp.Success {
		t.Fatal("handleBranch(switch unknown) success = true, want rejection")
	}
	if !strings.Contains(resp.Error, "does not exist") {
		t.Fatalf("handleBranch(switch unknown) error = %q, want missing-branch guidance", resp.Error)
	}
}

func TestSessionSocketPullRejectsDirtyWorkspaceWithoutMergeSupport(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)
	handler.diffReader = nil
	refresher := &workspaceRefreshMock{
		result: &monoclient.WorkspaceRefreshResult{Requested: 3, Refreshed: 3},
	}
	handler.SetWorkspaceRefresher(refresher)
	writeTrackedSessionFile(t, sessionMgr, "github.com/acme/monofs/main.go", "package main\n", ChangeModify)

	resp := handler.handlePull()
	if resp.Success {
		t.Fatal("handlePull() success = true, want rejection without merge support")
	}
	if !strings.Contains(resp.Error, "merge support unavailable") {
		t.Fatalf("handlePull() error = %q, want merge-support guidance", resp.Error)
	}
	if len(refresher.calls) != 0 {
		t.Fatalf("refresh calls = %d, want 0", len(refresher.calls))
	}
}

// switchableDiffReader serves different content before and after a
// refresh, simulating an upstream advance.
type switchableDiffReader struct {
	beforeContent map[string]string
	afterContent  map[string]string
	after         bool
}

func (r *switchableDiffReader) ReadOriginal(ctx context.Context, path string) ([]byte, error) {
	content := r.beforeContent
	if r.after {
		content = r.afterContent
	}
	data, ok := content[path]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found: "+path)
	}
	return []byte(data), nil
}

func (r *switchableDiffReader) flip() { r.after = true }

// refreshFlipMock wraps workspaceRefreshMock and flips the diff reader
// to post-refresh content when the refresh runs.
type refreshFlipMock struct {
	workspaceRefreshMock
	reader *switchableDiffReader
}

func (m *refreshFlipMock) RefreshWorkspaceRepositories(ctx context.Context, repos []monoclient.WorkspaceRepository) (*monoclient.WorkspaceRefreshResult, error) {
	result, err := m.workspaceRefreshMock.RefreshWorkspaceRepositories(ctx, repos)
	m.reader.flip()
	return result, err
}

func TestSessionSocketPullMergesLocalChangesCleanly(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)

	reader := &switchableDiffReader{
		beforeContent: map[string]string{
			"github.com/acme/monofs/main.go": "package main\n\nfunc main() {\n}\n",
		},
		afterContent: map[string]string{
			"github.com/acme/monofs/main.go": "package main\n\nfunc main() {\n\tprintln(\"upstream\")\n}\n",
		},
	}
	handler.SetDiffReader(reader)
	refresher := &refreshFlipMock{
		workspaceRefreshMock: workspaceRefreshMock{
			result: &monoclient.WorkspaceRefreshResult{Requested: 1, Refreshed: 1},
		},
		reader: reader,
	}
	handler.SetWorkspaceRefresher(refresher)

	// Local modification in a different region of the same file.
	writeTrackedSessionFile(t, sessionMgr, "github.com/acme/monofs/main.go",
		"package main\n\nfunc main() {\n}\n\nfunc helper() {\n}\n", ChangeModify)

	resp := handler.handlePull()
	if !resp.Success {
		t.Fatalf("handlePull() error = %s", resp.Error)
	}
	if !strings.Contains(resp.Message, "1 merged") {
		t.Fatalf("handlePull() message = %q, want merge summary", resp.Message)
	}

	// The overlay file should now contain both changes.
	localPath, err := sessionMgr.GetLocalPath("github.com/acme/monofs/main.go")
	if err != nil {
		t.Fatalf("GetLocalPath() error = %v", err)
	}
	content, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	merged := string(content)
	if !strings.Contains(merged, `println("upstream")`) {
		t.Errorf("merged content missing upstream change:\n%s", merged)
	}
	if !strings.Contains(merged, "func helper()") {
		t.Errorf("merged content missing local change:\n%s", merged)
	}
	if hasConflictMarkers(content) {
		t.Errorf("merged content has conflict markers:\n%s", merged)
	}

	// No conflicts recorded.
	conflicts, err := sessionMgr.ListSessionConflicts()
	if err != nil {
		t.Fatalf("ListSessionConflicts() error = %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("conflicts = %v, want none", conflicts)
	}
}

func TestSessionSocketPullRecordsConflicts(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)

	reader := &switchableDiffReader{
		beforeContent: map[string]string{
			"github.com/acme/monofs/config.go": "value = \"base\"\n",
		},
		afterContent: map[string]string{
			"github.com/acme/monofs/config.go": "value = \"upstream\"\n",
		},
	}
	handler.SetDiffReader(reader)
	refresher := &refreshFlipMock{
		workspaceRefreshMock: workspaceRefreshMock{
			result: &monoclient.WorkspaceRefreshResult{Requested: 1, Refreshed: 1},
		},
		reader: reader,
	}
	handler.SetWorkspaceRefresher(refresher)

	// Local modification of the SAME line that upstream changed.
	writeTrackedSessionFile(t, sessionMgr, "github.com/acme/monofs/config.go",
		"value = \"local\"\n", ChangeModify)

	resp := handler.handlePull()
	if !resp.Success {
		t.Fatalf("handlePull() error = %s", resp.Error)
	}
	if !strings.Contains(resp.Message, "CONFLICTED") {
		t.Fatalf("handlePull() message = %q, want conflict summary", resp.Message)
	}

	conflicts, err := sessionMgr.ListSessionConflicts()
	if err != nil {
		t.Fatalf("ListSessionConflicts() error = %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].Path != "github.com/acme/monofs/config.go" {
		t.Fatalf("conflicts = %+v, want one for config.go", conflicts)
	}

	// The conflicts command lists them.
	conflictResp := handler.handleConflicts()
	if !conflictResp.Success || conflictResp.Conflicts != 1 {
		t.Fatalf("handleConflicts() = %+v, want one conflict", conflictResp)
	}
	if len(conflictResp.ConflictList) != 1 || conflictResp.ConflictList[0].Path != "github.com/acme/monofs/config.go" {
		t.Fatalf("handleConflicts() list = %+v", conflictResp.ConflictList)
	}

	// The overlay content carries conflict markers.
	localPath, _ := sessionMgr.GetLocalPath("github.com/acme/monofs/config.go")
	content, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !hasConflictMarkers(content) {
		t.Errorf("conflicted file missing markers:\n%s", string(content))
	}

	// Staging a file with markers is rejected.
	stageResp := handler.handleAdd([]string{"github.com/acme/monofs/config.go"})
	if stageResp.Success {
		t.Fatal("handleAdd() should reject unresolved conflict markers")
	}

	// Commit is blocked while conflicts remain.
	commitResp := handler.handleCommit(SessionRequest{Action: "commit", LogicalCommitMessage: "nope"})
	if commitResp.Success {
		t.Fatal("handleCommit() should be blocked by unresolved conflicts")
	}

	// Resolve: rewrite clean content, stage, and verify the conflict
	// state clears.
	resolved := []byte("value = \"resolved\"\n")
	if err := os.WriteFile(localPath, resolved, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := sessionMgr.TrackChange(ChangeModify, "github.com/acme/monofs/config.go", ""); err != nil {
		t.Fatalf("TrackChange() error = %v", err)
	}
	stageResp = handler.handleAdd([]string{"github.com/acme/monofs/config.go"})
	if !stageResp.Success {
		t.Fatalf("handleAdd() after resolution error = %s", stageResp.Error)
	}
	conflicts, _ = sessionMgr.ListSessionConflicts()
	if len(conflicts) != 0 {
		t.Fatalf("conflicts after resolution = %v, want none", conflicts)
	}
}

func TestSessionSocketPullDropsVacuousLocalChanges(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)

	reader := &switchableDiffReader{
		beforeContent: map[string]string{
			"github.com/acme/monofs/readme.md": "hello\n",
		},
		afterContent: map[string]string{
			"github.com/acme/monofs/readme.md": "hello upstream\n",
		},
	}
	handler.SetDiffReader(reader)
	refresher := &refreshFlipMock{
		workspaceRefreshMock: workspaceRefreshMock{
			result: &monoclient.WorkspaceRefreshResult{Requested: 1, Refreshed: 1},
		},
		reader: reader,
	}
	handler.SetWorkspaceRefresher(refresher)

	// Local content identical to the pre-refresh base: the local change
	// is vacuous and the upstream version should win.
	writeTrackedSessionFile(t, sessionMgr, "github.com/acme/monofs/readme.md", "hello\n", ChangeModify)

	resp := handler.handlePull()
	if !resp.Success {
		t.Fatalf("handlePull() error = %s", resp.Error)
	}
	if !strings.Contains(resp.Message, "1 dropped") {
		t.Fatalf("handlePull() message = %q, want drop summary", resp.Message)
	}

	// The overlay entry is gone: the change no longer counts.
	changes := sessionMgr.GetChanges()
	if len(changes) != 0 {
		t.Fatalf("changes after vacuous drop = %d, want 0", len(changes))
	}
}

func TestSessionSocketPullRejectsPendingLocalCommits(t *testing.T) {
	handler, sessionMgr := newVirtualMonorepoSessionHandler(t)
	handler.commitMgr = NewCommitManager(sessionMgr, &mockClient{workspaceRepos: testWorkspaceRepositories()}, testLogger())
	handler.commitMgr.SetWorkspaceManifest(handler.rootNode.WorkspaceManifest())
	refresher := &workspaceRefreshMock{result: &monoclient.WorkspaceRefreshResult{Requested: 3, Refreshed: 3}}
	handler.SetWorkspaceRefresher(refresher)

	writeTrackedSessionFile(t, sessionMgr, "github.com/acme/monofs/main.go", "package main\n", ChangeModify)
	if resp := handler.handleAdd([]string{"github.com/acme/monofs/main.go"}); !resp.Success {
		t.Fatalf("handleAdd() error = %s", resp.Error)
	}
	if resp := handler.handleCommit(SessionRequest{Action: "commit", LogicalCommitMessage: "checkpoint"}); !resp.Success {
		t.Fatalf("handleCommit() error = %s", resp.Error)
	}

	resp := handler.handlePull()
	if resp.Success {
		t.Fatal("handlePull() success = true, want rejection for pending local commits")
	}
	if !strings.Contains(resp.Error, "local commits pending") {
		t.Fatalf("handlePull() error = %q, want pending-local-commit guidance", resp.Error)
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

type sessionSocketGetAttrClient struct {
	*mockClient
	getattrFunc func(ctx context.Context, path string) (*pb.GetAttrResponse, error)
}

func (m *sessionSocketGetAttrClient) GetAttr(ctx context.Context, path string) (*pb.GetAttrResponse, error) {
	if m.getattrFunc != nil {
		return m.getattrFunc(ctx, path)
	}
	return m.mockClient.GetAttr(ctx, path)
}

func (m *workspaceRefreshMock) RefreshWorkspaceRepositories(ctx context.Context, repos []monoclient.WorkspaceRepository) (*monoclient.WorkspaceRefreshResult, error) {
	copyRepos := append([]monoclient.WorkspaceRepository(nil), repos...)
	m.calls = append(m.calls, copyRepos)
	return m.result, m.err
}

func newVirtualMonorepoSessionHandler(t *testing.T) (*SessionSocketHandler, *SessionManager) {
	return newVirtualMonorepoSessionHandlerWithClient(t, &mockClient{workspaceRepos: testWorkspaceRepositories()})
}

func newVirtualMonorepoSessionHandlerWithClient(t *testing.T, client monoclient.MonoFSClient) (*SessionSocketHandler, *SessionManager) {
	t.Helper()

	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	root := NewRootWithSession(client, nil, sessionMgr, testLogger())
	if err := root.EnableVirtualMonorepo(); err != nil {
		t.Fatalf("EnableVirtualMonorepo() error = %v", err)
	}

	handler := &SessionSocketHandler{
		sessionMgr:  sessionMgr,
		principalID: "principal-test",
		rootNode:    root,
		diffReader: DiffReaderFunc(func(ctx context.Context, path string) ([]byte, error) {
			return nil, status.Error(codes.NotFound, "missing")
		}),
		logger: testLogger(),
		ctx:    context.Background(),
	}
	return handler, sessionMgr
}

func testWorkspaceRepositories() []monoclient.WorkspaceRepository {
	return []monoclient.WorkspaceRepository{
		{StorageID: "repo-monofs", DisplayPath: "github.com/acme/monofs", Source: "ssh://git@example.com/acme/monofs.git", Ref: "main", CommitHash: "aaaaaaa1"},
		{StorageID: "repo-guardian", DisplayPath: "github.com/acme/guardian", Source: "ssh://git@example.com/acme/guardian.git", Ref: "main", CommitHash: "bbbbbbb2"},
		{StorageID: "repo-doctor", DisplayPath: "github.com/acme/doctor", Source: "ssh://git@example.com/acme/doctor.git", Ref: "release/2026-05", CommitHash: "ccccccc3"},
		{StorageID: "dep-cache", DisplayPath: "dependency/go/mod/cache", Ref: "blob", CommitHash: "ddddddd4"},
	}
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

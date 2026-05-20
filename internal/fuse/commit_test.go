package fuse

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	monoclient "github.com/radryc/monofs/internal/client"
	"github.com/radryc/monofs/internal/workspacebundle"
)

func TestCommitManagerRejectsDependencyChangesBeforeCommit(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	writeTrackedCommitFile(t, sessionMgr, "dependency/go/mod/cache/download/example.com/@v/v1.0.0.mod", "module example.com\n", ChangeCreate)

	commitMgr := NewCommitManager(sessionMgr, &mockClient{}, testLogger())
	if _, err := commitMgr.CommitChanges(context.Background(), CommitOptions{}); err == nil || !strings.Contains(err.Error(), "no staged source changes") {
		t.Fatalf("CommitChanges() error = %v, want staged-change guidance", err)
	}
	if sessionMgr.GetCurrentSession() == nil {
		t.Fatal("session should remain active after rejected dependency commit")
	}
}

func TestCommitManagerCreatesLocalVirtualCommitFromStagedIndex(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	writeTrackedCommitFile(t, sessionMgr, "github.com/acme/monofs/main.go", "package main\n", ChangeModify)
	writeTrackedCommitFile(t, sessionMgr, "github.com/acme/guardian/README.md", "guardian repo\n", ChangeCreate)
	writeTrackedCommitFile(t, sessionMgr, "github.com/acme/doctor/app.go", "doctor repo\n", ChangeCreate)
	writeTrackedCommitDir(t, sessionMgr, "github.com/acme/doctor/pkg")
	writeTrackedCommitDelete(t, sessionMgr, "github.com/acme/doctor/old.txt")
	if err := sessionMgr.CreateSymlink("github.com/acme/monofs/current", "main.go"); err != nil {
		t.Fatalf("CreateSymlink() error = %v", err)
	}

	mockCli := &commitPublisherMockClient{
		mockClient: &mockClient{
			workspaceRepos: []monoclient.WorkspaceRepository{
				{StorageID: "repo-monofs", DisplayPath: "github.com/acme/monofs", Source: "ssh://git@example.com/acme/monofs.git", Ref: "main", CommitHash: "abc111"},
				{StorageID: "repo-guardian", DisplayPath: "github.com/acme/guardian", Source: "ssh://git@example.com/acme/guardian.git", Ref: "main", CommitHash: "abc222"},
				{StorageID: "repo-doctor", DisplayPath: "github.com/acme/doctor", Source: "ssh://git@example.com/acme/doctor.git", Ref: "main", CommitHash: "abc333"},
			},
		},
		result: &monoclient.WorkspacePublishResult{
			RefreshedRepositories: []monoclient.WorkspaceRepository{{StorageID: "repo-monofs", DisplayPath: "github.com/acme/monofs"}},
		},
	}
	commitMgr := NewCommitManager(sessionMgr, mockCli, testLogger())
	commitMgr.SetWorkspaceManifest(NewWorkspaceManifest(mockCli))
	commitMgr.SetPrincipalID("principal-test")

	stageTrackedCommitEntry(t, sessionMgr, StagedIndexEntry{
		Path:                "github.com/acme/monofs/main.go",
		RepositoryStorageID: "repo-monofs",
		RepositoryPath:      "github.com/acme/monofs",
		ChangeType:          ChangeModify,
		Content:             []byte("package main\n"),
		Mode:                0o644,
		LocalPath:           localCommitPath(t, sessionMgr, "github.com/acme/monofs/main.go"),
	})
	stageTrackedCommitEntry(t, sessionMgr, StagedIndexEntry{
		Path:                "github.com/acme/guardian/README.md",
		RepositoryStorageID: "repo-guardian",
		RepositoryPath:      "github.com/acme/guardian",
		ChangeType:          ChangeCreate,
		Content:             []byte("guardian repo\n"),
		Mode:                0o644,
		LocalPath:           localCommitPath(t, sessionMgr, "github.com/acme/guardian/README.md"),
	})
	stageTrackedCommitEntry(t, sessionMgr, StagedIndexEntry{
		Path:                "github.com/acme/doctor/app.go",
		RepositoryStorageID: "repo-doctor",
		RepositoryPath:      "github.com/acme/doctor",
		ChangeType:          ChangeCreate,
		Content:             []byte("doctor repo\n"),
		Mode:                0o644,
		LocalPath:           localCommitPath(t, sessionMgr, "github.com/acme/doctor/app.go"),
	})
	stageTrackedCommitEntry(t, sessionMgr, StagedIndexEntry{
		Path:                "github.com/acme/doctor/pkg",
		RepositoryStorageID: "repo-doctor",
		RepositoryPath:      "github.com/acme/doctor",
		ChangeType:          ChangeMkdir,
		Mode:                0o755,
		LocalPath:           localCommitPath(t, sessionMgr, "github.com/acme/doctor/pkg"),
	})
	stageTrackedCommitEntry(t, sessionMgr, StagedIndexEntry{
		Path:                "github.com/acme/doctor/old.txt",
		RepositoryStorageID: "repo-doctor",
		RepositoryPath:      "github.com/acme/doctor",
		ChangeType:          ChangeDelete,
	})
	stageTrackedCommitEntry(t, sessionMgr, StagedIndexEntry{
		Path:                "github.com/acme/monofs/current",
		RepositoryStorageID: "repo-monofs",
		RepositoryPath:      "github.com/acme/monofs",
		ChangeType:          ChangeSymlink,
		Mode:                0o777,
		SymlinkTarget:       "main.go",
		LocalPath:           localCommitPath(t, sessionMgr, "github.com/acme/monofs/current"),
	})

	result, err := commitMgr.CommitChanges(context.Background(), CommitOptions{
		LogicalCommitMessage:    "test local commit",
		AuthorName:              "Test User",
		AuthorEmail:             "test@example.com",
		RequestedBranchStrategy: "direct",
	})
	if err != nil {
		t.Fatalf("CommitChanges() error = %v", err)
	}
	if !result.Success {
		t.Fatalf("CommitChanges() success = false, errors = %v", result.Errors)
	}
	if result.FilesProcessed != 6 || result.FilesUploaded != 0 || result.FilesFailed != 0 {
		t.Fatalf("CommitChanges() result = %+v, want processed=6 uploaded=0 failed=0", result)
	}
	if result.Repositories != 3 {
		t.Fatalf("CommitChanges() repositories = %d, want 3", result.Repositories)
	}
	if strings.TrimSpace(result.LocalCommitID) == "" {
		t.Fatal("expected local commit id in result")
	}
	if sessionMgr.GetCurrentSession() == nil {
		t.Fatal("session should remain active after local commit")
	}

	if len(mockCli.publishedBundles) != 0 {
		t.Fatalf("expected no published bundles, got %d", len(mockCli.publishedBundles))
	}

	stagedEntries, err := sessionMgr.ListStagedEntries()
	if err != nil {
		t.Fatalf("ListStagedEntries() error = %v", err)
	}
	if len(stagedEntries) != 0 {
		t.Fatalf("staged entries remain after commit: %+v", stagedEntries)
	}

	localCommits, err := sessionMgr.ListLocalVirtualCommits()
	if err != nil {
		t.Fatalf("ListLocalVirtualCommits() error = %v", err)
	}
	if len(localCommits) != 1 {
		t.Fatalf("local commits = %d, want 1", len(localCommits))
	}
	stored := localCommits[0]
	if stored.ID != result.LocalCommitID {
		t.Fatalf("stored local commit id = %q, want %q", stored.ID, result.LocalCommitID)
	}
	if stored.Message != "test local commit" || stored.AuthorName != "Test User" || stored.AuthorEmail != "test@example.com" || stored.PrincipalID != "principal-test" {
		t.Fatalf("stored commit metadata = %+v", stored)
	}

	gotRepos := make([]string, 0, len(stored.Repositories))
	gotOps := make([]string, 0)
	for _, repo := range stored.Repositories {
		gotRepos = append(gotRepos, repo.DisplayPath)
		for _, op := range repo.Operations {
			entry := repo.DisplayPath + ":" + op.Kind + ":" + op.Path
			if op.Target != "" {
				entry += ":" + op.Target
			}
			gotOps = append(gotOps, entry)
		}
	}
	sort.Strings(gotRepos)
	sort.Strings(gotOps)
	if strings.Join(gotRepos, ",") != "github.com/acme/doctor,github.com/acme/guardian,github.com/acme/monofs" {
		t.Fatalf("published repos = %v", gotRepos)
	}
	if strings.Join(gotOps, ",") != "github.com/acme/doctor:delete:old.txt,github.com/acme/doctor:mkdir:pkg,github.com/acme/doctor:upsert:app.go,github.com/acme/guardian:upsert:README.md,github.com/acme/monofs:symlink:current:main.go,github.com/acme/monofs:upsert:main.go" {
		t.Fatalf("published operations = %v", gotOps)
	}
	if changes := sessionMgr.GetChanges(); len(changes) != 0 {
		t.Fatalf("GetChanges() after commit = %+v, want clean working tree", changes)
	}
}

func TestCommitManagerReclassifiesCommittedCreateToModifyWhenWorktreeMovesAhead(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	path := "github.com/acme/monofs/new.go"
	writeTrackedCommitFile(t, sessionMgr, path, "package main\n", ChangeCreate)
	stageTrackedCommitEntry(t, sessionMgr, StagedIndexEntry{
		Path:                path,
		RepositoryStorageID: "repo-monofs",
		RepositoryPath:      "github.com/acme/monofs",
		ChangeType:          ChangeCreate,
		Content:             []byte("package main\n"),
		Mode:                0o644,
		LocalPath:           localCommitPath(t, sessionMgr, path),
	})

	localPath := localCommitPath(t, sessionMgr, path)
	if err := os.WriteFile(localPath, []byte("package main\n// ahead\n"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := sessionMgr.TrackChange(ChangeModify, path, ""); err != nil {
		t.Fatalf("TrackChange() error = %v", err)
	}

	mockCli := &commitPublisherMockClient{
		mockClient: &mockClient{
			workspaceRepos: []monoclient.WorkspaceRepository{{
				StorageID: "repo-monofs", DisplayPath: "github.com/acme/monofs", Source: "ssh://git@example.com/acme/monofs.git", Ref: "main", CommitHash: "abc111",
			}},
		},
	}
	commitMgr := NewCommitManager(sessionMgr, mockCli, testLogger())
	commitMgr.SetWorkspaceManifest(NewWorkspaceManifest(mockCli))

	first, err := commitMgr.CommitChanges(context.Background(), CommitOptions{LogicalCommitMessage: "first"})
	if err != nil {
		t.Fatalf("first CommitChanges() error = %v", err)
	}
	changes := sessionMgr.GetChanges()
	if len(changes) != 1 || changes[0].Path != path || changes[0].Type != ChangeModify {
		t.Fatalf("GetChanges() after first local commit = %+v, want one modify for %q", changes, path)
	}

	stageTrackedCommitEntry(t, sessionMgr, StagedIndexEntry{
		Path:                path,
		RepositoryStorageID: "repo-monofs",
		RepositoryPath:      "github.com/acme/monofs",
		ChangeType:          ChangeModify,
		Content:             []byte("package main\n// ahead\n"),
		Mode:                0o644,
		LocalPath:           localPath,
	})

	second, err := commitMgr.CommitChanges(context.Background(), CommitOptions{LogicalCommitMessage: "second"})
	if err != nil {
		t.Fatalf("second CommitChanges() error = %v", err)
	}

	localCommits, err := sessionMgr.ListLocalVirtualCommits()
	if err != nil {
		t.Fatalf("ListLocalVirtualCommits() error = %v", err)
	}
	if len(localCommits) != 2 {
		t.Fatalf("local commits = %d, want 2", len(localCommits))
	}
	if localCommits[1].ParentID != first.LocalCommitID {
		t.Fatalf("second ParentID = %q, want %q", localCommits[1].ParentID, first.LocalCommitID)
	}
	if second.LocalCommitID == first.LocalCommitID {
		t.Fatal("expected distinct local commit ids")
	}
}

func TestCommitManagerPushPendingLocalCommitsBuildsSourceBundleAndMarksBranchCommitsPushed(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	if err := sessionMgr.SetCurrentLogicalBranch("feature/demo"); err != nil {
		t.Fatalf("SetCurrentLogicalBranch() error = %v", err)
	}

	mockCli := &commitPublisherMockClient{
		mockClient: &mockClient{
			workspaceRepos: []monoclient.WorkspaceRepository{
				{StorageID: "repo-monofs", DisplayPath: "github.com/acme/monofs", Source: "ssh://git@example.com/acme/monofs.git", Ref: "main", CommitHash: "abc111"},
				{StorageID: "repo-guardian", DisplayPath: "github.com/acme/guardian", Source: "ssh://git@example.com/acme/guardian.git", Ref: "main", CommitHash: "abc222"},
			},
		},
		sourcePushResult: &monoclient.WorkspaceSourcePushResult{
			Job: &pb.WorkspaceSyncJob{
				JobId: "job-source-1",
				State: pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED,
				Repositories: []*pb.WorkspaceSyncRepositoryResult{{
					StorageId:    "repo-monofs",
					DisplayPath:  "github.com/acme/monofs",
					Branch:       "main",
					TargetBranch: "feature/demo",
					PushedCommit: "push123456789",
					Status:       pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_PUBLISHED,
					Message:      "repository pushed from 2 local commits",
				}},
			},
		},
	}
	commitMgr := NewCommitManager(sessionMgr, mockCli, testLogger())
	commitMgr.SetWorkspaceManifest(NewWorkspaceManifest(mockCli))
	commitMgr.SetPrincipalID("principal-test")

	baseTime := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
	for _, commit := range []LocalVirtualCommit{
		{
			ID:            "local-1",
			LogicalBranch: "feature/demo",
			Message:       "first",
			AuthorName:    "Test User",
			AuthorEmail:   "test@example.com",
			PrincipalID:   "principal-test",
			CreatedAt:     baseTime,
			Repositories: []LocalCommitRepository{{
				StorageID:   "repo-monofs",
				DisplayPath: "github.com/acme/monofs",
				Branch:      "main",
				BaseCommit:  "abc111",
				Operations: []LocalCommitOperation{{
					Kind:    workspacebundle.OperationUpsert,
					Path:    "main.go",
					Mode:    0o644,
					Content: []byte("package main\n"),
				}},
			}},
		},
		{
			ID:            "local-2",
			ParentID:      "local-1",
			LogicalBranch: "feature/demo",
			Message:       "second",
			PrincipalID:   "principal-test",
			CreatedAt:     baseTime.Add(time.Minute),
			Repositories: []LocalCommitRepository{{
				StorageID:   "repo-monofs",
				DisplayPath: "github.com/acme/monofs",
				Branch:      "main",
				BaseCommit:  "abc111",
				Operations: []LocalCommitOperation{{
					Kind: workspacebundle.OperationDelete,
					Path: "old.go",
				}},
			}},
		},
		{
			ID:            "local-3",
			LogicalBranch: "release/demo",
			Message:       "other branch",
			PrincipalID:   "principal-test",
			CreatedAt:     baseTime.Add(2 * time.Minute),
			Repositories: []LocalCommitRepository{{
				StorageID:   "repo-guardian",
				DisplayPath: "github.com/acme/guardian",
				Branch:      "main",
				BaseCommit:  "abc222",
				Operations: []LocalCommitOperation{{
					Kind:    workspacebundle.OperationUpsert,
					Path:    "README.md",
					Mode:    0o644,
					Content: []byte("guardian\n"),
				}},
			}},
		},
	} {
		if err := sessionMgr.PutLocalVirtualCommit(commit); err != nil {
			t.Fatalf("PutLocalVirtualCommit(%q) error = %v", commit.ID, err)
		}
	}

	result, err := commitMgr.PushPendingLocalCommits(context.Background())
	if err != nil {
		t.Fatalf("PushPendingLocalCommits() error = %v", err)
	}
	if !result.Success || result.PushedCommits != 2 || result.Repositories != 1 {
		t.Fatalf("PushPendingLocalCommits() result = %+v", result)
	}
	if len(mockCli.sourcePushBundles) != 1 {
		t.Fatalf("source push bundles = %d, want 1", len(mockCli.sourcePushBundles))
	}
	bundle := mockCli.sourcePushBundles[0].bundle
	if bundle.LogicalBranch != "feature/demo" || strings.Join(bundle.LocalCommitIDs(), ",") != "local-1,local-2" {
		t.Fatalf("source push bundle = %+v, want current branch commits only", bundle)
	}
	if len(bundle.RepositoryRefs()) != 1 || bundle.RepositoryRefs()[0].GetDisplayPath() != "github.com/acme/monofs" {
		t.Fatalf("bundle repositories = %+v, want monofs repo only", bundle.RepositoryRefs())
	}

	stored, err := sessionMgr.ListLocalVirtualCommits()
	if err != nil {
		t.Fatalf("ListLocalVirtualCommits() error = %v", err)
	}
	byID := make(map[string]LocalVirtualCommit, len(stored))
	for _, commit := range stored {
		byID[commit.ID] = commit
	}
	if !byID["local-1"].Pushed || byID["local-1"].PushJobID != "job-source-1" {
		t.Fatalf("local-1 after push = %+v, want pushed state", byID["local-1"])
	}
	if !byID["local-2"].Pushed || byID["local-2"].PushJobID != "job-source-1" {
		t.Fatalf("local-2 after push = %+v, want pushed state", byID["local-2"])
	}
	if byID["local-3"].Pushed {
		t.Fatalf("local-3 after push = %+v, want other branch to remain pending", byID["local-3"])
	}

	mapping, found, err := sessionMgr.GetBranchMapping("principal-test", "feature/demo", "repo-monofs")
	if err != nil {
		t.Fatalf("GetBranchMapping() error = %v", err)
	}
	if !found {
		t.Fatal("expected persisted branch mapping after source push")
	}
	if mapping.ActualBranch != "feature/demo" || mapping.LastPushedCommit != "push123456789" {
		t.Fatalf("branch mapping = %+v, want actual branch and pushed commit", mapping)
	}
}

type commitPublisherMockClient struct {
	*mockClient
	publishedBundles  []publishedWorkspaceBundle
	result            *monoclient.WorkspacePublishResult
	err               error
	sourcePushBundles []pushedSourceCommitBundle
	sourcePushResult  *monoclient.WorkspaceSourcePushResult
	sourcePushErr     error
}

type publishedWorkspaceBundle struct {
	bundle *workspacebundle.Bundle
	opts   monoclient.WorkspacePublishOptions
}

type pushedSourceCommitBundle struct {
	bundle *workspacebundle.SourceCommitBundle
}

func (m *mockClient) ApplyRepositoryChanges(ctx context.Context, repo monoclient.WorkspaceRepository, changes []monoclient.RepositoryChange) (*monoclient.ApplyRepositoryChangesResult, error) {
	return &monoclient.ApplyRepositoryChangesResult{FilesUpserted: len(changes)}, nil
}

func (m *commitPublisherMockClient) PublishWorkspaceBundle(ctx context.Context, bundle *workspacebundle.Bundle, opts monoclient.WorkspacePublishOptions) (*monoclient.WorkspacePublishResult, error) {
	m.publishedBundles = append(m.publishedBundles, publishedWorkspaceBundle{bundle: bundle, opts: opts})
	if m.err != nil {
		return nil, m.err
	}
	if m.result != nil {
		return m.result, nil
	}
	return &monoclient.WorkspacePublishResult{}, nil
}

func (m *commitPublisherMockClient) PushWorkspaceCommitBundle(ctx context.Context, bundle *workspacebundle.SourceCommitBundle) (*monoclient.WorkspaceSourcePushResult, error) {
	m.sourcePushBundles = append(m.sourcePushBundles, pushedSourceCommitBundle{bundle: bundle})
	if m.sourcePushErr != nil {
		return nil, m.sourcePushErr
	}
	if m.sourcePushResult != nil {
		return m.sourcePushResult, nil
	}
	return &monoclient.WorkspaceSourcePushResult{Job: &pb.WorkspaceSyncJob{JobId: "job-default", State: pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED}}, nil
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

func writeTrackedCommitDir(t *testing.T, sessionMgr *SessionManager, monofsPath string) {
	t.Helper()

	localPath, err := sessionMgr.GetLocalPath(monofsPath)
	if err != nil {
		t.Fatalf("GetLocalPath(%q) error = %v", monofsPath, err)
	}
	if err := os.MkdirAll(localPath, 0755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", localPath, err)
	}
	if err := sessionMgr.TrackChange(ChangeMkdir, monofsPath, ""); err != nil {
		t.Fatalf("TrackChange(%q) error = %v", monofsPath, err)
	}
}

func writeTrackedCommitDelete(t *testing.T, sessionMgr *SessionManager, monofsPath string) {
	t.Helper()
	if err := sessionMgr.TrackChange(ChangeDelete, monofsPath, ""); err != nil {
		t.Fatalf("TrackChange(%q) error = %v", monofsPath, err)
	}
}

func stageTrackedCommitEntry(t *testing.T, sessionMgr *SessionManager, entry StagedIndexEntry) {
	t.Helper()
	if entry.StagedAt.IsZero() {
		entry.StagedAt = time.Now().UTC()
	}
	if err := sessionMgr.PutStagedEntry(entry); err != nil {
		t.Fatalf("PutStagedEntry(%q) error = %v", entry.Path, err)
	}
}

func localCommitPath(t *testing.T, sessionMgr *SessionManager, monofsPath string) string {
	t.Helper()
	localPath, err := sessionMgr.GetLocalPath(monofsPath)
	if err != nil {
		t.Fatalf("GetLocalPath(%q) error = %v", monofsPath, err)
	}
	return localPath
}

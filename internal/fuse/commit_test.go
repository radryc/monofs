package fuse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

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
	if _, err := commitMgr.CommitChanges(context.Background(), CommitOptions{}); err == nil || !strings.Contains(err.Error(), "monofs-session push") {
		t.Fatalf("CommitChanges() error = %v, want dependency push guidance", err)
	}
	if sessionMgr.GetCurrentSession() == nil {
		t.Fatal("session should remain active after rejected dependency commit")
	}
}

func TestCommitManagerPublishesVirtualMonorepoBundle(t *testing.T) {
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

	result, err := commitMgr.CommitChanges(context.Background(), CommitOptions{
		LogicalCommitMessage:    "test publish",
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
	if result.FilesProcessed != 6 || result.FilesUploaded != 6 || result.FilesFailed != 0 {
		t.Fatalf("CommitChanges() result = %+v, want processed/uploaded=3 and failed=0", result)
	}
	if result.Repositories != 3 {
		t.Fatalf("CommitChanges() repositories = %d, want 3", result.Repositories)
	}
	if sessionMgr.GetCurrentSession() != nil {
		t.Fatal("session should be archived after successful virtual monorepo commit")
	}

	if len(mockCli.publishedBundles) != 1 {
		t.Fatalf("expected 1 published bundle, got %d", len(mockCli.publishedBundles))
	}
	published := mockCli.publishedBundles[0]
	if published.opts.LogicalCommitMessage != "test publish" || published.opts.AuthorName != "Test User" || published.opts.AuthorEmail != "test@example.com" || published.opts.RequestedBranchStrategy != "direct" {
		t.Fatalf("unexpected publish opts: %+v", published.opts)
	}
	if published.bundle.WorkspaceID == "" {
		t.Fatal("expected bundle workspace id")
	}

	gotRepos := make([]string, 0, len(published.bundle.Repositories))
	gotOps := make([]string, 0)
	for _, repo := range published.bundle.Repositories {
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
	if len(result.RefreshedRepositories) != 1 || result.RefreshedRepositories[0].DisplayPath != "github.com/acme/monofs" {
		t.Fatalf("refreshed repositories = %+v", result.RefreshedRepositories)
	}
}

func TestCommitManagerKeepsSessionOnPublishFailure(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	writeTrackedCommitFile(t, sessionMgr, "github.com/acme/monofs/main.go", "package main\n", ChangeModify)

	mockCli := &commitPublisherMockClient{
		mockClient: &mockClient{
			workspaceRepos: []monoclient.WorkspaceRepository{{
				StorageID: "repo-monofs", DisplayPath: "github.com/acme/monofs", Source: "ssh://git@example.com/acme/monofs.git", Ref: "main", CommitHash: "abc111",
			}},
		},
		err: errors.New("publish failed"),
	}
	commitMgr := NewCommitManager(sessionMgr, mockCli, testLogger())
	commitMgr.SetWorkspaceManifest(NewWorkspaceManifest(mockCli))

	if _, err := commitMgr.CommitChanges(context.Background(), CommitOptions{}); err == nil || !strings.Contains(err.Error(), "publish failed") {
		t.Fatalf("CommitChanges() error = %v, want publish failure", err)
	}
	if sessionMgr.GetCurrentSession() == nil {
		t.Fatal("session should remain active after publish failure")
	}
}

type commitPublisherMockClient struct {
	*mockClient
	publishedBundles []publishedWorkspaceBundle
	result           *monoclient.WorkspacePublishResult
	err              error
}

type publishedWorkspaceBundle struct {
	bundle *workspacebundle.Bundle
	opts   monoclient.WorkspacePublishOptions
}

func (m *mockClient) ApplyRepositoryChanges(ctx context.Context, repo monoclient.WorkspaceRepository, changes []monoclient.RepositoryChange) (*monoclient.ApplyRepositoryChangesResult, error) {
	return &monoclient.ApplyRepositoryChangesResult{FilesUpserted: len(changes)}, nil
}

func (m *mockClient) PublishWorkspaceBundle(ctx context.Context, bundle *workspacebundle.Bundle, opts monoclient.WorkspacePublishOptions) (*monoclient.WorkspacePublishResult, error) {
	return nil, errors.New("workspace publish not configured")
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

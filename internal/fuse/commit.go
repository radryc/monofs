// Package fuse implements the FUSE filesystem layer for MonoFS.
package fuse

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/radryc/monofs/internal/client"
	"github.com/radryc/monofs/internal/sharding"
	"github.com/radryc/monofs/internal/workspacebundle"
)

// CommitManager handles pushing local changes to the backend
type CommitManager struct {
	sessionMgr *SessionManager
	client     commitClient
	workspace  *WorkspaceManifest
	logger     *slog.Logger
	mu         sync.Mutex
}

type commitChangeScope int

const (
	commitChangeWorkspace commitChangeScope = iota
	commitChangeBlob
	commitChangeExcluded
)

type classifiedCommitChange struct {
	Change
	Scope      commitChangeScope
	Repository *client.WorkspaceRepository
}

type commitRepositoryGroup struct {
	Key         string
	DisplayPath string
	Repository  *client.WorkspaceRepository
	Changes     []Change
}

type repositoryChangeApplier interface {
	ApplyRepositoryChanges(ctx context.Context, repo client.WorkspaceRepository, changes []client.RepositoryChange) (*client.ApplyRepositoryChangesResult, error)
}

type workspaceBundlePublisher interface {
	PublishWorkspaceBundle(ctx context.Context, bundle *workspacebundle.Bundle, opts client.WorkspacePublishOptions) (*client.WorkspacePublishResult, error)
}

type commitClient interface {
	client.MonoFSClient
	repositoryChangeApplier
	workspaceBundlePublisher
}

type CommitOptions struct {
	LogicalCommitMessage    string
	AuthorName              string
	AuthorEmail             string
	RequestedBranchStrategy string
}

// NewCommitManager creates a new commit manager
func NewCommitManager(sessionMgr *SessionManager, c commitClient, logger *slog.Logger) *CommitManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &CommitManager{
		sessionMgr: sessionMgr,
		client:     c,
		logger:     logger.With("component", "commit"),
	}
}

// SetWorkspaceManifest enables virtual-monorepo commit classification.
func (cm *CommitManager) SetWorkspaceManifest(manifest *WorkspaceManifest) {
	cm.workspace = manifest
}

// CommitResult represents the result of a commit operation
type CommitResult struct {
	Success               bool              // Overall success
	Repositories          int               // Number of repositories affected
	FilesProcessed        int               // Number of files processed
	FilesUploaded         int               // Number of files successfully uploaded
	FilesFailed           int               // Number of files that failed
	Errors                map[string]string // Path -> error message
	SessionID             string            // Committed session ID
	Message               string
	RefreshedRepositories []client.WorkspaceRepository
}

// CommitChanges pushes all local changes to the backend
func (cm *CommitManager) CommitChanges(ctx context.Context, opts CommitOptions) (*CommitResult, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	session := cm.sessionMgr.GetCurrentSession()
	if session == nil {
		return nil, fmt.Errorf("no active session")
	}

	result := &CommitResult{
		Success:   true,
		Errors:    make(map[string]string),
		SessionID: session.ID,
	}

	changes := cm.sessionMgr.GetChanges()
	if len(changes) == 0 {
		cm.logger.Info("no changes to commit")
		// Archive empty session
		if err := cm.sessionMgr.CommitSession(); err != nil {
			return nil, fmt.Errorf("archive session: %w", err)
		}
		return result, nil
	}

	classified, err := cm.classifyCommitChanges(ctx, changes)
	if err != nil {
		return nil, err
	}

	workspaceChanges := make([]Change, 0, len(classified))
	blobCount := 0
	excludedCount := 0
	for _, change := range classified {
		switch change.Scope {
		case commitChangeBlob:
			blobCount++
		case commitChangeExcluded:
			excludedCount++
		case commitChangeWorkspace:
			workspaceChanges = append(workspaceChanges, change.Change)
		}
	}

	if blobCount > 0 {
		return nil, fmt.Errorf("%d dependency changes pending; run monofs-session push first", blobCount)
	}
	if excludedCount > 0 {
		return nil, fmt.Errorf("%d excluded changes pending outside the virtual monorepo; discard them or use the system view", excludedCount)
	}

	if len(workspaceChanges) == 0 {
		cm.logger.Info("no non-blob changes to commit")
		if err := cm.sessionMgr.CommitSession(); err != nil {
			return nil, fmt.Errorf("archive session: %w", err)
		}
		return result, nil
	}

	repoGroups := cm.groupChangesByRepository(classified)
	result.Repositories = len(repoGroups)
	if cm.workspace != nil {
		bundle, filesProcessed, err := cm.buildWorkspaceBundle(session.ID, repoGroups)
		if err != nil {
			return nil, err
		}
		result.FilesProcessed = filesProcessed

		publishResult, err := cm.client.PublishWorkspaceBundle(ctx, bundle, client.WorkspacePublishOptions{
			WorkspaceID:             session.ID,
			LogicalCommitMessage:    opts.LogicalCommitMessage,
			AuthorName:              opts.AuthorName,
			AuthorEmail:             opts.AuthorEmail,
			RequestedBranchStrategy: opts.RequestedBranchStrategy,
		})
		if err != nil {
			return nil, err
		}

		result.FilesUploaded = filesProcessed
		result.RefreshedRepositories = append(result.RefreshedRepositories, publishResult.RefreshedRepositories...)
		if publishResult.Warning != "" {
			result.Message = publishResult.Warning
		}

		if err := cm.sessionMgr.CommitSession(); err != nil {
			return nil, fmt.Errorf("archive session: %w", err)
		}
		return result, nil
	}

	for _, repoGroup := range repoGroups {
		result.FilesProcessed += len(repoGroup.Changes)

		repo, err := cm.resolveCommitRepository(repoGroup)
		if err != nil {
			result.Success = false
			for _, change := range repoGroup.Changes {
				result.FilesFailed++
				result.Errors[change.Path] = err.Error()
			}
			continue
		}

		repoChanges, err := cm.repositoryClientChanges(*repo, repoGroup.Changes)
		if err != nil {
			result.Success = false
			for _, change := range repoGroup.Changes {
				result.FilesFailed++
				result.Errors[change.Path] = err.Error()
			}
			continue
		}

		if len(repoChanges) > 0 {
			if _, err := cm.client.ApplyRepositoryChanges(ctx, *repo, repoChanges); err != nil {
				result.Success = false
				for _, change := range repoGroup.Changes {
					result.FilesFailed++
					result.Errors[change.Path] = err.Error()
				}
				continue
			}
		}

		result.FilesUploaded += len(repoGroup.Changes)
	}

	if err := cm.sessionMgr.CommitSession(); err != nil {
		cm.logger.Warn("failed to archive session", "error", err)
	}

	return result, nil
}

func (cm *CommitManager) classifyCommitChanges(ctx context.Context, changes []Change) ([]classifiedCommitChange, error) {
	classified := make([]classifiedCommitChange, 0, len(changes))
	for _, change := range changes {
		item := classifiedCommitChange{Change: change}
		if isDependencyPath(change.Path) {
			item.Scope = commitChangeBlob
			classified = append(classified, item)
			continue
		}
		if cm.workspace == nil {
			item.Scope = commitChangeWorkspace
			classified = append(classified, item)
			continue
		}

		resolution, err := cm.workspace.ResolvePath(ctx, change.Path)
		if err != nil {
			return nil, fmt.Errorf("resolve workspace path %q: %w", change.Path, err)
		}
		if resolution.Repository != nil {
			repoCopy := *resolution.Repository
			item.Repository = &repoCopy
		}
		if !resolution.Included {
			item.Scope = commitChangeExcluded
		} else {
			item.Scope = commitChangeWorkspace
		}
		classified = append(classified, item)
	}
	return classified, nil
}

func (cm *CommitManager) groupChangesByRepository(changes []classifiedCommitChange) []commitRepositoryGroup {
	groups := make(map[string]*commitRepositoryGroup)
	for _, change := range changes {
		if change.Scope != commitChangeWorkspace {
			continue
		}

		key := "_root"
		displayPath := "_root"
		if change.Repository != nil {
			key = change.Repository.StorageID
			if key == "" {
				key = change.Repository.DisplayPath
			}
			displayPath = change.Repository.DisplayPath
		} else {
			parts := strings.SplitN(change.Path, "/", 4)
			if len(parts) >= 3 {
				displayPath = strings.Join(parts[:3], "/")
				key = displayPath
			}
		}

		group := groups[key]
		if group == nil {
			group = &commitRepositoryGroup{Key: key, DisplayPath: displayPath}
			if change.Repository != nil {
				repoCopy := *change.Repository
				group.Repository = &repoCopy
			} else if displayPath != "_root" {
				group.Repository = &client.WorkspaceRepository{
					StorageID:   sharding.GenerateStorageID(displayPath),
					DisplayPath: displayPath,
				}
			}
			groups[key] = group
		}
		group.Changes = append(group.Changes, change.Change)
	}

	out := make([]commitRepositoryGroup, 0, len(groups))
	for _, group := range groups {
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].DisplayPath < out[j].DisplayPath
	})
	return out
}

func formatCommitRepositorySummary(groups []commitRepositoryGroup) string {
	if len(groups) == 0 {
		return ""
	}
	const maxRepos = 3
	names := make([]string, 0, len(groups))
	for _, group := range groups {
		names = append(names, group.DisplayPath)
	}
	if len(names) > maxRepos {
		return fmt.Sprintf(": %s and %d more", strings.Join(names[:maxRepos], ", "), len(names)-maxRepos)
	}
	return ": " + strings.Join(names, ", ")
}

func (cm *CommitManager) resolveCommitRepository(group commitRepositoryGroup) (*client.WorkspaceRepository, error) {
	if group.Repository != nil {
		repoCopy := *group.Repository
		if repoCopy.StorageID == "" && repoCopy.DisplayPath != "" {
			repoCopy.StorageID = sharding.GenerateStorageID(repoCopy.DisplayPath)
		}
		return &repoCopy, nil
	}
	if group.DisplayPath == "" || group.DisplayPath == "_root" {
		return nil, fmt.Errorf("cannot resolve repository for commit group %q", group.DisplayPath)
	}
	return &client.WorkspaceRepository{
		StorageID:   sharding.GenerateStorageID(group.DisplayPath),
		DisplayPath: group.DisplayPath,
	}, nil
}

func (cm *CommitManager) repositoryClientChanges(repo client.WorkspaceRepository, changes []Change) ([]client.RepositoryChange, error) {
	out := make([]client.RepositoryChange, 0, len(changes))
	for _, change := range changes {
		repoRelativePath, err := commitRepositoryRelativePath(repo.DisplayPath, change.Path)
		if err != nil {
			return nil, err
		}

		switch change.Type {
		case ChangeCreate, ChangeModify:
			content, mode, err := loadCommitLocalFile(change.LocalPath)
			if err != nil {
				return nil, err
			}
			out = append(out, client.RepositoryChange{
				Kind:    client.RepositoryChangeUpsert,
				Path:    repoRelativePath,
				Content: content,
				Mode:    mode,
				Mtime:   time.Now().Unix(),
			})
		case ChangeDelete:
			out = append(out, client.RepositoryChange{
				Kind: client.RepositoryChangeDelete,
				Path: repoRelativePath,
			})
		case ChangeMkdir, ChangeRmdir:
			continue
		case ChangeSymlink:
			return nil, fmt.Errorf("symlink commits are not implemented for %q", change.Path)
		default:
			return nil, fmt.Errorf("unknown change type: %s", change.Type)
		}
	}
	return out, nil
}

func (cm *CommitManager) buildWorkspaceBundle(workspaceID string, repoGroups []commitRepositoryGroup) (*workspacebundle.Bundle, int, error) {
	bundle := &workspacebundle.Bundle{
		WorkspaceID:  workspaceID,
		Repositories: make([]workspacebundle.RepositoryBundle, 0, len(repoGroups)),
	}
	processed := 0

	for _, repoGroup := range repoGroups {
		repo, err := cm.resolveCommitRepository(repoGroup)
		if err != nil {
			return nil, 0, err
		}
		if strings.TrimSpace(repo.Source) == "" {
			return nil, 0, fmt.Errorf("repository %q has no source configured for publish", repo.DisplayPath)
		}
		if strings.TrimSpace(repo.Ref) == "" {
			return nil, 0, fmt.Errorf("repository %q has no branch configured for publish", repo.DisplayPath)
		}
		if strings.TrimSpace(repo.CommitHash) == "" {
			return nil, 0, fmt.Errorf("repository %q has no base commit configured for publish", repo.DisplayPath)
		}

		repoBundle := workspacebundle.RepositoryBundle{
			StorageID:   repo.StorageID,
			DisplayPath: repo.DisplayPath,
			RepoURL:     repo.Source,
			Branch:      repo.Ref,
			BaseCommit:  repo.CommitHash,
			Operations:  make([]workspacebundle.Operation, 0, len(repoGroup.Changes)),
		}

		for _, change := range repoGroup.Changes {
			op, include, err := cm.workspaceBundleOperation(*repo, change)
			if err != nil {
				return nil, 0, err
			}
			if !include {
				continue
			}
			repoBundle.Operations = append(repoBundle.Operations, op)
			processed++
		}

		if len(repoBundle.Operations) == 0 {
			continue
		}
		bundle.Repositories = append(bundle.Repositories, repoBundle)
	}

	if err := bundle.Validate(); err != nil {
		return nil, 0, err
	}
	return bundle, processed, nil
}

func (cm *CommitManager) workspaceBundleOperation(repo client.WorkspaceRepository, change Change) (workspacebundle.Operation, bool, error) {
	repoRelativePath, err := commitRepositoryRelativePath(repo.DisplayPath, change.Path)
	if err != nil {
		return workspacebundle.Operation{}, false, err
	}

	switch change.Type {
	case ChangeCreate, ChangeModify:
		content, mode, err := loadCommitLocalFile(change.LocalPath)
		if err != nil {
			return workspacebundle.Operation{}, false, err
		}
		return workspacebundle.Operation{
			Kind:    workspacebundle.OperationUpsert,
			Path:    repoRelativePath,
			Mode:    int64(mode),
			Content: content,
		}, true, nil
	case ChangeDelete:
		return workspacebundle.Operation{Kind: workspacebundle.OperationDelete, Path: repoRelativePath}, true, nil
	case ChangeMkdir:
		mode := commitLocalMode(change.LocalPath, 0755)
		return workspacebundle.Operation{Kind: workspacebundle.OperationMkdir, Path: repoRelativePath, Mode: int64(mode)}, true, nil
	case ChangeRmdir:
		return workspacebundle.Operation{Kind: workspacebundle.OperationRmdir, Path: repoRelativePath}, true, nil
	case ChangeSymlink:
		target, err := cm.commitSymlinkTarget(change)
		if err != nil {
			return workspacebundle.Operation{}, false, err
		}
		return workspacebundle.Operation{Kind: workspacebundle.OperationSymlink, Path: repoRelativePath, Target: target}, true, nil
	default:
		return workspacebundle.Operation{}, false, fmt.Errorf("unknown change type: %s", change.Type)
	}
}

func commitLocalMode(localPath string, fallback os.FileMode) uint32 {
	if localPath == "" {
		return uint32(fallback.Perm())
	}
	info, err := os.Lstat(localPath)
	if err != nil {
		return uint32(fallback.Perm())
	}
	return uint32(info.Mode().Perm())
}

func (cm *CommitManager) commitSymlinkTarget(change Change) (string, error) {
	if strings.TrimSpace(change.SymlinkTarget) != "" {
		return change.SymlinkTarget, nil
	}
	if target, ok := cm.sessionMgr.GetSymlinkTarget(change.Path); ok {
		return target, nil
	}
	if strings.TrimSpace(change.LocalPath) == "" {
		return "", fmt.Errorf("symlink target missing for %q", change.Path)
	}
	target, err := os.Readlink(change.LocalPath)
	if err != nil {
		return "", fmt.Errorf("read symlink target for %q: %w", change.Path, err)
	}
	return target, nil
}

func commitRepositoryRelativePath(displayPath, fullPath string) (string, error) {
	trimmedDisplayPath := strings.Trim(displayPath, "/")
	trimmedFullPath := strings.Trim(fullPath, "/")
	if trimmedDisplayPath == "" {
		return "", fmt.Errorf("empty repository display path for %q", fullPath)
	}
	if trimmedFullPath == trimmedDisplayPath {
		return "", fmt.Errorf("path %q does not name a file within repository %q", fullPath, displayPath)
	}
	prefix := trimmedDisplayPath + "/"
	if !strings.HasPrefix(trimmedFullPath, prefix) {
		return "", fmt.Errorf("path %q does not belong to repository %q", fullPath, displayPath)
	}
	return strings.TrimPrefix(trimmedFullPath, prefix), nil
}

func loadCommitLocalFile(localPath string) ([]byte, uint32, error) {
	content, err := os.ReadFile(localPath)
	if err != nil {
		return nil, 0, fmt.Errorf("read local file %q: %w", localPath, err)
	}
	info, err := os.Lstat(localPath)
	if err != nil {
		return nil, 0, fmt.Errorf("stat local file %q: %w", localPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, fmt.Errorf("symlink commits are not implemented for %q", localPath)
	}
	return content, uint32(info.Mode().Perm()), nil
}

// DryRun returns what would be committed without actually committing
func (cm *CommitManager) DryRun() (*CommitResult, error) {
	session := cm.sessionMgr.GetCurrentSession()
	if session == nil {
		return nil, fmt.Errorf("no active session")
	}

	changes := cm.sessionMgr.GetChanges()

	result := &CommitResult{
		Success:        true,
		FilesProcessed: len(changes),
		SessionID:      session.ID,
		Errors:         make(map[string]string),
	}

	for _, change := range changes {
		// Check if local file still exists for create/modify
		if change.Type == ChangeCreate || change.Type == ChangeModify {
			if _, err := os.Stat(change.LocalPath); err != nil {
				result.Errors[change.Path] = fmt.Sprintf("file not found: %v", err)
				result.FilesFailed++
			}
		}
	}

	return result, nil
}

// Package fuse implements the FUSE filesystem layer for MonoFS.
package fuse

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/client"
	"github.com/radryc/monofs/internal/sharding"
	"github.com/radryc/monofs/internal/workspacebundle"
)

// CommitManager handles pushing local changes to the backend
type CommitManager struct {
	sessionMgr  *SessionManager
	client      commitClient
	workspace   *WorkspaceManifest
	principalID string
	logger      *slog.Logger
	mu          sync.Mutex
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

type workspaceCommitBundlePusher interface {
	PushWorkspaceCommitBundle(ctx context.Context, bundle *workspacebundle.SourceCommitBundle) (*client.WorkspaceSourcePushResult, error)
}

type commitClient interface {
	client.MonoFSClient
	repositoryChangeApplier
	workspaceBundlePublisher
	workspaceCommitBundlePusher
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

// SetPrincipalID records the mounted client identity used for local commits.
func (cm *CommitManager) SetPrincipalID(principalID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cm.principalID = strings.TrimSpace(principalID)
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
	LocalCommitID         string
}

type PushResult struct {
	Success       bool
	SessionID     string
	LogicalBranch string
	PushedCommits int
	Repositories  int
	JobID         string
	Message       string
}

// CommitChanges creates a new local virtual commit from the staged index.
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

	stagedEntries, err := cm.sessionMgr.ListStagedEntries()
	if err != nil {
		return nil, fmt.Errorf("list staged entries: %w", err)
	}
	if len(stagedEntries) == 0 {
		return nil, fmt.Errorf("no staged source changes; run monofs-session add or monofs-session rm first")
	}

	localCommit, filesProcessed, err := cm.buildLocalVirtualCommit(ctx, stagedEntries, opts)
	if err != nil {
		return nil, err
	}
	result.Repositories = len(localCommit.Repositories)
	result.FilesProcessed = filesProcessed
	result.LocalCommitID = localCommit.ID

	if err := cm.sessionMgr.PutLocalVirtualCommit(localCommit); err != nil {
		return nil, fmt.Errorf("persist local commit: %w", err)
	}
	if err := cm.advanceOverlayBaseline(stagedEntries); err != nil {
		_ = cm.sessionMgr.DeleteLocalVirtualCommit(localCommit.ID)
		return nil, err
	}
	if err := cm.sessionMgr.ClearStagedEntries(); err != nil {
		return nil, fmt.Errorf("clear staged entries: %w", err)
	}

	return result, nil
}

func (cm *CommitManager) PushPendingLocalCommits(ctx context.Context) (*PushResult, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	session := cm.sessionMgr.GetCurrentSession()
	if session == nil {
		return nil, fmt.Errorf("no active session")
	}

	logicalBranch, found, err := cm.sessionMgr.GetCurrentLogicalBranch()
	if err != nil {
		return nil, fmt.Errorf("read current logical branch: %w", err)
	}
	if !found {
		logicalBranch = ""
	}

	pendingCommits, err := cm.pendingLocalCommitsForBranch(logicalBranch)
	if err != nil {
		return nil, err
	}
	if len(pendingCommits) == 0 {
		return nil, fmt.Errorf("no pending local commits on the current logical branch")
	}

	bundle, err := cm.buildSourceCommitBundle(ctx, cm.workspacePushWorkspaceID(session), logicalBranch, pendingCommits)
	if err != nil {
		return nil, err
	}

	pushResult, err := cm.client.PushWorkspaceCommitBundle(ctx, bundle)
	if err != nil {
		return nil, err
	}
	if pushResult == nil || pushResult.Job == nil {
		return nil, fmt.Errorf("source push finished without a job summary")
	}

	pushedAt := time.Now().UTC()
	for _, commit := range pendingCommits {
		commit.Pushed = true
		commit.PushJobID = pushResult.Job.GetJobId()
		commit.PushedAt = pushedAt
		if err := cm.sessionMgr.PutLocalVirtualCommit(commit); err != nil {
			return nil, fmt.Errorf("persist pushed local commit %q: %w", commit.ID, err)
		}
	}
	if err := cm.recordSourcePushBranchMappings(logicalBranch, pushResult.Job.GetRepositories()); err != nil {
		return nil, err
	}

	repoCount := len(bundle.RepositoryRefs())
	return &PushResult{
		Success:       true,
		SessionID:     session.ID,
		LogicalBranch: logicalBranch,
		PushedCommits: len(pendingCommits),
		Repositories:  repoCount,
		JobID:         pushResult.Job.GetJobId(),
		Message:       formatSourcePushSummary(len(pendingCommits), repoCount, logicalBranch),
	}, nil
}

func (cm *CommitManager) buildLocalVirtualCommit(ctx context.Context, stagedEntries []StagedIndexEntry, opts CommitOptions) (LocalVirtualCommit, int, error) {
	repoGroups, err := cm.groupStagedEntriesByRepository(ctx, stagedEntries)
	if err != nil {
		return LocalVirtualCommit{}, 0, err
	}

	logicalBranch, foundLogicalBranch, err := cm.sessionMgr.GetCurrentLogicalBranch()
	if err != nil {
		return LocalVirtualCommit{}, 0, fmt.Errorf("read current logical branch: %w", err)
	}
	if !foundLogicalBranch {
		logicalBranch = ""
	}

	localCommits, err := cm.sessionMgr.ListLocalVirtualCommits()
	if err != nil {
		return LocalVirtualCommit{}, 0, fmt.Errorf("list local commits: %w", err)
	}

	createdAt := time.Now().UTC()
	commit := LocalVirtualCommit{
		ID:            newLocalCommitID(),
		ParentID:      latestLocalCommitID(localCommits, logicalBranch),
		LogicalBranch: logicalBranch,
		Message:       defaultLocalCommitMessage(opts.LogicalCommitMessage),
		AuthorName:    strings.TrimSpace(opts.AuthorName),
		AuthorEmail:   strings.TrimSpace(opts.AuthorEmail),
		PrincipalID:   strings.TrimSpace(cm.principalID),
		CreatedAt:     createdAt,
		Repositories:  make([]LocalCommitRepository, 0, len(repoGroups)),
	}

	processed := 0
	for _, repoGroup := range repoGroups {
		repoCommit := LocalCommitRepository{
			StorageID:   repoGroup.Repository.StorageID,
			DisplayPath: repoGroup.Repository.DisplayPath,
			RepoURL:     repoGroup.Repository.Source,
			Branch:      repoGroup.Repository.Ref,
			BaseCommit:  repoGroup.Repository.CommitHash,
			Operations:  make([]LocalCommitOperation, 0, len(repoGroup.Entries)),
		}

		for _, entry := range repoGroup.Entries {
			op, err := localCommitOperationFromStagedEntry(repoGroup.Repository.DisplayPath, entry)
			if err != nil {
				return LocalVirtualCommit{}, 0, err
			}
			repoCommit.Operations = append(repoCommit.Operations, op)
			processed++
		}

		sort.Slice(repoCommit.Operations, func(i, j int) bool {
			if repoCommit.Operations[i].Path != repoCommit.Operations[j].Path {
				return repoCommit.Operations[i].Path < repoCommit.Operations[j].Path
			}
			return repoCommit.Operations[i].Kind < repoCommit.Operations[j].Kind
		})
		commit.Repositories = append(commit.Repositories, repoCommit)
	}

	sort.Slice(commit.Repositories, func(i, j int) bool {
		return commit.Repositories[i].DisplayPath < commit.Repositories[j].DisplayPath
	})

	return commit, processed, nil
}

func (cm *CommitManager) pendingLocalCommitsForBranch(logicalBranch string) ([]LocalVirtualCommit, error) {
	localCommits, err := cm.sessionMgr.ListLocalVirtualCommits()
	if err != nil {
		return nil, fmt.Errorf("list local commits: %w", err)
	}
	sortLocalVirtualCommits(localCommits)
	filtered := make([]LocalVirtualCommit, 0, len(localCommits))
	for _, commit := range localCommits {
		if commit.Pushed {
			continue
		}
		if strings.TrimSpace(commit.LogicalBranch) != strings.TrimSpace(logicalBranch) {
			continue
		}
		filtered = append(filtered, commit)
	}
	return filtered, nil
}

func (cm *CommitManager) workspacePushWorkspaceID(session *WriteSession) string {
	if principalID := strings.TrimSpace(cm.principalID); principalID != "" {
		return principalID
	}
	if session != nil {
		return session.ID
	}
	return fmt.Sprintf("workspace-%d", time.Now().UnixNano())
}

func (cm *CommitManager) buildSourceCommitBundle(ctx context.Context, workspaceID, logicalBranch string, commits []LocalVirtualCommit) (*workspacebundle.SourceCommitBundle, error) {
	repoMetadata, err := cm.workspaceRepositoryMetadata(ctx)
	if err != nil {
		return nil, err
	}
	metadataByStorage := make(map[string]client.WorkspaceRepository, len(repoMetadata))
	for _, repo := range repoMetadata {
		if strings.TrimSpace(repo.StorageID) != "" {
			metadataByStorage[repo.StorageID] = repo
		}
	}

	bundle := &workspacebundle.SourceCommitBundle{
		WorkspaceID:   workspaceID,
		PrincipalID:   strings.TrimSpace(cm.principalID),
		LogicalBranch: logicalBranch,
		Commits:       make([]workspacebundle.SourceCommit, 0, len(commits)),
	}
	for _, commit := range commits {
		sourceCommit := workspacebundle.SourceCommit{
			ID:            commit.ID,
			ParentID:      commit.ParentID,
			Message:       commit.Message,
			AuthorName:    commit.AuthorName,
			AuthorEmail:   commit.AuthorEmail,
			PrincipalID:   commit.PrincipalID,
			CreatedAtUnix: commit.CreatedAt.Unix(),
			Repositories:  make([]workspacebundle.SourceCommitRepository, 0, len(commit.Repositories)),
		}
		for _, repo := range commit.Repositories {
			converted, err := sourceCommitRepositoryFromLocal(repo, repoMetadata, metadataByStorage)
			if err != nil {
				return nil, fmt.Errorf("build source push repository %q for commit %q: %w", repo.DisplayPath, commit.ID, err)
			}
			sourceCommit.Repositories = append(sourceCommit.Repositories, converted)
		}
		bundle.Commits = append(bundle.Commits, sourceCommit)
	}
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	return bundle, nil
}

func sourceCommitRepositoryFromLocal(repo LocalCommitRepository, metadataByDisplayPath, metadataByStorage map[string]client.WorkspaceRepository) (workspacebundle.SourceCommitRepository, error) {
	resolved := client.WorkspaceRepository{}
	if metadata, ok := metadataByStorage[strings.TrimSpace(repo.StorageID)]; ok {
		resolved = metadata
	}
	if resolved.DisplayPath == "" {
		if metadata, ok := metadataByDisplayPath[strings.TrimSpace(repo.DisplayPath)]; ok {
			resolved = metadata
		}
	}

	displayPath := strings.TrimSpace(repo.DisplayPath)
	if displayPath == "" {
		displayPath = strings.TrimSpace(resolved.DisplayPath)
	}
	storageID := strings.TrimSpace(repo.StorageID)
	if storageID == "" {
		storageID = strings.TrimSpace(resolved.StorageID)
	}
	if storageID == "" && displayPath != "" {
		storageID = sharding.GenerateStorageID(displayPath)
	}

	repoURL := strings.TrimSpace(repo.RepoURL)
	if repoURL == "" {
		repoURL = strings.TrimSpace(resolved.Source)
	}
	branch := strings.TrimSpace(repo.Branch)
	if branch == "" {
		branch = strings.TrimSpace(resolved.Ref)
	}
	baseCommit := strings.TrimSpace(repo.BaseCommit)
	if baseCommit == "" {
		baseCommit = strings.TrimSpace(resolved.CommitHash)
	}

	operations := make([]workspacebundle.Operation, 0, len(repo.Operations))
	for _, op := range repo.Operations {
		operations = append(operations, workspacebundle.Operation{
			Kind:    op.Kind,
			Path:    op.Path,
			Mode:    int64(op.Mode),
			Content: append([]byte(nil), op.Content...),
			Target:  op.Target,
		})
	}

	return workspacebundle.SourceCommitRepository{
		StorageID:   storageID,
		DisplayPath: displayPath,
		RepoURL:     repoURL,
		Branch:      branch,
		BaseCommit:  baseCommit,
		Operations:  operations,
	}, nil
}

func (cm *CommitManager) recordSourcePushBranchMappings(logicalBranch string, repos []*pb.WorkspaceSyncRepositoryResult) error {
	logicalBranch = strings.TrimSpace(logicalBranch)
	principalID := strings.TrimSpace(cm.principalID)
	if logicalBranch == "" || principalID == "" {
		return nil
	}
	now := time.Now().UTC()
	for _, repo := range repos {
		if repo == nil || strings.TrimSpace(repo.GetStorageId()) == "" {
			continue
		}
		mapping, found, err := cm.sessionMgr.GetBranchMapping(principalID, logicalBranch, repo.GetStorageId())
		if err != nil {
			return fmt.Errorf("read branch mapping for %q: %w", repo.GetDisplayPath(), err)
		}
		if !found {
			mapping = SessionBranchMapping{
				PrincipalID:    principalID,
				LogicalBranch:  logicalBranch,
				StorageID:      repo.GetStorageId(),
				DisplayPath:    repo.GetDisplayPath(),
				OriginalBranch: repo.GetBranch(),
				CreatedAt:      now,
			}
		}
		if mapping.CreatedAt.IsZero() {
			mapping.CreatedAt = now
		}
		if strings.TrimSpace(mapping.DisplayPath) == "" {
			mapping.DisplayPath = repo.GetDisplayPath()
		}
		if strings.TrimSpace(mapping.OriginalBranch) == "" {
			mapping.OriginalBranch = repo.GetBranch()
		}
		if targetBranch := strings.TrimSpace(repo.GetTargetBranch()); targetBranch != "" {
			mapping.ActualBranch = targetBranch
		} else if strings.TrimSpace(mapping.ActualBranch) == "" {
			mapping.ActualBranch = repo.GetBranch()
		}
		if pushedCommit := strings.TrimSpace(repo.GetPushedCommit()); pushedCommit != "" {
			mapping.LastPushedCommit = pushedCommit
		}
		if err := cm.sessionMgr.PutBranchMapping(mapping); err != nil {
			return fmt.Errorf("persist branch mapping for %q: %w", repo.GetDisplayPath(), err)
		}
	}
	return nil
}

func formatSourcePushSummary(commitCount, repoCount int, logicalBranch string) string {
	message := fmt.Sprintf("pushed %d local commit(s) across %d repositor(y/ies)", commitCount, repoCount)
	if strings.TrimSpace(logicalBranch) == "" {
		return message + " to tracked upstream branches"
	}
	return message + fmt.Sprintf(" on logical branch %s", logicalBranch)
}

type stagedRepositoryGroup struct {
	Repository client.WorkspaceRepository
	Entries    []StagedIndexEntry
}

func (cm *CommitManager) groupStagedEntriesByRepository(ctx context.Context, stagedEntries []StagedIndexEntry) ([]stagedRepositoryGroup, error) {
	repoMetadata, err := cm.workspaceRepositoryMetadata(ctx)
	if err != nil {
		return nil, err
	}

	groups := make(map[string]*stagedRepositoryGroup)
	for _, entry := range stagedEntries {
		repo, err := cm.resolveStagedRepository(entry, repoMetadata)
		if err != nil {
			return nil, err
		}
		key := repo.StorageID
		if strings.TrimSpace(key) == "" {
			key = repo.DisplayPath
		}
		group := groups[key]
		if group == nil {
			group = &stagedRepositoryGroup{Repository: repo, Entries: make([]StagedIndexEntry, 0, 1)}
			groups[key] = group
		}
		group.Entries = append(group.Entries, entry)
	}

	out := make([]stagedRepositoryGroup, 0, len(groups))
	for _, group := range groups {
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Repository.DisplayPath < out[j].Repository.DisplayPath
	})
	return out, nil
}

func (cm *CommitManager) workspaceRepositoryMetadata(ctx context.Context) (map[string]client.WorkspaceRepository, error) {
	if cm.workspace == nil {
		return nil, nil
	}
	entries, err := cm.workspace.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workspace repositories: %w", err)
	}
	repos := make(map[string]client.WorkspaceRepository, len(entries))
	for _, entry := range entries {
		if !entry.Included {
			continue
		}
		repos[entry.Repository.DisplayPath] = entry.Repository
	}
	return repos, nil
}

func (cm *CommitManager) resolveStagedRepository(entry StagedIndexEntry, repoMetadata map[string]client.WorkspaceRepository) (client.WorkspaceRepository, error) {
	displayPath := strings.Trim(strings.TrimSpace(entry.RepositoryPath), "/")
	if displayPath == "" {
		parts := strings.Split(strings.Trim(strings.TrimSpace(entry.Path), "/"), "/")
		if len(parts) < 4 {
			return client.WorkspaceRepository{}, fmt.Errorf("cannot infer repository for staged path %q", entry.Path)
		}
		displayPath = strings.Join(parts[:3], "/")
	}

	if repoMetadata != nil {
		if repo, found := repoMetadata[displayPath]; found {
			if strings.TrimSpace(repo.StorageID) == "" && strings.TrimSpace(entry.RepositoryStorageID) != "" {
				repo.StorageID = entry.RepositoryStorageID
			}
			return repo, nil
		}
	}

	storageID := strings.TrimSpace(entry.RepositoryStorageID)
	if storageID == "" {
		storageID = sharding.GenerateStorageID(displayPath)
	}
	return client.WorkspaceRepository{
		StorageID:   storageID,
		DisplayPath: displayPath,
	}, nil
}

func localCommitOperationFromStagedEntry(displayPath string, entry StagedIndexEntry) (LocalCommitOperation, error) {
	repoRelativePath, err := commitRepositoryRelativePath(displayPath, entry.Path)
	if err != nil {
		return LocalCommitOperation{}, err
	}

	switch entry.ChangeType {
	case ChangeCreate, ChangeModify:
		return LocalCommitOperation{
			Kind:    workspacebundle.OperationUpsert,
			Path:    repoRelativePath,
			Mode:    entry.Mode,
			Content: append([]byte(nil), entry.Content...),
		}, nil
	case ChangeDelete:
		return LocalCommitOperation{Kind: workspacebundle.OperationDelete, Path: repoRelativePath}, nil
	case ChangeMkdir:
		mode := entry.Mode
		if mode == 0 {
			mode = 0o755
		}
		return LocalCommitOperation{Kind: workspacebundle.OperationMkdir, Path: repoRelativePath, Mode: mode}, nil
	case ChangeRmdir:
		return LocalCommitOperation{Kind: workspacebundle.OperationRmdir, Path: repoRelativePath}, nil
	case ChangeSymlink:
		if strings.TrimSpace(entry.SymlinkTarget) == "" {
			return LocalCommitOperation{}, fmt.Errorf("symlink target missing for %q", entry.Path)
		}
		return LocalCommitOperation{Kind: workspacebundle.OperationSymlink, Path: repoRelativePath, Target: entry.SymlinkTarget}, nil
	default:
		return LocalCommitOperation{}, fmt.Errorf("unsupported staged change type %q", entry.ChangeType)
	}
}

func newLocalCommitID() string {
	id := generateSessionID()
	if len(id) > 12 {
		id = id[:12]
	}
	return "local-" + id
}

func defaultLocalCommitMessage(message string) string {
	if trimmed := strings.TrimSpace(message); trimmed != "" {
		return trimmed
	}
	return "local commit"
}

func latestLocalCommitID(commits []LocalVirtualCommit, logicalBranch string) string {
	if len(commits) == 0 {
		return ""
	}
	branch := strings.TrimSpace(logicalBranch)
	sorted := append([]LocalVirtualCommit(nil), commits...)
	sortLocalVirtualCommits(sorted)
	for index := len(sorted) - 1; index >= 0; index-- {
		if strings.TrimSpace(sorted[index].LogicalBranch) == branch {
			return sorted[index].ID
		}
	}
	return ""
}

func (cm *CommitManager) advanceOverlayBaseline(stagedEntries []StagedIndexEntry) error {
	for _, entry := range stagedEntries {
		if err := cm.reconcileCommittedEntry(entry); err != nil {
			return err
		}
	}
	return nil
}

func (cm *CommitManager) reconcileCommittedEntry(entry StagedIndexEntry) error {
	localPath, err := cm.sessionMgr.GetLocalPath(entry.Path)
	if err != nil {
		return fmt.Errorf("resolve local path for %q: %w", entry.Path, err)
	}

	if entry.ChangeType == ChangeDelete || entry.ChangeType == ChangeRmdir {
		if _, statErr := os.Lstat(localPath); os.IsNotExist(statErr) || cm.sessionMgr.IsDeleted(entry.Path) {
			return cm.sessionMgr.GetOverlayDB().MarkDeletedWithType(entry.Path, ChangeBaseline)
		} else if statErr != nil {
			return fmt.Errorf("stat committed path %q: %w", entry.Path, statErr)
		}
		return cm.reclassifyPathFromDisk(entry.Path, true)
	}

	if cm.sessionMgr.IsDeleted(entry.Path) {
		deleteType := ChangeDelete
		if entry.ChangeType == ChangeMkdir {
			deleteType = ChangeRmdir
		}
		return cm.sessionMgr.GetOverlayDB().MarkDeletedWithType(entry.Path, deleteType)
	}

	matches, currentType, err := stagedEntryMatchesCurrent(entry, localPath)
	if err != nil {
		return err
	}
	if matches {
		return cm.putOverlayFileFromDisk(entry.Path, ChangeBaseline)
	}

	switch currentType {
	case FileEntrySymlink:
		return cm.putOverlayFileFromDisk(entry.Path, ChangeSymlink)
	case FileEntryDir:
		if entry.ChangeType == ChangeMkdir {
			return cm.putOverlayFileFromDisk(entry.Path, ChangeBaseline)
		}
		return cm.putOverlayFileFromDisk(entry.Path, ChangeMkdir)
	default:
		return cm.putOverlayFileFromDisk(entry.Path, ChangeModify)
	}
}

func stagedEntryMatchesCurrent(entry StagedIndexEntry, localPath string) (bool, FileEntryType, error) {
	info, err := os.Lstat(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("stat committed path %q: %w", entry.Path, err)
	}

	currentType := fileEntryTypeFromInfo(info)
	switch entry.ChangeType {
	case ChangeCreate, ChangeModify:
		if currentType != FileEntryRegular {
			return false, currentType, nil
		}
		content, mode, err := loadCommitLocalFile(localPath)
		if err != nil {
			return false, currentType, err
		}
		return bytes.Equal(content, entry.Content) && (entry.Mode == 0 || mode == entry.Mode), currentType, nil
	case ChangeMkdir:
		if currentType != FileEntryDir {
			return false, currentType, nil
		}
		return true, currentType, nil
	case ChangeSymlink:
		if currentType != FileEntrySymlink {
			return false, currentType, nil
		}
		target, err := os.Readlink(localPath)
		if err != nil {
			return false, currentType, fmt.Errorf("read symlink target for %q: %w", entry.Path, err)
		}
		return target == entry.SymlinkTarget, currentType, nil
	default:
		return false, currentType, nil
	}
}

func fileEntryTypeFromInfo(info os.FileInfo) FileEntryType {
	if info.Mode()&os.ModeSymlink != 0 {
		return FileEntrySymlink
	}
	if info.IsDir() {
		return FileEntryDir
	}
	return FileEntryRegular
}

func (cm *CommitManager) reclassifyPathFromDisk(monofsPath string, baselineWasDelete bool) error {
	localPath, err := cm.sessionMgr.GetLocalPath(monofsPath)
	if err != nil {
		return err
	}
	info, err := os.Lstat(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return cm.sessionMgr.GetOverlayDB().MarkDeletedWithType(monofsPath, ChangeBaseline)
		}
		return fmt.Errorf("stat %q: %w", monofsPath, err)
	}

	switch fileEntryTypeFromInfo(info) {
	case FileEntryDir:
		return cm.putOverlayFileFromDisk(monofsPath, ChangeMkdir)
	case FileEntrySymlink:
		return cm.putOverlayFileFromDisk(monofsPath, ChangeSymlink)
	default:
		changeType := ChangeModify
		if baselineWasDelete {
			changeType = ChangeCreate
		}
		return cm.putOverlayFileFromDisk(monofsPath, changeType)
	}
}

func (cm *CommitManager) putOverlayFileFromDisk(monofsPath string, changeType ChangeType) error {
	db := cm.sessionMgr.GetOverlayDB()
	if db == nil {
		return fmt.Errorf("overlay database not available")
	}

	localPath, err := cm.sessionMgr.GetLocalPath(monofsPath)
	if err != nil {
		return err
	}
	info, err := os.Lstat(localPath)
	if err != nil {
		return fmt.Errorf("stat %q: %w", monofsPath, err)
	}

	entry := FileEntry{
		Type:       fileEntryTypeFromInfo(info),
		LocalPath:  localPath,
		Mode:       uint32(info.Mode().Perm()),
		Size:       uint64(info.Size()),
		Mtime:      info.ModTime().Unix(),
		ChangeType: changeType,
		Timestamp:  time.Now().UTC(),
	}
	if entry.Type == FileEntrySymlink {
		target, err := os.Readlink(localPath)
		if err != nil {
			return fmt.Errorf("read symlink target for %q: %w", monofsPath, err)
		}
		entry.SymlinkTarget = target
	}
	if existing, found, err := db.GetFile(monofsPath); err == nil && found {
		entry.OrigHash = existing.OrigHash
	} else if err != nil {
		return err
	}
	return db.PutFile(monofsPath, entry)
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

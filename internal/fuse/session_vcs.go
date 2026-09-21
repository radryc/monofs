package fuse

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// StagedIndexEntry is a persisted snapshot of a source change selected for the
// next local virtual commit.
type StagedIndexEntry struct {
	Path                string     `json:"path"`
	RepositoryStorageID string     `json:"repository_storage_id,omitempty"`
	RepositoryPath      string     `json:"repository_path,omitempty"`
	ChangeType          ChangeType `json:"change_type"`
	StagedAt            time.Time  `json:"staged_at"`
	Content             []byte     `json:"content,omitempty"`
	Mode                uint32     `json:"mode,omitempty"`
	SymlinkTarget       string     `json:"symlink_target,omitempty"`
	LocalPath           string     `json:"local_path,omitempty"`
}

// LocalCommitOperation is a persisted repo-relative operation belonging to a
// local virtual commit.
type LocalCommitOperation struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Mode    uint32 `json:"mode,omitempty"`
	Content []byte `json:"content,omitempty"`
	Target  string `json:"target,omitempty"`
}

// LocalCommitRepository groups operations for one repository inside a local
// virtual commit.
type LocalCommitRepository struct {
	StorageID   string                 `json:"storage_id"`
	DisplayPath string                 `json:"display_path"`
	RepoURL     string                 `json:"repo_url,omitempty"`
	Branch      string                 `json:"branch,omitempty"`
	BaseCommit  string                 `json:"base_commit,omitempty"`
	Operations  []LocalCommitOperation `json:"operations,omitempty"`
}

// LocalVirtualCommit is a session-local commit that has not necessarily been
// pushed upstream yet.
type LocalVirtualCommit struct {
	ID            string                  `json:"id"`
	ParentID      string                  `json:"parent_id,omitempty"`
	LogicalBranch string                  `json:"logical_branch,omitempty"`
	Message       string                  `json:"message"`
	AuthorName    string                  `json:"author_name,omitempty"`
	AuthorEmail   string                  `json:"author_email,omitempty"`
	PrincipalID   string                  `json:"principal_id,omitempty"`
	CreatedAt     time.Time               `json:"created_at"`
	Repositories  []LocalCommitRepository `json:"repositories,omitempty"`
	Pushed        bool                    `json:"pushed,omitempty"`
	PushJobID     string                  `json:"push_job_id,omitempty"`
	PushedAt      time.Time               `json:"pushed_at,omitempty"`
}

// SessionBranchMapping records the actual remote branch assigned to a logical
// branch for one repository and principal.
type SessionBranchMapping struct {
	PrincipalID      string    `json:"principal_id"`
	LogicalBranch    string    `json:"logical_branch"`
	StorageID        string    `json:"storage_id"`
	DisplayPath      string    `json:"display_path,omitempty"`
	OriginalBranch   string    `json:"original_branch,omitempty"`
	ActualBranch     string    `json:"actual_branch"`
	LastPushedCommit string    `json:"last_pushed_commit,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// SessionConflict records an unresolved 3-way merge conflict produced by
// pulling upstream changes into a session with local modifications.
type SessionConflict struct {
	Path       string    `json:"path"`
	Reason     string    `json:"reason,omitempty"`
	BaseCommit string    `json:"base_commit,omitempty"`
	TheirsRef  string    `json:"theirs_ref,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// PutSessionConflict records an unresolved merge conflict.
func (sm *SessionManager) PutSessionConflict(conflict SessionConflict) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return fmt.Errorf("no active session")
	}
	return sm.db.PutSessionConflict(conflict)
}

// GetSessionConflict returns the recorded conflict for a path.
func (sm *SessionManager) GetSessionConflict(path string) (SessionConflict, bool, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return SessionConflict{}, false, nil
	}
	return sm.db.GetSessionConflict(path)
}

// ListSessionConflicts returns all unresolved merge conflicts.
func (sm *SessionManager) ListSessionConflicts() ([]SessionConflict, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return nil, nil
	}
	return sm.db.ListSessionConflicts()
}

// ClearSessionConflict marks a conflicted path as resolved.
func (sm *SessionManager) ClearSessionConflict(path string) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return fmt.Errorf("no active session")
	}
	return sm.db.DeleteSessionConflict(path)
}

// HasSessionConflicts reports whether any unresolved merge conflicts
// remain.
func (sm *SessionManager) HasSessionConflicts() bool {
	conflicts, err := sm.ListSessionConflicts()
	return err == nil && len(conflicts) > 0
}

func (sm *SessionManager) PutStagedEntry(entry StagedIndexEntry) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return fmt.Errorf("no active session")
	}
	entry.Path = strings.TrimSpace(entry.Path)
	if entry.Path == "" {
		return fmt.Errorf("staged entry path is required")
	}
	if entry.StagedAt.IsZero() {
		entry.StagedAt = time.Now().UTC()
	}
	return sm.db.PutStagedEntry(entry.Path, entry)
}

func (sm *SessionManager) GetStagedEntry(path string) (StagedIndexEntry, bool, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return StagedIndexEntry{}, false, nil
	}
	return sm.db.GetStagedEntry(path)
}

func (sm *SessionManager) ListStagedEntries() ([]StagedIndexEntry, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return nil, nil
	}
	return sm.db.ListStagedEntries()
}

func (sm *SessionManager) DeleteStagedEntry(path string) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return fmt.Errorf("no active session")
	}
	return sm.db.DeleteStagedEntry(path)
}

func (sm *SessionManager) ClearStagedEntries() error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return fmt.Errorf("no active session")
	}
	return sm.db.ClearStagedEntries()
}

func (sm *SessionManager) PutLocalVirtualCommit(commit LocalVirtualCommit) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return fmt.Errorf("no active session")
	}
	commit.ID = strings.TrimSpace(commit.ID)
	if commit.ID == "" {
		return fmt.Errorf("local virtual commit id is required")
	}
	if commit.CreatedAt.IsZero() {
		commit.CreatedAt = time.Now().UTC()
	}
	return sm.db.PutLocalVirtualCommit(commit)
}

func (sm *SessionManager) GetLocalVirtualCommit(id string) (LocalVirtualCommit, bool, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return LocalVirtualCommit{}, false, nil
	}
	return sm.db.GetLocalVirtualCommit(id)
}

func (sm *SessionManager) ListLocalVirtualCommits() ([]LocalVirtualCommit, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return nil, nil
	}
	return sm.db.ListLocalVirtualCommits()
}

func (sm *SessionManager) DeleteLocalVirtualCommit(id string) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return fmt.Errorf("no active session")
	}
	return sm.db.DeleteLocalVirtualCommit(id)
}

func (sm *SessionManager) SetCurrentLogicalBranch(branch string) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return fmt.Errorf("no active session")
	}
	return sm.db.SetCurrentLogicalBranch(branch)
}

func (sm *SessionManager) GetCurrentLogicalBranch() (string, bool, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return "", false, nil
	}
	return sm.db.GetCurrentLogicalBranch()
}

func (sm *SessionManager) PutBranchMapping(mapping SessionBranchMapping) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return fmt.Errorf("no active session")
	}
	return sm.db.PutBranchMapping(mapping)
}

func (sm *SessionManager) GetBranchMapping(principalID, logicalBranch, storageID string) (SessionBranchMapping, bool, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return SessionBranchMapping{}, false, nil
	}
	return sm.db.GetBranchMapping(principalID, logicalBranch, storageID)
}

func (sm *SessionManager) ListBranchMappings() ([]SessionBranchMapping, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return nil, nil
	}
	return sm.db.ListBranchMappings()
}

func (sm *SessionManager) DeleteBranchMapping(principalID, logicalBranch, storageID string) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return fmt.Errorf("no active session")
	}
	return sm.db.DeleteBranchMapping(principalID, logicalBranch, storageID)
}

// DeleteLogicalBranch removes a logical branch: its unpushed local virtual
// commits and its branch mappings. Pushed commits are intentionally retained
// (they reflect work already published upstream). If the deleted branch was the
// current branch, the current branch is cleared.
func (sm *SessionManager) DeleteLogicalBranch(branch string) (deletedCommits, deletedMappings int, err error) {
	if branch == "" {
		return 0, 0, fmt.Errorf("logical branch name is required")
	}
	sm.mu.RLock()
	if sm.current == nil || sm.db == nil {
		sm.mu.RUnlock()
		return 0, 0, fmt.Errorf("no active session")
	}
	db := sm.db
	sm.mu.RUnlock()

	commits, err := db.ListLocalVirtualCommits()
	if err != nil {
		return 0, 0, err
	}
	for _, c := range commits {
		if c.LogicalBranch != branch || c.Pushed {
			continue
		}
		if err := db.DeleteLocalVirtualCommit(c.ID); err != nil {
			return deletedCommits, deletedMappings, err
		}
		deletedCommits++
	}

	mappings, err := db.ListBranchMappings()
	if err != nil {
		return deletedCommits, deletedMappings, err
	}
	for _, m := range mappings {
		if m.LogicalBranch != branch {
			continue
		}
		if err := db.DeleteBranchMapping(m.PrincipalID, m.LogicalBranch, m.StorageID); err != nil {
			return deletedCommits, deletedMappings, err
		}
		deletedMappings++
	}

	if current, found, err := db.GetCurrentLogicalBranch(); err == nil && found && current == branch {
		_ = db.SetCurrentLogicalBranch("")
	}

	return deletedCommits, deletedMappings, nil
}

func sortLocalVirtualCommits(commits []LocalVirtualCommit) {
	sort.Slice(commits, func(left, right int) bool {
		if commits[left].CreatedAt.Equal(commits[right].CreatedAt) {
			return commits[left].ID < commits[right].ID
		}
		return commits[left].CreatedAt.Before(commits[right].CreatedAt)
	})
}

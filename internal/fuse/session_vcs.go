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

func sortLocalVirtualCommits(commits []LocalVirtualCommit) {
	sort.Slice(commits, func(left, right int) bool {
		if commits[left].CreatedAt.Equal(commits[right].CreatedAt) {
			return commits[left].ID < commits[right].ID
		}
		return commits[left].CreatedAt.Before(commits[right].CreatedAt)
	})
}

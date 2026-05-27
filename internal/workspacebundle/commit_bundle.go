package workspacebundle

import (
	"encoding/json"
	"fmt"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
)

type SourceCommitBundle struct {
	WorkspaceID   string         `json:"workspace_id"`
	PrincipalID   string         `json:"principal_id,omitempty"`
	LogicalBranch string         `json:"logical_branch,omitempty"`
	Commits       []SourceCommit `json:"commits"`
}

type SourceCommit struct {
	ID            string                   `json:"id"`
	ParentID      string                   `json:"parent_id,omitempty"`
	Message       string                   `json:"message"`
	AuthorName    string                   `json:"author_name,omitempty"`
	AuthorEmail   string                   `json:"author_email,omitempty"`
	PrincipalID   string                   `json:"principal_id,omitempty"`
	CreatedAtUnix int64                    `json:"created_at_unix,omitempty"`
	Repositories  []SourceCommitRepository `json:"repositories"`
}

type SourceCommitRepository struct {
	StorageID   string      `json:"storage_id"`
	DisplayPath string      `json:"display_path"`
	RepoURL     string      `json:"repo_url"`
	Branch      string      `json:"branch"`
	BaseCommit  string      `json:"base_commit"`
	Operations  []Operation `json:"operations"`
}

func ParseSourceCommitBundle(data []byte) (*SourceCommitBundle, error) {
	var bundle SourceCommitBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("decode source commit bundle: %w", err)
	}
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	return &bundle, nil
}

func (b *SourceCommitBundle) Validate() error {
	if b == nil {
		return fmt.Errorf("source commit bundle is required")
	}
	if strings.TrimSpace(b.WorkspaceID) == "" {
		return fmt.Errorf("workspace_id is required")
	}
	if len(b.Commits) == 0 {
		return fmt.Errorf("source commit bundle must include at least one commit")
	}
	seenCommits := make(map[string]struct{}, len(b.Commits))
	for i := range b.Commits {
		commit := b.Commits[i]
		if strings.TrimSpace(commit.ID) == "" {
			return fmt.Errorf("commit %d id is required", i)
		}
		if _, ok := seenCommits[commit.ID]; ok {
			return fmt.Errorf("duplicate commit id %q", commit.ID)
		}
		seenCommits[commit.ID] = struct{}{}
		if len(commit.Repositories) == 0 {
			return fmt.Errorf("commit %q must include at least one repository", commit.ID)
		}
		seenRepos := make(map[string]struct{}, len(commit.Repositories))
		for j := range commit.Repositories {
			repo := commit.Repositories[j]
			if strings.TrimSpace(repo.StorageID) == "" {
				return fmt.Errorf("commit %q repository %d storage_id is required", commit.ID, j)
			}
			if _, ok := seenRepos[repo.StorageID]; ok {
				return fmt.Errorf("commit %q contains duplicate repository storage_id %q", commit.ID, repo.StorageID)
			}
			seenRepos[repo.StorageID] = struct{}{}
			if strings.TrimSpace(repo.DisplayPath) == "" {
				return fmt.Errorf("commit %q repository %q display_path is required", commit.ID, repo.StorageID)
			}
			if strings.TrimSpace(repo.RepoURL) == "" {
				return fmt.Errorf("commit %q repository %q repo_url is required", commit.ID, repo.StorageID)
			}
			if strings.TrimSpace(repo.Branch) == "" {
				return fmt.Errorf("commit %q repository %q branch is required", commit.ID, repo.StorageID)
			}
			if strings.TrimSpace(repo.BaseCommit) == "" {
				return fmt.Errorf("commit %q repository %q base_commit is required", commit.ID, repo.StorageID)
			}
			for k := range repo.Operations {
				if err := validateOperation(repo.StorageID, k, repo.Operations[k]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (b *SourceCommitBundle) RepositoryRefs() []*pb.WorkspaceRepositoryRef {
	if b == nil {
		return nil
	}
	refs := make([]*pb.WorkspaceRepositoryRef, 0)
	seen := make(map[string]struct{})
	for _, commit := range b.Commits {
		for _, repo := range commit.Repositories {
			if _, ok := seen[repo.StorageID]; ok {
				continue
			}
			seen[repo.StorageID] = struct{}{}
			refs = append(refs, &pb.WorkspaceRepositoryRef{
				StorageId:   repo.StorageID,
				DisplayPath: repo.DisplayPath,
				RepoUrl:     repo.RepoURL,
				Branch:      repo.Branch,
				BaseCommit:  repo.BaseCommit,
			})
		}
	}
	return refs
}

func (b *SourceCommitBundle) LocalCommitIDs() []string {
	if b == nil {
		return nil
	}
	ids := make([]string, 0, len(b.Commits))
	for _, commit := range b.Commits {
		if strings.TrimSpace(commit.ID) == "" {
			continue
		}
		ids = append(ids, commit.ID)
	}
	return ids
}

func (b *SourceCommitBundle) RepositoryByStorageID(storageID string) *SourceCommitRepository {
	if b == nil {
		return nil
	}
	for _, commit := range b.Commits {
		for i := range commit.Repositories {
			if commit.Repositories[i].StorageID == storageID {
				return &commit.Repositories[i]
			}
		}
	}
	return nil
}

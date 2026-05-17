package workspacebundle

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
)

const (
	OperationUpsert  = "upsert"
	OperationDelete  = "delete"
	OperationMkdir   = "mkdir"
	OperationRmdir   = "rmdir"
	OperationSymlink = "symlink"
)

type Bundle struct {
	WorkspaceID  string             `json:"workspace_id"`
	Repositories []RepositoryBundle `json:"repositories"`
}

type RepositoryBundle struct {
	StorageID   string      `json:"storage_id"`
	DisplayPath string      `json:"display_path"`
	RepoURL     string      `json:"repo_url"`
	Branch      string      `json:"branch"`
	BaseCommit  string      `json:"base_commit"`
	Operations  []Operation `json:"operations"`
}

type Operation struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Mode    int64  `json:"mode,omitempty"`
	Content []byte `json:"content,omitempty"`
	Target  string `json:"target,omitempty"`
}

func Parse(data []byte) (*Bundle, error) {
	var bundle Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("decode workspace bundle: %w", err)
	}
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	return &bundle, nil
}

func (b *Bundle) Validate() error {
	if b == nil {
		return fmt.Errorf("workspace bundle is required")
	}
	if strings.TrimSpace(b.WorkspaceID) == "" {
		return fmt.Errorf("workspace_id is required")
	}
	if len(b.Repositories) == 0 {
		return fmt.Errorf("workspace bundle must include at least one repository")
	}
	seen := make(map[string]struct{}, len(b.Repositories))
	for i := range b.Repositories {
		repo := b.Repositories[i]
		if strings.TrimSpace(repo.StorageID) == "" {
			return fmt.Errorf("repository %d storage_id is required", i)
		}
		if _, ok := seen[repo.StorageID]; ok {
			return fmt.Errorf("duplicate repository storage_id %q", repo.StorageID)
		}
		seen[repo.StorageID] = struct{}{}
		if strings.TrimSpace(repo.DisplayPath) == "" {
			return fmt.Errorf("repository %q display_path is required", repo.StorageID)
		}
		if strings.TrimSpace(repo.RepoURL) == "" {
			return fmt.Errorf("repository %q repo_url is required", repo.StorageID)
		}
		if strings.TrimSpace(repo.Branch) == "" {
			return fmt.Errorf("repository %q branch is required", repo.StorageID)
		}
		if strings.TrimSpace(repo.BaseCommit) == "" {
			return fmt.Errorf("repository %q base_commit is required", repo.StorageID)
		}
		for j := range repo.Operations {
			if err := validateOperation(repo.StorageID, j, repo.Operations[j]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Bundle) RepositoryRefs() []*pb.WorkspaceRepositoryRef {
	if b == nil {
		return nil
	}
	refs := make([]*pb.WorkspaceRepositoryRef, 0, len(b.Repositories))
	for _, repo := range b.Repositories {
		refs = append(refs, &pb.WorkspaceRepositoryRef{
			StorageId:   repo.StorageID,
			DisplayPath: repo.DisplayPath,
			RepoUrl:     repo.RepoURL,
			Branch:      repo.Branch,
			BaseCommit:  repo.BaseCommit,
		})
	}
	return refs
}

func (b *Bundle) RepositoryByStorageID(storageID string) *RepositoryBundle {
	if b == nil {
		return nil
	}
	for i := range b.Repositories {
		if b.Repositories[i].StorageID == storageID {
			return &b.Repositories[i]
		}
	}
	return nil
}

func validateOperation(storageID string, idx int, op Operation) error {
	switch op.Kind {
	case OperationUpsert, OperationDelete, OperationMkdir, OperationRmdir, OperationSymlink:
	default:
		return fmt.Errorf("repository %q operation %d has unsupported kind %q", storageID, idx, op.Kind)
	}
	if strings.TrimSpace(op.Path) == "" {
		return fmt.Errorf("repository %q operation %d path is required", storageID, idx)
	}
	if !isSafeRelativePath(op.Path) {
		return fmt.Errorf("repository %q operation %d path %q is invalid", storageID, idx, op.Path)
	}
	if op.Kind == OperationSymlink && strings.TrimSpace(op.Target) == "" {
		return fmt.Errorf("repository %q operation %d symlink target is required", storageID, idx)
	}
	return nil
}

func isSafeRelativePath(path string) bool {
	if path == "." || path == "" {
		return false
	}
	if filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".git" {
			return false
		}
	}
	return true
}

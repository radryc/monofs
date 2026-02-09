package git

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitpkg "github.com/radryc/monofs/internal/git"
	"github.com/radryc/monofs/internal/storage"
)

// GitIngestionBackend implements IngestionBackend for Git repositories
type GitIngestionBackend struct {
	repoMgr   *gitpkg.RepoManager
	repo      *git.Repository
	branch    string
	repoID    string
	sourceURL string
}

// NewGitIngestionBackend creates a new Git ingestion backend
func NewGitIngestionBackend() storage.IngestionBackend {
	return &GitIngestionBackend{}
}

func (g *GitIngestionBackend) Type() storage.IngestionType {
	return storage.IngestionTypeGit
}

func (g *GitIngestionBackend) Initialize(ctx context.Context, sourceURL string, config map[string]string) error {
	g.sourceURL = sourceURL
	g.branch = config["branch"]
	if g.branch == "" {
		g.branch = "main"
	}

	// Generate repo ID from URL
	g.repoID = config["display_path"]
	if g.repoID == "" {
		g.repoID = normalizeRepoID(sourceURL)
	}

	// Create temporary repo manager for ingestion
	tmpDir := "/tmp/monofs-ingestion-" + g.repoID
	repoMgr, err := gitpkg.NewRepoManager(tmpDir)
	if err != nil {
		return fmt.Errorf("failed to create repo manager: %w", err)
	}
	g.repoMgr = repoMgr

	// Clone or open repository
	repo, err := g.repoMgr.CloneOrOpen(ctx, sourceURL, g.repoID, g.branch)
	if err != nil {
		return fmt.Errorf("failed to clone/open repository: %w", err)
	}
	g.repo = repo

	// Extract commit information from the current branch
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(g.branch), true)
	if err == nil {
		commit, err := repo.CommitObject(ref.Hash())
		if err == nil {
			config["commit_hash"] = commit.Hash.String()
			config["commit_time"] = fmt.Sprintf("%d", commit.Committer.When.Unix())
			config["commit_message"] = commit.Message
		}
	}

	return nil
}

func (g *GitIngestionBackend) Validate(ctx context.Context, sourceURL string, config map[string]string) error {
	// Try to detect default branch
	tmpMgr, err := gitpkg.NewRepoManager("/tmp/monofs-validate")
	if err != nil {
		return err
	}
	defer tmpMgr.CleanupRepo("validate")

	branch, err := tmpMgr.GetDefaultBranch(ctx, sourceURL)
	if err != nil {
		return fmt.Errorf("failed to validate Git repository: %w", err)
	}

	if branch == "" {
		return fmt.Errorf("repository has no branches")
	}

	return nil
}

func (g *GitIngestionBackend) WalkFiles(ctx context.Context, fn func(storage.FileMetadata) error) error {
	if g.repo == nil {
		return fmt.Errorf("backend not initialized")
	}

	// Get commit information
	var commitHash, commitMessage string
	var commitTime int64
	ref, err := g.repo.Reference(plumbing.NewBranchReferenceName(g.branch), true)
	if err == nil {
		commit, err := g.repo.CommitObject(ref.Hash())
		if err == nil {
			commitHash = commit.Hash.String()
			commitTime = commit.Committer.When.Unix()
			commitMessage = commit.Message
		}
	}

	// Walk tree using the repo manager
	return g.repoMgr.WalkTree(g.repo, g.branch, func(gitMeta gitpkg.FileMetadata) error {
		meta := storage.FileMetadata{
			Path:        gitMeta.Path,
			Size:        gitMeta.Size,
			Mode:        gitMeta.Mode,
			ModTime:     gitMeta.Mtime,
			ContentHash: gitMeta.BlobHash,
			Metadata: map[string]string{
				"branch":         g.branch,
				"repo_url":       g.sourceURL,
				"commit_hash":    commitHash,
				"commit_time":    fmt.Sprintf("%d", commitTime),
				"commit_message": commitMessage,
			},
		}
		return fn(meta)
	})
}

func (g *GitIngestionBackend) GetMetadata(ctx context.Context, path string) (*storage.FileMetadata, error) {
	if g.repo == nil {
		return nil, fmt.Errorf("backend not initialized")
	}

	gitMeta, err := g.repoMgr.GetFileMetadata(g.repo, g.branch, path)
	if err != nil {
		return nil, err
	}

	return &storage.FileMetadata{
		Path:        gitMeta.Path,
		Size:        gitMeta.Size,
		Mode:        gitMeta.Mode,
		ModTime:     gitMeta.Mtime,
		ContentHash: gitMeta.BlobHash,
		Metadata: map[string]string{
			"branch":   g.branch,
			"repo_url": g.sourceURL,
		},
	}, nil
}

func (g *GitIngestionBackend) Cleanup() error {
	if g.repoMgr != nil {
		return g.repoMgr.CleanupRepo(g.repoID)
	}
	return nil
}

func normalizeRepoID(repoURL string) string {
	// Extract the repository name from URL
	// This is a simple implementation - might need more sophisticated logic
	return filepath.Base(repoURL)
}

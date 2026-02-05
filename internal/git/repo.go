// Package git handles Git repository operations for MonoFS.
package git

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// RepoManager handles Git repository operations.
type RepoManager struct {
	cacheDir string
}

// NewRepoManager creates a new repository manager.
func NewRepoManager(cacheDir string) (*RepoManager, error) {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache dir: %w", err)
	}

	return &RepoManager{
		cacheDir: cacheDir,
	}, nil
}

// OpenOnly opens an existing repository without cloning.
// Returns nil, ErrRepoNotCloned if the repository is not locally available.
// This is used for fast-path lookups where we don't want to block on network operations.
var ErrRepoNotCloned = fmt.Errorf("repository not cloned locally")

func (rm *RepoManager) OpenOnly(repoID string) (*git.Repository, error) {
	repoPath := filepath.Join(rm.cacheDir, repoID)
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return nil, ErrRepoNotCloned
	}
	return repo, nil
}

// CloneOrOpen clones a repository or opens existing one.
func (rm *RepoManager) CloneOrOpen(ctx context.Context, repoURL, repoID, branch string) (*git.Repository, error) {
	repoPath := filepath.Join(rm.cacheDir, repoID)

	// Try to open existing repo
	repo, err := git.PlainOpen(repoPath)
	if err == nil {
		// Fetch latest changes (shallow fetch to match clone depth)
		if err := repo.FetchContext(ctx, &git.FetchOptions{
			Depth: 1,
		}); err != nil && err != git.NoErrAlreadyUpToDate {
			// Non-fatal error, continue with existing data
		}
		return repo, nil
	}

	// Clone new repo
	if branch == "" {
		return nil, fmt.Errorf("branch must be specified")
	}

	repo, err = git.PlainCloneContext(ctx, repoPath, &git.CloneOptions{
		URL:               repoURL,
		ReferenceName:     plumbing.NewBranchReferenceName(branch),
		SingleBranch:      true,
		Depth:             1,
		Tags:              git.NoTags, // Don't download tags - huge performance improvement
		NoCheckout:        true,       // Skip working directory - we only need git objects for tree walk
		ShallowSubmodules: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to clone repo: %w", err)
	}

	return repo, nil
}

// GetDefaultBranch detects the default branch of a remote repository.
func (rm *RepoManager) GetDefaultBranch(ctx context.Context, repoURL string) (string, error) {
	// Create a remote to query the repository
	rem := git.NewRemote(nil, &config.RemoteConfig{
		Name: "origin",
		URLs: []string{repoURL},
	})

	// List references to find HEAD
	refs, err := rem.ListContext(ctx, &git.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to list remote refs: %w", err)
	}

	// Find the symbolic HEAD reference
	for _, ref := range refs {
		if ref.Name() == plumbing.HEAD {
			if ref.Target() != "" {
				// Symbolic ref - extract branch name
				targetRef := ref.Target().String()
				if strings.HasPrefix(targetRef, "refs/heads/") {
					return strings.TrimPrefix(targetRef, "refs/heads/"), nil
				}
			}
		}
	}

	// Fallback: try common default branches
	for _, branch := range []string{"main", "master"} {
		for _, ref := range refs {
			if ref.Name() == plumbing.NewBranchReferenceName(branch) {
				return branch, nil
			}
		}
	}

	return "", fmt.Errorf("could not determine default branch")
}

// FileMetadata represents file metadata from Git.
type FileMetadata struct {
	Path     string
	Size     uint64
	Mode     uint32
	BlobHash string
	Mtime    int64
}

// WalkTree walks the Git tree and yields file metadata.
// Walks git tree objects directly without requiring a working directory.
func (rm *RepoManager) WalkTree(repo *git.Repository, branch string, fn func(FileMetadata) error) error {
	if branch == "" {
		branch = "main"
	}

	// Try multiple reference formats in order of preference:
	// 1. Local branch ref (refs/heads/branch)
	// 2. Remote tracking ref (refs/remotes/origin/branch)
	// 3. HEAD (fallback)
	refNames := []plumbing.ReferenceName{
		plumbing.NewBranchReferenceName(branch),
		plumbing.NewRemoteReferenceName("origin", branch),
		plumbing.HEAD,
	}

	var ref *plumbing.Reference
	var err error
	for _, refName := range refNames {
		ref, err = repo.Reference(refName, true)
		if err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("failed to get branch ref (tried local, remote, HEAD): %w", err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return fmt.Errorf("failed to get commit: %w", err)
	}

	commitTime := commit.Committer.When.Unix()

	// Get tree and walk it directly (works with NoCheckout)
	tree, err := commit.Tree()
	if err != nil {
		return fmt.Errorf("failed to get tree: %w", err)
	}

	// Walk tree files directly
	return tree.Files().ForEach(func(f *object.File) error {
		mode, err := f.Mode.ToOSFileMode()
		if err != nil {
			mode = 0644 // Default to regular file
		}

		return fn(FileMetadata{
			Path:     f.Name,
			Size:     uint64(f.Size),
			Mode:     uint32(mode),
			BlobHash: f.Hash.String(),
			Mtime:    commitTime,
		})
	})
}

// ReadBlob reads a blob from the repository by hash.
func (rm *RepoManager) ReadBlob(repo *git.Repository, blobHash string) ([]byte, error) {
	hash := plumbing.NewHash(blobHash)
	blob, err := repo.BlobObject(hash)
	if err != nil {
		return nil, fmt.Errorf("failed to get blob: %w", err)
	}

	reader, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("failed to get blob reader: %w", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read blob: %w", err)
	}

	return data, nil
}

// CleanupRepo removes a cloned repository from cache to free disk space.
func (rm *RepoManager) CleanupRepo(repoID string) error {
	repoPath := filepath.Join(rm.cacheDir, repoID)
	return os.RemoveAll(repoPath)
}

// OpenOrClone tries to open an existing repo or clones it if not present.
func (rm *RepoManager) OpenOrClone(repoURL, branch string) (*git.Repository, error) {
	// Generate cache path from URL
	repoID := filepath.Base(repoURL)
	repoPath := filepath.Join(rm.cacheDir, repoID)

	// Try to open existing repo
	repo, err := git.PlainOpen(repoPath)
	if err == nil {
		return repo, nil
	}

	// Clone new repo
	if branch == "" {
		branch = "main"
	}

	repo, err = git.PlainClone(repoPath, &git.CloneOptions{
		URL:           repoURL,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
		Depth:         1,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to clone repo: %w", err)
	}

	return repo, nil
}

// GetFileMetadata retrieves metadata for a specific file in the repository.
func (rm *RepoManager) GetFileMetadata(repo *git.Repository, branch, filePath string) (FileMetadata, error) {
	if branch == "" {
		branch = "main"
	}

	// Try multiple reference formats in order of preference:
	// 1. Local branch ref (refs/heads/branch)
	// 2. Remote tracking ref (refs/remotes/origin/branch)
	// 3. HEAD (fallback)
	refNames := []plumbing.ReferenceName{
		plumbing.NewBranchReferenceName(branch),
		plumbing.NewRemoteReferenceName("origin", branch),
		plumbing.HEAD,
	}

	var ref *plumbing.Reference
	var err error
	for _, refName := range refNames {
		ref, err = repo.Reference(refName, true)
		if err == nil {
			break
		}
	}
	if err != nil {
		return FileMetadata{}, fmt.Errorf("failed to get branch ref (tried local, remote, HEAD): %w", err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return FileMetadata{}, fmt.Errorf("failed to get commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return FileMetadata{}, fmt.Errorf("failed to get tree: %w", err)
	}

	file, err := tree.File(filePath)
	if err != nil {
		return FileMetadata{}, fmt.Errorf("failed to get file: %w", err)
	}

	mode, err := file.Mode.ToOSFileMode()
	if err != nil {
		return FileMetadata{}, err
	}

	return FileMetadata{
		Path:     file.Name,
		Size:     uint64(file.Size),
		Mode:     uint32(mode),
		BlobHash: file.Hash.String(),
		Mtime:    commit.Committer.When.Unix(),
	}, nil
}

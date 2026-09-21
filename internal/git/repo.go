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
	return rm.CloneOrOpenRef(ctx, repoURL, repoID, branch, false)
}

// CloneOrOpenRef clones a repository at a branch, tag, or commit SHA. When
// fetchTags is true (tag/SHA refs), tags are fetched rather than skipped so the
// target object is available for tree walking.
//
// Resolution for a bare name ref: the branch is tried first (the common case,
// including the default branch), and when that fails the name is tried as a
// tag. A name that matches both a branch and a tag resolves as the branch.
func (rm *RepoManager) CloneOrOpenRef(ctx context.Context, repoURL, repoID, ref string, fetchTags bool) (*git.Repository, error) {
	repoPath := filepath.Join(rm.cacheDir, repoID)

	// Try to open existing repo
	repo, err := git.PlainOpen(repoPath)
	if err == nil {
		// Refresh objects; when a tag/SHA was requested, fetch tags so the
		// ref can be resolved if it wasn't present before.
		fetchOpts := &git.FetchOptions{Depth: 1}
		if fetchTags {
			fetchOpts.Tags = git.AllTags
		}
		if err := repo.FetchContext(ctx, fetchOpts); err != nil && err != git.NoErrAlreadyUpToDate {
			// Non-fatal error, continue with existing data
		}
		return repo, nil
	}

	if ref == "" {
		return nil, fmt.Errorf("branch must be specified")
	}

	switch kind := ClassifyRef(ref); kind {
	case RefTag:
		return rm.cloneTagRef(ctx, repoPath, repoURL, TrimRefPrefix(ref))
	case RefSHA:
		return rm.cloneSHARef(ctx, repoPath, repoURL, TrimRefPrefix(ref))
	default:
		repo, err := rm.cloneBranchRef(ctx, repoPath, repoURL, ref)
		if err == nil {
			return repo, nil
		}
		// A bare name might be a tag; try that before giving up.
		_ = os.RemoveAll(repoPath)
		if tagRepo, tagErr := rm.cloneTagRef(ctx, repoPath, repoURL, ref); tagErr == nil {
			return tagRepo, nil
		}
		return nil, err
	}
}

// cloneBranchRef shallow-clones a single branch (the default ingest path).
func (rm *RepoManager) cloneBranchRef(ctx context.Context, repoPath, repoURL, branch string) (*git.Repository, error) {
	repo, err := git.PlainCloneContext(ctx, repoPath, &git.CloneOptions{
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

// cloneTagRef shallow-clones a specific tag.
func (rm *RepoManager) cloneTagRef(ctx context.Context, repoPath, repoURL, tag string) (*git.Repository, error) {
	repo, err := git.PlainCloneContext(ctx, repoPath, &git.CloneOptions{
		URL:               repoURL,
		ReferenceName:     plumbing.NewTagReferenceName(tag),
		SingleBranch:      true,
		Depth:             1,
		Tags:              git.TagFollowing,
		NoCheckout:        true,
		ShallowSubmodules: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to clone repo tag %q: %w", tag, err)
	}
	return repo, nil
}

// CloneOrOpenHistory clones (with full history and all tags) or opens an
// existing repo and refreshes it with full history, for read-side operations
// like upstream log, tag listing, and blame that need the complete commit graph.
func (rm *RepoManager) CloneOrOpenHistory(ctx context.Context, repoURL, repoID, ref string) (*git.Repository, error) {
	repoPath := filepath.Join(rm.cacheDir, repoID)

	if repo, err := git.PlainOpen(repoPath); err == nil {
		_ = repo.FetchContext(ctx, &git.FetchOptions{
			Depth: 0,
			Tags:  git.AllTags,
		})
		return repo, nil
	}

	if ref == "" {
		ref = "main"
	}
	referenceName := plumbing.ReferenceName(plumbing.HEAD)
	switch ClassifyRef(ref) {
	case RefTag:
		referenceName = plumbing.NewTagReferenceName(TrimRefPrefix(ref))
	case RefSHA:
		referenceName = plumbing.HEAD
	default:
		referenceName = plumbing.NewBranchReferenceName(ref)
	}

	repo, err := git.PlainCloneContext(ctx, repoPath, &git.CloneOptions{
		URL:               repoURL,
		ReferenceName:     referenceName,
		SingleBranch:      referenceName != plumbing.HEAD && ClassifyRef(ref) != RefSHA,
		Depth:             0, // full history
		Tags:              git.AllTags,
		NoCheckout:        true,
		ShallowSubmodules: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to clone repo history: %w", err)
	}
	return repo, nil
}

// cloneSHARef clones the full repository (all branches + tags) so an arbitrary
// commit SHA is reachable, then verifies the requested commit exists.
func (rm *RepoManager) cloneSHARef(ctx context.Context, repoPath, repoURL, sha string) (*git.Repository, error) {
	repo, err := git.PlainCloneContext(ctx, repoPath, &git.CloneOptions{
		URL:               repoURL,
		SingleBranch:      false,
		Depth:             0, // full history so arbitrary SHAs are reachable
		Tags:              git.AllTags,
		NoCheckout:        true,
		ShallowSubmodules: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to clone repo for SHA: %w", err)
	}
	if _, err := repo.CommitObject(plumbing.NewHash(sha)); err != nil {
		return nil, fmt.Errorf("commit SHA %q not found in repository: %w", sha, err)
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

// resolveReference attempts to resolve a git reference using multiple strategies.
// Tries: branch name, origin/branch name, tag name, HEAD, and finally a raw
// commit SHA (for tag/SHA-pinned ingests).
func resolveReference(repo *git.Repository, branch string) (*plumbing.Reference, error) {
	// A raw commit SHA resolves directly to itself when the object is present.
	if IsHexSHA(branch) {
		hash := plumbing.NewHash(branch)
		if _, err := repo.CommitObject(hash); err != nil {
			return nil, fmt.Errorf("failed to resolve commit SHA %q: %w", branch, err)
		}
		return plumbing.NewHashReference(plumbing.ReferenceName("refs/monofs/sha"), hash), nil
	}

	refNames := []plumbing.ReferenceName{
		plumbing.NewBranchReferenceName(branch),
		plumbing.NewRemoteReferenceName("origin", branch),
		plumbing.NewTagReferenceName(branch),
		plumbing.HEAD,
	}

	var ref *plumbing.Reference
	var err error
	for _, refName := range refNames {
		ref, err = repo.Reference(refName, true)
		if err == nil {
			return ref, nil
		}
	}
	return nil, fmt.Errorf("failed to get branch ref (tried local, remote, tag, HEAD): %w", err)
}

// resolveReferenceWithFallback tries the specified branch, then falls back to main/master
func resolveReferenceWithFallback(repo *git.Repository, branch string) (*plumbing.Reference, error) {
	// Try the specified branch first
	if branch != "" {
		ref, err := resolveReference(repo, branch)
		if err == nil {
			return ref, nil
		}
	}

	// Fallback to common default branches
	for _, fallbackBranch := range []string{"main", "master"} {
		ref, err := resolveReference(repo, fallbackBranch)
		if err == nil {
			return ref, nil
		}
	}

	return nil, fmt.Errorf("failed to resolve reference (tried %q, main, master)", branch)
}

// ResolveCommit resolves a branch, tag, or commit SHA ref to its commit hash,
// peeling annotated tags down to their target commit.
func (rm *RepoManager) ResolveCommit(repo *git.Repository, ref string) (plumbing.Hash, error) {
	r, err := resolveReference(repo, ref)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return peelToCommit(repo, r.Hash())
}

// peelToCommit dereferences annotated tags to their target commit. Non-tag
// objects are returned unchanged.
func peelToCommit(repo *git.Repository, hash plumbing.Hash) (plumbing.Hash, error) {
	for i := 0; i < 10; i++ { // bounded to avoid pathological tag chains
		tag, err := repo.TagObject(hash)
		if err != nil {
			// No tag object at this hash (or hash is a commit) - done.
			return hash, nil
		}
		if tag.TargetType == plumbing.CommitObject || tag.TargetType == plumbing.TagObject {
			hash = tag.Target
			continue
		}
		return hash, nil
	}
	return hash, fmt.Errorf("tag chain too deep while resolving %s", hash)
}

// WalkTree walks the Git tree and yields file metadata.
// Walks git tree objects directly without requiring a working directory.
func (rm *RepoManager) WalkTree(repo *git.Repository, branch string, fn func(FileMetadata) error) error {
	if branch == "" {
		branch = "main"
	}

	commitHash, err := rm.ResolveCommit(repo, branch)
	if err != nil {
		return err
	}

	commit, err := repo.CommitObject(commitHash)
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

	commitHash, err := rm.ResolveCommit(repo, branch)
	if err != nil {
		return FileMetadata{}, err
	}

	commit, err := repo.CommitObject(commitHash)
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

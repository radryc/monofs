package file

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/go-git/go-git/v6"

	gitpkg "github.com/radryc/monofs/internal/git"
	"github.com/radryc/monofs/internal/storage"
)

type FileIngestionBackend struct {
	repoMgr      *gitpkg.RepoManager
	repo         *git.Repository
	branch       string
	repoID       string
	sourceDir    string
	isGitRepo    bool
	plainDirHash string
}

func NewFileIngestionBackend() storage.IngestionBackend {
	return &FileIngestionBackend{}
}

func (f *FileIngestionBackend) Type() storage.IngestionType {
	return storage.IngestionTypeFile
}

func (f *FileIngestionBackend) Validate(ctx context.Context, sourceURL string, config map[string]string) error {
	info, err := os.Stat(sourceURL)
	if err != nil {
		return fmt.Errorf("cannot access path %q: %w", sourceURL, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("path %q is not a directory", sourceURL)
	}
	return nil
}

func (f *FileIngestionBackend) Initialize(ctx context.Context, sourceURL string, config map[string]string) error {
	f.sourceDir = sourceURL

	f.repoID = config["repo_id"]
	if f.repoID == "" {
		f.repoID = config["display_path"]
	}
	if f.repoID == "" {
		f.repoID = filepath.Base(sourceURL)
	}

	gitDir := filepath.Join(sourceURL, ".git")
	gitInfo, err := os.Stat(gitDir)
	if err == nil && gitInfo.IsDir() {
		f.isGitRepo = true
		return f.initializeGit(ctx, config)
	}

	f.isGitRepo = false
	return f.initializePlain(config)
}

func (f *FileIngestionBackend) initializeGit(ctx context.Context, config map[string]string) error {
	repo, err := git.PlainOpen(f.sourceDir)
	if err != nil {
		return fmt.Errorf("failed to open local git repo at %q: %w", f.sourceDir, err)
	}
	f.repo = repo

	headRef, err := repo.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD ref: %w", err)
	}
	f.branch = headRef.Name().Short()

	repoMgr, err := gitpkg.NewRepoManager("/tmp/monofs-file-ingestion-" + f.repoID)
	if err != nil {
		return fmt.Errorf("failed to create repo manager: %w", err)
	}
	f.repoMgr = repoMgr

	commit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return fmt.Errorf("failed to get commit: %w", err)
	}
	config["commit_hash"] = commit.Hash.String()
	config["commit_time"] = fmt.Sprintf("%d", commit.Committer.When.Unix())
	config["commit_message"] = commit.Message

	return nil
}

func (f *FileIngestionBackend) initializePlain(config map[string]string) error {
	f.branch = "default"

	dirHash := sha256.Sum256([]byte(f.sourceDir))
	f.plainDirHash = fmt.Sprintf("local-%x", dirHash[:8])
	config["commit_hash"] = f.plainDirHash
	config["commit_time"] = fmt.Sprintf("%d", time.Now().Unix())
	config["commit_message"] = "Local directory upload"

	return nil
}

func (f *FileIngestionBackend) WalkFiles(ctx context.Context, fn func(storage.FileMetadata) error) error {
	if f.isGitRepo {
		return f.walkGit(ctx, fn)
	}
	return f.walkPlain(ctx, fn)
}

func (f *FileIngestionBackend) walkGit(ctx context.Context, fn func(storage.FileMetadata) error) error {
	var commitHash, commitMessage string
	var commitTime int64

	ref, err := f.repo.Head()
	if err == nil {
		commit, err := f.repo.CommitObject(ref.Hash())
		if err == nil {
			commitHash = commit.Hash.String()
			commitTime = commit.Committer.When.Unix()
			commitMessage = commit.Message
		}
	}

	return f.repoMgr.WalkTree(f.repo, f.branch, func(gitMeta gitpkg.FileMetadata) error {
		content, err := f.repoMgr.ReadBlob(f.repo, gitMeta.BlobHash)
		if err != nil {
			return fmt.Errorf("failed to read blob %s for %s: %w", gitMeta.BlobHash, gitMeta.Path, err)
		}

		meta := storage.FileMetadata{
			Path:        gitMeta.Path,
			Size:        gitMeta.Size,
			Mode:        gitMeta.Mode,
			ModTime:     gitMeta.Mtime,
			ContentHash: gitMeta.BlobHash,
			Content:     content,
			Metadata: map[string]string{
				"branch":         f.branch,
				"repo_url":       f.sourceDir,
				"commit_hash":    commitHash,
				"commit_time":    fmt.Sprintf("%d", commitTime),
				"commit_message": commitMessage,
			},
		}
		return fn(meta)
	})
}

func (f *FileIngestionBackend) walkPlain(ctx context.Context, fn func(storage.FileMetadata) error) error {
	return filepath.WalkDir(f.sourceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		mode := info.Mode()
		if mode&os.ModeSymlink != 0 {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read file %q: %w", path, err)
		}

		contentHash := sha256.Sum256(content)
		relPath, err := filepath.Rel(f.sourceDir, path)
		if err != nil {
			return fmt.Errorf("failed to compute relative path: %w", err)
		}

		meta := storage.FileMetadata{
			Path:        relPath,
			Size:        uint64(info.Size()),
			Mode:        uint32(mode),
			ModTime:     info.ModTime().Unix(),
			ContentHash: fmt.Sprintf("%x", contentHash),
			Content:     content,
			Metadata: map[string]string{
				"branch":         "default",
				"repo_url":       f.sourceDir,
				"commit_hash":    f.plainDirHash,
				"commit_time":    fmt.Sprintf("%d", time.Now().Unix()),
				"commit_message": "Local directory upload",
			},
		}
		return fn(meta)
	})
}

func (f *FileIngestionBackend) GetMetadata(ctx context.Context, path string) (*storage.FileMetadata, error) {
	if f.isGitRepo {
		return f.getMetadataGit(ctx, path)
	}
	return f.getMetadataPlain(path)
}

func (f *FileIngestionBackend) getMetadataGit(ctx context.Context, filePath string) (*storage.FileMetadata, error) {
	gitMeta, err := f.repoMgr.GetFileMetadata(f.repo, f.branch, filePath)
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
			"branch":   f.branch,
			"repo_url": f.sourceDir,
		},
	}, nil
}

func (f *FileIngestionBackend) getMetadataPlain(filePath string) (*storage.FileMetadata, error) {
	fullPath := filepath.Join(f.sourceDir, filePath)
	info, err := os.Stat(fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat %q: %w", fullPath, err)
	}
	return &storage.FileMetadata{
		Path:    filePath,
		Size:    uint64(info.Size()),
		Mode:    uint32(info.Mode()),
		ModTime: info.ModTime().Unix(),
		Metadata: map[string]string{
			"branch":   "default",
			"repo_url": f.sourceDir,
		},
	}, nil
}

func (f *FileIngestionBackend) Cleanup() error {
	if f.repoMgr != nil {
		return f.repoMgr.CleanupRepo(f.repoID)
	}
	return nil
}

var _ storage.IngestionBackend = (*FileIngestionBackend)(nil)

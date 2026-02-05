package git

import (
	"bytes"
	"context"
	"fmt"
	"io"

	gitpkg "github.com/radryc/monofs/internal/git"
	"github.com/radryc/monofs/internal/storage"
)

// GitFetchBackend implements FetchBackend for Git repositories
type GitFetchBackend struct {
	repoMgr   *gitpkg.RepoManager
	blobCache *gitpkg.BlobCache
	cacheDir  string
}

// NewGitFetchBackend creates a new Git fetch backend
func NewGitFetchBackend() storage.FetchBackend {
	return &GitFetchBackend{}
}

func (g *GitFetchBackend) Type() storage.FetchType {
	return storage.FetchTypeGit
}

func (g *GitFetchBackend) Initialize(ctx context.Context, config map[string]string) error {
	cacheDir := config["cache_dir"]
	if cacheDir == "" {
		return fmt.Errorf("cache_dir config required")
	}
	g.cacheDir = cacheDir

	repoMgr, err := gitpkg.NewRepoManager(cacheDir)
	if err != nil {
		return fmt.Errorf("failed to create repo manager: %w", err)
	}
	g.repoMgr = repoMgr

	// Create blob cache with default config
	blobCacheCfg := gitpkg.DefaultBlobCacheConfig()
	if blobCacheDir := config["blob_cache_dir"]; blobCacheDir != "" {
		blobCacheCfg.CacheDir = blobCacheDir
	}

	blobCache, err := gitpkg.NewBlobCache(repoMgr, blobCacheCfg)
	if err != nil {
		return fmt.Errorf("failed to create blob cache: %w", err)
	}
	g.blobCache = blobCache

	return nil
}

func (g *GitFetchBackend) FetchBlob(ctx context.Context, blobID string) ([]byte, error) {
	if g.blobCache == nil {
		return nil, fmt.Errorf("backend not initialized")
	}

	// Use the blob cache's ReadBlob which handles restoration automatically
	// For now, we pass nil for the metadata since the cache will handle it
	return g.blobCache.ReadBlob(ctx, nil, blobID)
}

func (g *GitFetchBackend) FetchBlobStream(ctx context.Context, blobID string) (io.ReadCloser, error) {
	// For Git, we read the whole blob into memory
	// Future: implement streaming for large files if needed
	data, err := g.FetchBlob(ctx, blobID)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (g *GitFetchBackend) StoreBlob(ctx context.Context, data []byte) (string, error) {
	// Git doesn't support storing arbitrary blobs through this interface
	// This is for future S3/local storage backends
	return "", fmt.Errorf("StoreBlob not supported for Git backend")
}

func (g *GitFetchBackend) StoreBlobStream(ctx context.Context, reader io.Reader) (string, error) {
	// Git doesn't support storing arbitrary blobs through this interface
	return "", fmt.Errorf("StoreBlobStream not supported for Git backend")
}

func (g *GitFetchBackend) Cleanup() error {
	if g.blobCache != nil {
		return g.blobCache.Close()
	}
	return nil
}

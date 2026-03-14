package git

import (
	"bytes"
	"context"
	"fmt"
	"io"

	gitpkg "github.com/radryc/monofs/internal/git"
	"github.com/radryc/monofs/internal/storage"
)

// GitFetchBackend implements FetchBackend for Git repositories.
// Used by the router for optional direct Git blob access.
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

func (g *GitFetchBackend) Initialize(ctx context.Context, config storage.BackendConfig) error {
	cacheDir := config.CacheDir
	if cacheDir == "" {
		if config.Extra != nil {
			cacheDir = config.Extra["cache_dir"]
		}
		if cacheDir == "" {
			return fmt.Errorf("cache_dir config required")
		}
	}
	g.cacheDir = cacheDir

	repoMgr, err := gitpkg.NewRepoManager(cacheDir)
	if err != nil {
		return fmt.Errorf("failed to create repo manager: %w", err)
	}
	g.repoMgr = repoMgr

	// Create blob cache with default config
	blobCacheCfg := gitpkg.DefaultBlobCacheConfig()
	if config.Extra != nil {
		if blobCacheDir := config.Extra["blob_cache_dir"]; blobCacheDir != "" {
			blobCacheCfg.CacheDir = blobCacheDir
		}
	}

	blobCache, err := gitpkg.NewBlobCache(repoMgr, blobCacheCfg)
	if err != nil {
		return fmt.Errorf("failed to create blob cache: %w", err)
	}
	g.blobCache = blobCache

	return nil
}

func (g *GitFetchBackend) FetchBlob(ctx context.Context, req *storage.FetchRequest) (*storage.FetchResult, error) {
	if g.blobCache == nil {
		return nil, fmt.Errorf("backend not initialized")
	}

	data, err := g.blobCache.ReadBlob(ctx, nil, req.ContentID)
	if err != nil {
		return nil, err
	}

	return &storage.FetchResult{
		Content:   data,
		Size:      int64(len(data)),
		FromCache: true,
	}, nil
}

func (g *GitFetchBackend) FetchBlobStream(ctx context.Context, req *storage.FetchRequest) (io.ReadCloser, int64, error) {
	result, err := g.FetchBlob(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(result.Content)), result.Size, nil
}

func (g *GitFetchBackend) Warmup(ctx context.Context, sourceKey string, config map[string]string) error {
	return nil
}

func (g *GitFetchBackend) CachedSources() []string {
	return nil
}

func (g *GitFetchBackend) Cleanup(ctx context.Context, sourceKey string) error {
	return nil
}

func (g *GitFetchBackend) Close() error {
	if g.blobCache != nil {
		return g.blobCache.Close()
	}
	return nil
}

func (g *GitFetchBackend) Stats() storage.BackendStats {
	return storage.BackendStats{}
}

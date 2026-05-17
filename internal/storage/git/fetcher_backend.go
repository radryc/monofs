package git

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/radryc/monofs/internal/storage"
)

// GitBackend fetches blobs from Git repositories.
type GitBackend struct {
	config storage.BackendConfig

	mu    sync.RWMutex
	repos map[string]*cachedRepo // repoURL -> cached repo

	stats *storage.AtomicStats
}

type cachedRepo struct {
	repo       *gogit.Repository
	repoPath   string
	lastAccess time.Time
	mu         sync.Mutex // Protects fetch operations
}

// NewGitBackend creates a new Git backend.
func NewGitBackend() *GitBackend {
	gb := &GitBackend{
		repos: make(map[string]*cachedRepo),
		stats: storage.NewAtomicStats(),
	}
	return gb
}

func (gb *GitBackend) Type() storage.FetchType {
	return storage.FetchTypeGit
}

func (gb *GitBackend) Initialize(ctx context.Context, cfg storage.BackendConfig) error {
	gb.config = cfg

	// Ensure cache directory exists
	if err := os.MkdirAll(cfg.CacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create git cache dir: %w", err)
	}

	// Scan existing repos in cache dir
	entries, err := os.ReadDir(cfg.CacheDir)
	if err != nil {
		return nil // Empty cache is fine
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		repoPath := filepath.Join(cfg.CacheDir, entry.Name())
		repo, err := gogit.PlainOpen(repoPath)
		if err != nil {
			continue // Invalid repo, skip
		}
		// Extract repo URL from config
		repoCfg, err := repo.Config()
		if err != nil {
			continue
		}
		if remote, ok := repoCfg.Remotes["origin"]; ok && len(remote.URLs) > 0 {
			gb.repos[remote.URLs[0]] = &cachedRepo{
				repo:       repo,
				repoPath:   repoPath,
				lastAccess: time.Now(),
			}
		}
	}

	// Start cache cleanup goroutine
	go gb.cleanupLoop(ctx)

	return nil
}

func (gb *GitBackend) FetchBlob(ctx context.Context, req *storage.FetchRequest) (*storage.FetchResult, error) {
	start := time.Now()

	repoURL := req.SourceConfig["repo_url"]
	branch := req.SourceConfig["branch"]
	if branch == "" {
		branch = "main"
	}

	// Get or clone repo
	cached, err := gb.getOrCloneRepo(ctx, repoURL, branch)
	if err != nil {
		gb.stats.RecordError()
		return nil, fmt.Errorf("failed to get repo: %w", err)
	}

	// Read blob
	cached.mu.Lock()
	defer cached.mu.Unlock()
	cached.lastAccess = time.Now()

	blobHash := plumbing.NewHash(req.ContentID)
	blob, err := cached.repo.BlobObject(blobHash)
	if err != nil {
		// Try fetching latest
		if fetchErr := gb.fetchLatest(ctx, cached, branch); fetchErr == nil {
			blob, err = cached.repo.BlobObject(blobHash)
		}
		if err != nil {
			gb.stats.RecordError()
			return nil, fmt.Errorf("blob not found: %s: %w", req.ContentID, err)
		}
	}

	reader, err := blob.Reader()
	if err != nil {
		gb.stats.RecordError()
		return nil, fmt.Errorf("failed to read blob: %w", err)
	}
	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		gb.stats.RecordError()
		return nil, fmt.Errorf("failed to read blob content: %w", err)
	}

	latency := time.Since(start)
	gb.stats.RecordSuccess(latency, int64(len(content)))

	return &storage.FetchResult{
		Content:        content,
		Size:           int64(len(content)),
		FromCache:      false,
		FetchLatencyMs: latency.Milliseconds(),
	}, nil
}

func (gb *GitBackend) FetchBlobStream(ctx context.Context, req *storage.FetchRequest) (io.ReadCloser, int64, error) {
	repoURL := req.SourceConfig["repo_url"]
	branch := req.SourceConfig["branch"]
	if branch == "" {
		branch = "main"
	}

	cached, err := gb.getOrCloneRepo(ctx, repoURL, branch)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get repo: %w", err)
	}

	cached.mu.Lock()
	defer cached.mu.Unlock()
	cached.lastAccess = time.Now()

	blobHash := plumbing.NewHash(req.ContentID)
	blob, err := cached.repo.BlobObject(blobHash)
	if err != nil {
		if fetchErr := gb.fetchLatest(ctx, cached, branch); fetchErr == nil {
			blob, err = cached.repo.BlobObject(blobHash)
		}
		if err != nil {
			return nil, 0, fmt.Errorf("blob not found: %s: %w", req.ContentID, err)
		}
	}

	reader, err := blob.Reader()
	if err != nil {
		return nil, 0, fmt.Errorf("failed to read blob: %w", err)
	}

	return reader, blob.Size, nil
}

func (gb *GitBackend) Warmup(ctx context.Context, sourceKey string, config map[string]string) error {
	branch := config["branch"]
	if branch == "" {
		branch = "main"
	}

	_, err := gb.getOrCloneRepo(ctx, sourceKey, branch)
	return err
}

func (gb *GitBackend) CachedSources() []string {
	gb.mu.RLock()
	defer gb.mu.RUnlock()

	sources := make([]string, 0, len(gb.repos))
	for url := range gb.repos {
		sources = append(sources, url)
	}
	return sources
}

func (gb *GitBackend) Cleanup(ctx context.Context, sourceKey string) error {
	gb.mu.Lock()
	defer gb.mu.Unlock()

	cached, ok := gb.repos[sourceKey]
	if !ok {
		return nil
	}

	delete(gb.repos, sourceKey)

	// Remove from disk
	if cached.repoPath != "" {
		os.RemoveAll(cached.repoPath)
	}

	return nil
}

func (gb *GitBackend) Close() error {
	gb.mu.Lock()
	defer gb.mu.Unlock()

	// Clear all references (don't delete files, they can be reused on restart)
	gb.repos = make(map[string]*cachedRepo)
	return nil
}

func (gb *GitBackend) Stats() storage.BackendStats {
	return gb.stats.GetStats()
}

// getOrCloneRepo returns a cached repo or clones it.
func (gb *GitBackend) getOrCloneRepo(ctx context.Context, repoURL, branch string) (*cachedRepo, error) {
	gb.mu.RLock()
	cached, ok := gb.repos[repoURL]
	gb.mu.RUnlock()

	if ok {
		return cached, nil
	}

	gb.mu.Lock()
	defer gb.mu.Unlock()

	// Double-check after acquiring write lock
	if cached, ok = gb.repos[repoURL]; ok {
		return cached, nil
	}

	// Clone the repo
	repoPath := filepath.Join(gb.config.CacheDir, hashString(repoURL))

	// Try to open existing
	repo, err := gogit.PlainOpen(repoPath)
	if err != nil {
		// Clone new (shallow, no checkout)
		repo, err = gogit.PlainCloneContext(ctx, repoPath, false, &gogit.CloneOptions{
			URL:           repoURL,
			ReferenceName: plumbing.NewBranchReferenceName(branch),
			SingleBranch:  true,
			Depth:         1,
			Tags:          gogit.NoTags,
			NoCheckout:    true,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to clone repo %s: %w", repoURL, err)
		}
	}

	cached = &cachedRepo{
		repo:       repo,
		repoPath:   repoPath,
		lastAccess: time.Now(),
	}
	gb.repos[repoURL] = cached

	return cached, nil
}

func (gb *GitBackend) fetchLatest(ctx context.Context, cached *cachedRepo, branch string) error {
	err := cached.repo.FetchContext(ctx, &gogit.FetchOptions{
		Depth: 1,
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/heads/%s", branch, branch)),
		},
	})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		return err
	}
	return nil
}

func (gb *GitBackend) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	maxAge := time.Duration(gb.config.MaxCacheAgeSecs) * time.Second
	if maxAge == 0 {
		maxAge = 1 * time.Hour
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			gb.cleanupOldRepos(maxAge)
		}
	}
}

func (gb *GitBackend) cleanupOldRepos(maxAge time.Duration) {
	gb.mu.Lock()
	defer gb.mu.Unlock()

	now := time.Now()
	for url, cached := range gb.repos {
		if now.Sub(cached.lastAccess) > maxAge {
			delete(gb.repos, url)
			if cached.repoPath != "" {
				os.RemoveAll(cached.repoPath)
			}
		}
	}
}

func hashString(s string) string {
	// Simple hash for directory name
	h := uint64(0)
	for _, c := range s {
		h = h*31 + uint64(c)
	}
	return fmt.Sprintf("%016x", h)
}

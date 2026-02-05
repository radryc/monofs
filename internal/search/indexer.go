// Package search provides code search functionality using Zoekt.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"strings"
	"sync"
	"time"

	grafanaregexp "github.com/grafana/regexp"
	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
	"github.com/sourcegraph/zoekt/search"

	"github.com/radryc/monofs/internal/client"
)

// Indexer manages Zoekt indexing and searching
type Indexer struct {
	mu              sync.RWMutex
	indexDir        string
	cacheDir        string
	searcher        zoekt.Streamer
	logger          *slog.Logger
	pathToStorageID map[string]string   // DisplayPath -> StorageID mapping
	monofsClient    client.MonoFSClient // Optional client for fetching from storage nodes
}

// IndexRequest contains repository indexing parameters
type IndexRequest struct {
	StorageID   string
	DisplayPath string
	RepoURL     string
	Ref         string
}

// IndexResult contains indexing results
type IndexResult struct {
	FilesIndexed   int64
	IndexSizeBytes int64
	Duration       time.Duration
}

// SearchRequest contains search parameters
type SearchRequest struct {
	Query         string
	StorageID     string // Optional: limit to specific repo
	MaxResults    int
	CaseSensitive bool
	Regex         bool
	FilePatterns  []string
}

// SearchResult contains a single search match
type SearchResult struct {
	StorageID     string
	DisplayPath   string
	FilePath      string
	LineNumber    int
	LineContent   string
	Matches       []MatchRange
	BeforeContext string
	AfterContext  string
}

// MatchRange indicates match position
type MatchRange struct {
	Start int
	End   int
}

// SearchResults contains all search results
type SearchResults struct {
	Results       []SearchResult
	TotalMatches  int64
	FilesSearched int64
	Duration      time.Duration
	Truncated     bool
}

// globToRegex converts a glob pattern to a regex pattern
// e.g., "*.go" -> ".*\.go$", "test_*.py" -> "test_.*\.py$"
func globToRegex(glob string) string {
	// If it looks like a simple extension filter (e.g., ".go"), match files ending with it
	if strings.HasPrefix(glob, ".") && !strings.Contains(glob, "*") {
		return ".*" + regexp.QuoteMeta(glob) + "$"
	}

	// Convert glob wildcards to regex
	var result strings.Builder
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				// ** matches everything including path separators
				result.WriteString(".*")
				i++ // skip next *
			} else {
				// * matches anything except path separators
				result.WriteString("[^/]*")
			}
		case '?':
			result.WriteString("[^/]")
		case '.', '+', '^', '$', '(', ')', '[', ']', '{', '}', '|', '\\':
			// Escape regex metacharacters
			result.WriteByte('\\')
			result.WriteByte(c)
		default:
			result.WriteByte(c)
		}
	}
	// Anchor to end of filename (match the filename part)
	return result.String() + "$"
}

// NewIndexer creates a new Zoekt indexer
func NewIndexer(indexDir, cacheDir string, monofsClient client.MonoFSClient, logger *slog.Logger) (*Indexer, error) {
	if err := os.MkdirAll(indexDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create index dir: %w", err)
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache dir: %w", err)
	}

	// Create the searcher that reads from the index directory
	searcher, err := search.NewDirectorySearcher(indexDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create searcher: %w", err)
	}

	return &Indexer{
		indexDir:        indexDir,
		cacheDir:        cacheDir,
		searcher:        searcher,
		logger:          logger,
		pathToStorageID: make(map[string]string),
		monofsClient:    monofsClient,
	}, nil
}

// RegisterStorageMapping registers a DisplayPath to StorageID mapping.
// This allows the Search function to return StorageID in results.
// Call this for all known repositories on startup.
func (i *Indexer) RegisterStorageMapping(displayPath, storageID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.pathToStorageID[displayPath] = storageID
}

// Close closes the indexer
func (i *Indexer) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.searcher != nil {
		i.searcher.Close()
	}
	return nil
}

// IndexRepository indexes a git repository
func (i *Indexer) IndexRepository(ctx context.Context, req IndexRequest) (*IndexResult, error) {
	start := time.Now()

	// Store the mapping from DisplayPath to StorageID
	i.mu.Lock()
	i.pathToStorageID[req.DisplayPath] = req.StorageID
	i.mu.Unlock()

	i.logger.Info("preparing repository for indexing",
		"storage_id", req.StorageID,
		"source", req.RepoURL,
		"ref", req.Ref,
		"has_client", i.monofsClient != nil)

	// If we have a monofs client, fetch files from storage nodes directly
	// This is the preferred method as it doesn't require external network access
	if i.monofsClient != nil {
		return i.indexFromMonoFS(ctx, req, start)
	}

	// Fallback: fetch from external sources directly (requires network access)
	return i.indexFromExternal(ctx, req, start)
}

// indexFromMonoFS indexes a repository using the MonoFS client to fetch files
// from storage nodes. This is the preferred method as the data is already ingested.
func (i *Indexer) indexFromMonoFS(ctx context.Context, req IndexRequest, start time.Time) (*IndexResult, error) {
	i.logger.Info("fetching files from MonoFS cluster",
		"storage_id", req.StorageID,
		"display_path", req.DisplayPath)

	// Create Zoekt builder options
	opts := index.Options{
		IndexDir: i.indexDir,
		RepositoryDescription: zoekt.Repository{
			Name:   req.DisplayPath,
			Source: req.RepoURL,
			Branches: []zoekt.RepositoryBranch{
				{Name: req.Ref, Version: "HEAD"},
			},
		},
	}
	opts.SetDefaults()

	builder, err := index.NewBuilder(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to create builder: %w", err)
	}

	// Walk the repository tree using MonoFS client
	var filesIndexed int64
	if err := i.walkMonoFSTree(ctx, req.DisplayPath, "", req.Ref, builder, &filesIndexed); err != nil {
		builder.Finish() // Clean up builder
		return nil, fmt.Errorf("failed to walk repository: %w", err)
	}

	// Finish building the index
	if err := builder.Finish(); err != nil {
		return nil, fmt.Errorf("failed to finish index: %w", err)
	}

	// Calculate index size
	var indexSize int64
	displayPathEscaped := url.QueryEscape(req.DisplayPath)
	filepath.WalkDir(i.indexDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(filepath.Base(path), displayPathEscaped) {
			if info, err := d.Info(); err == nil {
				indexSize += info.Size()
			}
		}
		return nil
	})

	i.logger.Info("indexing from MonoFS completed",
		"storage_id", req.StorageID,
		"files", filesIndexed,
		"index_size", indexSize,
		"duration", time.Since(start))

	return &IndexResult{
		FilesIndexed:   filesIndexed,
		IndexSizeBytes: indexSize,
		Duration:       time.Since(start),
	}, nil
}

// walkMonoFSTree recursively walks the repository tree using MonoFS client
func (i *Indexer) walkMonoFSTree(ctx context.Context, repoPath, subPath, branch string, builder *index.Builder, filesIndexed *int64) error {
	// Construct the full path in MonoFS
	fullPath := repoPath
	if subPath != "" {
		fullPath = filepath.Join(repoPath, subPath)
	}

	// Read directory entries
	entries, err := i.monofsClient.ReadDir(ctx, fullPath)
	if err != nil {
		return fmt.Errorf("failed to read directory %s: %w", fullPath, err)
	}

	for _, entry := range entries {
		entryPath := entry.Name
		if subPath != "" {
			entryPath = filepath.Join(subPath, entry.Name)
		}

		// Check if it's a directory (mode has directory bit set)
		isDir := (entry.Mode & 0040000) != 0

		if isDir {
			// Skip .git directory
			if entry.Name == ".git" {
				continue
			}

			// Recurse into subdirectory
			if err := i.walkMonoFSTree(ctx, repoPath, entryPath, branch, builder, filesIndexed); err != nil {
				i.logger.Warn("failed to walk subdirectory",
					"path", entryPath,
					"error", err)
				// Continue with other entries
				continue
			}
		} else {
			// It's a file - fetch and index it
			filePath := filepath.Join(repoPath, entryPath)

			// Skip binary files based on extension
			if isBinaryFile(filePath) {
				continue
			}

			// Read file content via MonoFS client
			// We read in chunks, limit to 1MB max
			const maxSize = 1024 * 1024
			content, err := i.monofsClient.Read(ctx, filePath, 0, maxSize)
			if err != nil {
				i.logger.Warn("failed to read file",
					"path", filePath,
					"error", err)
				continue
			}

			// Skip if file is too large (truncated read)
			if len(content) >= maxSize {
				continue
			}

			// Skip binary content
			if isBinaryContent(content) {
				continue
			}

			// Add to index
			doc := index.Document{
				Name:     entryPath,
				Content:  content,
				Branches: []string{branch},
			}

			if err := builder.Add(doc); err != nil {
				i.logger.Warn("failed to add file to index",
					"path", entryPath,
					"error", err)
				continue
			}

			*filesIndexed++
		}
	}

	return nil
}

// indexFromExternal fetches files from external sources (git clone/go mod download)
// This requires external network access and is used as fallback when no MonoFS client is configured.
func (i *Indexer) indexFromExternal(ctx context.Context, req IndexRequest, start time.Time) (*IndexResult, error) {
	// Determine source type and fetch accordingly
	repoDir := filepath.Join(i.cacheDir, req.StorageID)

	// Clean up any existing clone
	os.RemoveAll(repoDir)

	// Check if this is a Go module (contains @ sign)
	isGoModule := strings.Contains(req.RepoURL, "@")

	if isGoModule {
		// For Go modules, we need to download from Go proxy or use go get
		i.logger.Info("fetching Go module for indexing",
			"storage_id", req.StorageID,
			"module", req.RepoURL)

		// Create directory for the module
		if err := os.MkdirAll(repoDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create module directory: %w", err)
		}

		// Parse module path and version
		parts := strings.Split(req.RepoURL, "@")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid Go module format (expected module@version): %s", req.RepoURL)
		}
		modulePath := parts[0]
		moduleVersion := parts[1]

		// Use go mod download to fetch the module
		downloadCmd := exec.CommandContext(ctx, "go", "mod", "download", "-json", req.RepoURL)
		downloadCmd.Dir = repoDir
		downloadCmd.Env = append(os.Environ(),
			"GO111MODULE=on",
			"GOPROXY=https://proxy.golang.org,direct",
		)

		output, err := downloadCmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("go mod download failed: %w: %s", err, string(output))
		}

		// Parse the output to find the module directory
		var downloadInfo struct {
			Path    string `json:"Path"`
			Version string `json:"Version"`
			Dir     string `json:"Dir"`
			Error   string `json:"Error"`
		}
		if err := json.Unmarshal(output, &downloadInfo); err != nil {
			return nil, fmt.Errorf("failed to parse go mod download output: %w", err)
		}

		if downloadInfo.Error != "" {
			return nil, fmt.Errorf("go mod download error: %s", downloadInfo.Error)
		}

		if downloadInfo.Dir == "" {
			return nil, fmt.Errorf("go mod download did not return module directory")
		}

		// Copy files from the downloaded module to our repo directory
		if err := copyDir(downloadInfo.Dir, repoDir); err != nil {
			return nil, fmt.Errorf("failed to copy module files: %w", err)
		}

		i.logger.Info("go module fetched successfully",
			"storage_id", req.StorageID,
			"module", modulePath,
			"version", moduleVersion)
	} else {
		// Git repository - clone as before
		i.logger.Info("cloning repository for indexing",
			"storage_id", req.StorageID,
			"url", req.RepoURL,
			"branch", req.Ref)

		// Clone using git command (more reliable than go-git for large repos)
		cloneCmd := exec.CommandContext(ctx, "git", "clone",
			"--depth=1",
			"--single-branch",
			"--branch", req.Ref,
			req.RepoURL,
			repoDir,
		)
		cloneCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

		if output, err := cloneCmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git clone failed: %w: %s", err, string(output))
		}
	}

	defer func() {
		// Clean up clone after indexing
		os.RemoveAll(repoDir)
	}()

	// Build Zoekt index
	i.logger.Info("building zoekt index",
		"storage_id", req.StorageID,
		"repo_dir", repoDir)

	// Create Zoekt builder options - indexes go directly in indexDir
	// Zoekt will create shard files with unique names based on repo name
	opts := index.Options{
		IndexDir: i.indexDir,
		RepositoryDescription: zoekt.Repository{
			Name:   req.DisplayPath,
			Source: req.RepoURL,
			Branches: []zoekt.RepositoryBranch{
				{Name: req.Ref, Version: "HEAD"},
			},
		},
	}
	opts.SetDefaults()

	builder, err := index.NewBuilder(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to create builder: %w", err)
	}

	// Walk the repository and add files to the index
	var filesIndexed int64
	err = filepath.WalkDir(repoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip .git directory
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip binary files and large files
		info, err := d.Info()
		if err != nil {
			return nil // Skip files we can't stat
		}

		// Skip files larger than 1MB
		if info.Size() > 1024*1024 {
			return nil
		}

		// Skip binary files based on extension
		if isBinaryFile(path) {
			return nil
		}

		// Read file content
		content, err := os.ReadFile(path)
		if err != nil {
			return nil // Skip files we can't read
		}

		// Skip binary content
		if isBinaryContent(content) {
			return nil
		}

		// Get relative path
		relPath, err := filepath.Rel(repoDir, path)
		if err != nil {
			return nil
		}

		// Add to index
		doc := index.Document{
			Name:     relPath,
			Content:  content,
			Branches: []string{req.Ref},
		}

		if err := builder.Add(doc); err != nil {
			i.logger.Warn("failed to add file to index",
				"path", relPath,
				"error", err)
			return nil
		}

		filesIndexed++
		return nil
	})

	if err != nil {
		builder.Finish() // Clean up builder
		return nil, fmt.Errorf("failed to walk repository: %w", err)
	}

	// Finish building the index
	if err := builder.Finish(); err != nil {
		return nil, fmt.Errorf("failed to finish index: %w", err)
	}

	// Calculate index size by finding shards for this repo
	var indexSize int64
	displayPathEscaped := url.QueryEscape(req.DisplayPath)
	filepath.WalkDir(i.indexDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(filepath.Base(path), displayPathEscaped) {
			if info, err := d.Info(); err == nil {
				indexSize += info.Size()
			}
		}
		return nil
	})

	i.logger.Info("indexing completed",
		"storage_id", req.StorageID,
		"files", filesIndexed,
		"index_size", indexSize,
		"duration", time.Since(start))

	return &IndexResult{
		FilesIndexed:   filesIndexed,
		IndexSizeBytes: indexSize,
		Duration:       time.Since(start),
	}, nil
}

// Search performs a code search
func (i *Indexer) Search(ctx context.Context, req SearchRequest) (*SearchResults, error) {
	start := time.Now()

	// Build query
	var q query.Q
	var err error

	// Check if query contains Zoekt query syntax (lang:, file:, repo:, etc.)
	hasQuerySyntax := strings.Contains(req.Query, ":") &&
		(strings.Contains(req.Query, "lang:") ||
			strings.Contains(req.Query, "file:") ||
			strings.Contains(req.Query, "repo:") ||
			strings.Contains(req.Query, "case:") ||
			strings.Contains(req.Query, "sym:"))

	if req.Regex || hasQuerySyntax {
		// Parse using Zoekt's query language (supports lang:, file:, etc.)
		q, err = query.Parse(req.Query)
		if err != nil {
			return nil, fmt.Errorf("failed to parse query: %w", err)
		}
	} else {
		// If Regex mode is manually requested but query doesn't look like Zoekt syntax
		if req.Regex {
			re, err := syntax.Parse(req.Query, syntax.Perl)
			if err != nil {
				return nil, fmt.Errorf("invalid regex: %w", err)
			}
			q = &query.Regexp{
				Regexp:        re,
				CaseSensitive: req.CaseSensitive,
				Content:       true,
			}
		} else {
			// For literal text search, use Substring query directly
			// Set Content: true to search file content, not just filenames
			q = &query.Substring{
				Pattern:       req.Query,
				CaseSensitive: req.CaseSensitive,
				Content:       true,
			}
		}
	}

	// Apply file pattern filter if specified
	if len(req.FilePatterns) > 0 {
		var fileQueries []query.Q
		for _, pattern := range req.FilePatterns {
			// Convert glob pattern to regex
			// e.g., "*.go" -> ".*\.go$", "test_*.py" -> "test_.*\.py$"
			regexPattern := globToRegex(pattern)
			re, err := syntax.Parse(regexPattern, syntax.Perl)
			if err != nil {
				// Fall back to substring match if regex fails
				fq := &query.Substring{Pattern: pattern, FileName: true}
				fileQueries = append(fileQueries, fq)
			} else {
				fq := &query.Regexp{Regexp: re, FileName: true}
				fileQueries = append(fileQueries, fq)
			}
		}
		if len(fileQueries) > 1 {
			q = query.NewAnd(q, query.NewOr(fileQueries...))
		} else {
			q = query.NewAnd(q, fileQueries[0])
		}
	}

	// Apply repository filter if specified
	if req.StorageID != "" {
		// Look up DisplayPath for this StorageID
		i.mu.RLock()
		var targetDisplayPath string
		for dp, sid := range i.pathToStorageID {
			if sid == req.StorageID {
				targetDisplayPath = dp
				break
			}
		}
		i.mu.RUnlock()

		if targetDisplayPath != "" {
			// Exact match on repository name
			repoQ := &query.Repo{Regexp: grafanaregexp.MustCompile(regexp.QuoteMeta(targetDisplayPath))}
			q = query.NewAnd(q, repoQ)
		} else {
			// Repository not found in our mapping, don't return anything
			return &SearchResults{Duration: time.Since(start)}, nil
		}
	}

	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = 100
	}

	// Execute search
	searchOpts := &zoekt.SearchOptions{
		MaxMatchDisplayCount: maxResults,
		NumContextLines:      1,
		ChunkMatches:         true,
	}

	result, err := i.searcher.Search(ctx, q, searchOpts)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	// Get storage ID mapping
	i.mu.RLock()
	pathToStorageID := i.pathToStorageID
	i.mu.RUnlock()

	// Convert results
	var results []SearchResult
	var totalMatches int64

	for _, file := range result.Files {
		// Look up StorageID from DisplayPath
		storageID := pathToStorageID[file.Repository]

		for _, chunk := range file.ChunkMatches {
			for _, r := range chunk.Ranges {
				sr := SearchResult{
					StorageID:   storageID,
					DisplayPath: file.Repository,
					FilePath:    file.FileName,
					LineNumber:  int(r.Start.LineNumber),
					LineContent: strings.TrimRight(string(chunk.Content), "\n"),
					Matches: []MatchRange{
						{Start: int(r.Start.ByteOffset), End: int(r.End.ByteOffset)},
					},
				}
				results = append(results, sr)
				totalMatches++

				if len(results) >= maxResults {
					break
				}
			}
			if len(results) >= maxResults {
				break
			}
		}
		if len(results) >= maxResults {
			break
		}
	}

	return &SearchResults{
		Results:       results,
		TotalMatches:  totalMatches,
		FilesSearched: int64(result.Stats.ShardsScanned),
		Duration:      time.Since(start),
		Truncated:     len(results) >= maxResults,
	}, nil
}

// DeleteIndex removes the index for a repository by display path
func (i *Indexer) DeleteIndex(displayPath string) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Find and remove shard files matching this display path
	displayPathEscaped := url.QueryEscape(displayPath)

	return filepath.WalkDir(i.indexDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // Ignore errors
		}
		if !d.IsDir() && strings.Contains(filepath.Base(path), displayPathEscaped) && strings.HasSuffix(path, ".zoekt") {
			os.Remove(path)
		}
		return nil
	})
}

// GetIndexSize returns the index size for a repository by display path
func (i *Indexer) GetIndexSize(displayPath string) (int64, error) {
	displayPathEscaped := url.QueryEscape(displayPath)

	var size int64
	err := filepath.WalkDir(i.indexDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // Ignore errors
		}
		if !d.IsDir() && strings.Contains(filepath.Base(path), displayPathEscaped) {
			if info, err := d.Info(); err == nil {
				size += info.Size()
			}
		}
		return nil
	})

	return size, err
}

// IndexExists checks if an index exists for a repository by display path
func (i *Indexer) IndexExists(displayPath string) bool {
	// URL encode the displayPath to match Zoekt's naming convention
	displayPathEscaped := url.QueryEscape(displayPath)

	found := false
	filepath.WalkDir(i.indexDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(filepath.Base(path), displayPathEscaped) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// IndexLocalRequest contains parameters for indexing a local directory
type IndexLocalRequest struct {
	StorageID   string
	DisplayPath string
	SourceDir   string
	Ref         string
}

// IndexLocalDir indexes a local directory directly (for testing)
func (i *Indexer) IndexLocalDir(ctx context.Context, req IndexLocalRequest) (*IndexResult, error) {
	start := time.Now()

	// Store the mapping from DisplayPath to StorageID
	i.mu.Lock()
	i.pathToStorageID[req.DisplayPath] = req.StorageID
	i.mu.Unlock()

	i.logger.Info("indexing local directory",
		"storage_id", req.StorageID,
		"source_dir", req.SourceDir)

	// Create Zoekt builder options - indexes go directly in indexDir
	opts := index.Options{
		IndexDir: i.indexDir,
		RepositoryDescription: zoekt.Repository{
			Name:   req.DisplayPath,
			Source: req.SourceDir,
			Branches: []zoekt.RepositoryBranch{
				{Name: req.Ref, Version: "HEAD"},
			},
		},
	}
	opts.SetDefaults()

	builder, err := index.NewBuilder(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to create builder: %w", err)
	}

	// Walk the directory and add files to the index
	var filesIndexed int64
	err = filepath.WalkDir(req.SourceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip .git directory
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip binary files and large files
		info, err := d.Info()
		if err != nil {
			return nil
		}

		if info.Size() > 1024*1024 {
			return nil
		}

		if isBinaryFile(path) {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		if isBinaryContent(content) {
			return nil
		}

		relPath, err := filepath.Rel(req.SourceDir, path)
		if err != nil {
			return nil
		}

		doc := index.Document{
			Name:     relPath,
			Content:  content,
			Branches: []string{req.Ref},
		}

		if err := builder.Add(doc); err != nil {
			i.logger.Warn("failed to add file to index", "path", relPath, "error", err)
			return nil
		}

		filesIndexed++
		return nil
	})

	if err != nil {
		builder.Finish()
		return nil, fmt.Errorf("failed to walk directory: %w", err)
	}

	if err := builder.Finish(); err != nil {
		return nil, fmt.Errorf("failed to finish index: %w", err)
	}

	// Calculate index size by finding shards for this repo
	var indexSize int64
	displayPathEscaped := url.QueryEscape(req.DisplayPath)
	filepath.WalkDir(i.indexDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(filepath.Base(path), displayPathEscaped) {
			if info, err := d.Info(); err == nil {
				indexSize += info.Size()
			}
		}
		return nil
	})

	i.logger.Info("local indexing completed",
		"storage_id", req.StorageID,
		"files", filesIndexed,
		"index_size", indexSize,
		"duration", time.Since(start))

	return &IndexResult{
		FilesIndexed:   filesIndexed,
		IndexSizeBytes: indexSize,
		Duration:       time.Since(start),
	}, nil
}

// ReloadSearcher reloads the searcher to pick up new indexes
func (i *Indexer) ReloadSearcher() error {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.searcher != nil {
		i.searcher.Close()
	}

	searcher, err := search.NewDirectorySearcher(i.indexDir)
	if err != nil {
		return fmt.Errorf("failed to create searcher: %w", err)
	}
	i.searcher = searcher
	return nil
}

// isBinaryFile checks if a file is likely binary based on extension
func isBinaryFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	binaryExts := map[string]bool{
		".exe": true, ".dll": true, ".so": true, ".dylib": true,
		".zip": true, ".tar": true, ".gz": true, ".bz2": true, ".xz": true, ".7z": true,
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".bmp": true, ".ico": true, ".webp": true,
		".mp3": true, ".mp4": true, ".avi": true, ".mkv": true, ".mov": true, ".wav": true,
		".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true,
		".wasm": true, ".o": true, ".a": true, ".lib": true,
		".pyc": true, ".pyo": true, ".class": true,
		".ttf": true, ".otf": true, ".woff": true, ".woff2": true, ".eot": true,
	}
	return binaryExts[ext]
}

// isBinaryContent checks if content is likely binary
func isBinaryContent(content []byte) bool {
	// Check first 8KB for null bytes
	checkLen := len(content)
	if checkLen > 8192 {
		checkLen = 8192
	}

	for i := 0; i < checkLen; i++ {
		if content[i] == 0 {
			return true
		}
	}
	return false
}

// copyDir recursively copies a directory tree
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Calculate destination path
		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(dst, relPath)

		if info.IsDir() {
			// Create directory with writable permissions
			if err := os.MkdirAll(dstPath, 0755); err != nil {
				return err
			}
			// Ensure the directory is writable (in case it already existed)
			return os.Chmod(dstPath, 0755)
		}

		// Copy file
		srcFile, err := os.Open(path)
		if err != nil {
			return err
		}
		defer srcFile.Close()

		// Create file with readable permissions (preserve execute bit if present)
		mode := os.FileMode(0644)
		if info.Mode()&0111 != 0 {
			mode = 0755
		}
		dstFile, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		defer dstFile.Close()

		_, err = io.Copy(dstFile, srcFile)
		return err
	})
}

package fetcher

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CargoBackend fetches files from crates.io registry.
type CargoBackend struct {
	config BackendConfig

	// HTTP client for crates.io
	client *http.Client

	// API URL (default: crates.io)
	apiURL string

	mu     sync.RWMutex
	crates map[string]*cachedCargoCrate // crate@version -> cached crate

	stats atomic.Pointer[BackendStats]
}

type cachedCargoCrate struct {
	crateName  string
	version    string
	cratePath  string            // Path to cached .crate file
	files      map[string][]byte // Extracted files (lazy loaded)
	lastAccess time.Time
	mu         sync.Mutex
}

type cratesIOCrateMetadata struct {
	Crate struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"crate"`
	Versions []cratesIOVersion `json:"versions"`
}

type cratesIOVersion struct {
	Num       string `json:"num"`
	DL_Path   string `json:"dl_path"`
	Downloads int    `json:"downloads"`
}

// NewCargoBackend creates a new crates.io backend.
func NewCargoBackend() *CargoBackend {
	cb := &CargoBackend{
		crates: make(map[string]*cachedCargoCrate),
		client: &http.Client{
			Timeout: 5 * time.Minute,
		},
		apiURL: "https://crates.io",
	}
	cb.stats.Store(&BackendStats{})
	return cb
}

func (cb *CargoBackend) Type() SourceType {
	return SourceTypeCargo
}

func (cb *CargoBackend) Initialize(ctx context.Context, config BackendConfig) error {
	cb.config = config

	// Override API URL if configured
	if url, ok := config.Extra["api_url"]; ok && url != "" {
		cb.apiURL = url
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(config.CacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cargo cache dir: %w", err)
	}

	// Scan existing crates in cache
	entries, err := os.ReadDir(config.CacheDir)
	if err != nil {
		return nil // Empty cache is fine
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Directory structure: crate_hash/version/
		cratePath := filepath.Join(config.CacheDir, entry.Name())
		versions, _ := os.ReadDir(cratePath)
		for _, ver := range versions {
			if !ver.IsDir() {
				continue
			}
			crateFile := filepath.Join(cratePath, ver.Name(), "crate.tar.gz")
			if _, err := os.Stat(crateFile); err == nil {
				// Found a cached crate
				key := entry.Name() + "@" + ver.Name()
				cb.crates[key] = &cachedCargoCrate{
					crateName:  entry.Name(),
					version:    ver.Name(),
					cratePath:  crateFile,
					files:      make(map[string][]byte),
					lastAccess: time.Now(),
				}
			}
		}
	}

	// Start cleanup goroutine
	go cb.cleanupLoop(ctx)

	return nil
}

func (cb *CargoBackend) FetchBlob(ctx context.Context, req *FetchRequest) (*FetchResult, error) {
	start := time.Now()

	// ContentID format: crate@version/path/to/file
	crateName, version, filePath, err := parseCargoContentID(req.ContentID)
	if err != nil {
		// ContentID is likely a blob hash, not a crate path
		// Get crate info from source config
		crateName = req.SourceConfig["crate_name"]
		version = req.SourceConfig["version"]
		// Get file path from source config
		filePath = req.SourceConfig["file_path"]
		if filePath == "" {
			filePath = req.SourceConfig["display_path"]
		}
	}

	if crateName == "" || version == "" {
		cb.recordError()
		return nil, fmt.Errorf("invalid cargo request: missing crate_name or version")
	}

	// Get or download crate
	cached, err := cb.getOrDownloadCrate(ctx, crateName, version)
	if err != nil {
		cb.recordError()
		return nil, fmt.Errorf("failed to get crate: %w", err)
	}

	// Read file from crate
	content, err := cb.readFileFromCrate(cached, filePath)
	if err != nil {
		cb.recordError()
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	latency := time.Since(start).Milliseconds()
	cb.recordSuccess(int64(len(content)), latency, false)

	return &FetchResult{
		Content:   content,
		Size:      int64(len(content)),
		FromCache: false,
	}, nil
}

func (cb *CargoBackend) FetchBlobStream(ctx context.Context, req *FetchRequest) (io.ReadCloser, int64, error) {
	result, err := cb.FetchBlob(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(result.Content)), result.Size, nil
}

func (cb *CargoBackend) Warmup(ctx context.Context, sourceKey string, config map[string]string) error {
	crateName := config["crate_name"]
	version := config["version"]

	if crateName == "" || version == "" {
		return fmt.Errorf("warmup requires crate_name and version")
	}

	_, err := cb.getOrDownloadCrate(ctx, crateName, version)
	return err
}

func (cb *CargoBackend) CachedSources() []string {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	sources := make([]string, 0, len(cb.crates))
	for key := range cb.crates {
		sources = append(sources, key)
	}
	return sources
}

func (cb *CargoBackend) Cleanup(ctx context.Context, sourceKey string) error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cached, ok := cb.crates[sourceKey]
	if !ok {
		return nil
	}

	// Remove from memory
	delete(cb.crates, sourceKey)

	// Remove from disk
	if cached.cratePath != "" {
		dir := filepath.Dir(cached.cratePath)
		return os.RemoveAll(dir)
	}

	return nil
}

func (cb *CargoBackend) Close() error {
	return nil
}

func (cb *CargoBackend) Stats() BackendStats {
	return *cb.stats.Load()
}

// getOrDownloadCrate retrieves a crate from cache or downloads it.
func (cb *CargoBackend) getOrDownloadCrate(ctx context.Context, crateName, version string) (*cachedCargoCrate, error) {
	key := crateName + "@" + version

	// Check cache first
	cb.mu.RLock()
	cached, ok := cb.crates[key]
	cb.mu.RUnlock()

	if ok {
		cached.mu.Lock()
		cached.lastAccess = time.Now()
		cached.mu.Unlock()
		return cached, nil
	}

	// Download crate
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Double-check (another goroutine may have downloaded it)
	if cached, ok := cb.crates[key]; ok {
		return cached, nil
	}

	// Download .crate file
	cratePath, err := cb.downloadCrate(ctx, crateName, version)
	if err != nil {
		return nil, fmt.Errorf("failed to download crate: %w", err)
	}

	cached = &cachedCargoCrate{
		crateName:  crateName,
		version:    version,
		cratePath:  cratePath,
		files:      make(map[string][]byte),
		lastAccess: time.Now(),
	}

	cb.crates[key] = cached
	return cached, nil
}

// downloadCrate downloads the .crate tarball from crates.io.
func (cb *CargoBackend) downloadCrate(ctx context.Context, crateName, version string) (string, error) {
	// crates.io download URL:
	// https://crates.io/api/v1/crates/<crate>/<version>/download
	downloadURL := fmt.Sprintf("%s/api/v1/crates/%s/%s/download", cb.apiURL, crateName, version)

	// Create cache directory: cache/cargo/<crate_hash>/<version>/
	crateHash := sanitizeCrateName(crateName)
	cacheDir := filepath.Join(cb.config.CacheDir, crateHash, version)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", err
	}

	cratePath := filepath.Join(cacheDir, "crate.tar.gz")

	// Download .crate file
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return "", err
	}

	// crates.io requires User-Agent header
	req.Header.Set("User-Agent", "monofs-fetcher/1.0")

	resp, err := cb.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download crate: HTTP %d from %s", resp.StatusCode, downloadURL)
	}

	// Save to file
	f, err := os.Create(cratePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(cratePath)
		return "", err
	}

	return cratePath, nil
}

// readFileFromCrate extracts and reads a file from the cached crate tarball.
func (cb *CargoBackend) readFileFromCrate(cached *cachedCargoCrate, filePath string) ([]byte, error) {
	cached.mu.Lock()
	defer cached.mu.Unlock()

	// Check if already extracted
	if content, ok := cached.files[filePath]; ok {
		return content, nil
	}

	// Open crate file
	f, err := os.Open(cached.cratePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Decompress gzip
	gzr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("failed to open gzip: %w", err)
	}
	defer gzr.Close()

	// Read tar
	tr := tar.NewReader(gzr)

	// crates.io tarballs have a "<crate>-<version>/" prefix in all paths
	prefix := fmt.Sprintf("%s-%s/", cached.crateName, cached.version)
	targetPath := prefix + strings.TrimPrefix(filePath, "/")

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		if header.Name == targetPath {
			content, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}

			// Cache the extracted file
			cached.files[filePath] = content
			return content, nil
		}
	}

	return nil, fmt.Errorf("file not found in crate: %s", filePath)
}

// cleanupLoop periodically removes old cached crates.
func (cb *CargoBackend) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cb.cleanupOldCrates()
		}
	}
}

func (cb *CargoBackend) cleanupOldCrates() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cutoff := time.Now().Add(-24 * time.Hour)

	for key, cached := range cb.crates {
		cached.mu.Lock()
		if cached.lastAccess.Before(cutoff) {
			// Remove from disk
			if cached.cratePath != "" {
				dir := filepath.Dir(cached.cratePath)
				os.RemoveAll(dir)
			}
			delete(cb.crates, key)
		}
		cached.mu.Unlock()
	}
}

// parseCargoContentID parses crate@version/path/to/file format.
func parseCargoContentID(contentID string) (crateName, version, filePath string, err error) {
	// Format: crate@version/path/to/file
	// Example: serde@1.0.193/src/lib.rs

	parts := strings.SplitN(contentID, "@", 2)
	if len(parts) != 2 {
		return "", "", "", fmt.Errorf("invalid cargo content ID format: %s", contentID)
	}

	crateName = parts[0]
	rest := parts[1]

	// Split version and file path
	slashIdx := strings.Index(rest, "/")
	if slashIdx == -1 {
		version = rest
		filePath = ""
	} else {
		version = rest[:slashIdx]
		filePath = rest[slashIdx+1:]
	}

	return crateName, version, filePath, nil
}

// sanitizeCrateName converts crate name to safe directory name.
func sanitizeCrateName(name string) string {
	// Replace any problematic characters
	s := strings.ReplaceAll(name, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

// recordSuccess updates backend statistics for successful fetch.
func (cb *CargoBackend) recordSuccess(bytes, latencyMs int64, cached bool) {
	stats := cb.stats.Load()
	newStats := *stats
	newStats.Requests++
	newStats.BytesFetched += bytes
	if cached {
		newStats.CacheHits++
	} else {
		newStats.CacheMisses++
	}
	cb.stats.Store(&newStats)
}

// recordError updates backend statistics for failed fetch.
func (cb *CargoBackend) recordError() {
	stats := cb.stats.Load()
	newStats := *stats
	newStats.Requests++
	newStats.Errors++
	cb.stats.Store(&newStats)
}

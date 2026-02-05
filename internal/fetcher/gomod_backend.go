package fetcher

import (
	"archive/zip"
	"bytes"
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

// GoModBackend fetches files from Go module proxies.
type GoModBackend struct {
	config BackendConfig

	// HTTP client for module proxy
	client *http.Client

	// Proxy URL (default: proxy.golang.org)
	proxyURL string

	mu      sync.RWMutex
	modules map[string]*cachedModule // module@version -> cached module

	stats atomic.Pointer[BackendStats]
}

type cachedModule struct {
	modulePath string
	version    string
	zipPath    string            // Path to cached zip file
	files      map[string][]byte // Extracted files (lazy loaded)
	lastAccess time.Time
	mu         sync.Mutex
}

// NewGoModBackend creates a new Go modules backend.
func NewGoModBackend() *GoModBackend {
	gb := &GoModBackend{
		modules: make(map[string]*cachedModule),
		client: &http.Client{
			Timeout: 5 * time.Minute,
		},
		proxyURL: "https://proxy.golang.org",
	}
	gb.stats.Store(&BackendStats{})
	return gb
}

func (gb *GoModBackend) Type() SourceType {
	return SourceTypeGoMod
}

func (gb *GoModBackend) Initialize(ctx context.Context, config BackendConfig) error {
	gb.config = config

	// Override proxy URL if configured
	if url, ok := config.Extra["proxy_url"]; ok && url != "" {
		gb.proxyURL = url
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(config.CacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create gomod cache dir: %w", err)
	}

	// Scan existing modules in cache
	entries, err := os.ReadDir(config.CacheDir)
	if err != nil {
		return nil // Empty cache is fine
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Directory structure: module_hash/version/
		modulePath := filepath.Join(config.CacheDir, entry.Name())
		versions, _ := os.ReadDir(modulePath)
		for _, ver := range versions {
			if !ver.IsDir() {
				continue
			}
			zipPath := filepath.Join(modulePath, ver.Name(), "module.zip")
			if _, err := os.Stat(zipPath); err == nil {
				// Found a cached module
				key := entry.Name() + "@" + ver.Name()
				gb.modules[key] = &cachedModule{
					modulePath: entry.Name(),
					version:    ver.Name(),
					zipPath:    zipPath,
					files:      make(map[string][]byte),
					lastAccess: time.Now(),
				}
			}
		}
	}

	// Start cleanup goroutine
	go gb.cleanupLoop(ctx)

	return nil
}

func (gb *GoModBackend) FetchBlob(ctx context.Context, req *FetchRequest) (*FetchResult, error) {
	start := time.Now()

	// ContentID format: module@version/path/to/file
	modulePath, version, filePath, err := parseGoModContentID(req.ContentID)
	if err != nil {
		// ContentID is likely a blob hash, not a module path
		// Get module info from source config
		modulePath = req.SourceConfig["module_path"]
		version = req.SourceConfig["version"]
		// Get file path from source config (file_path or display_path)
		filePath = req.SourceConfig["file_path"]
		if filePath == "" {
			filePath = req.SourceConfig["display_path"]
		}
	}

	if modulePath == "" || version == "" {
		gb.recordError()
		return nil, fmt.Errorf("invalid go module request: missing module_path or version")
	}

	// Get or download module
	cached, err := gb.getOrDownloadModule(ctx, modulePath, version)
	if err != nil {
		gb.recordError()
		return nil, fmt.Errorf("failed to get module: %w", err)
	}

	// Read file from module
	content, err := gb.readFileFromModule(cached, filePath)
	if err != nil {
		gb.recordError()
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	latency := time.Since(start).Milliseconds()
	gb.recordSuccess(int64(len(content)), latency, false)

	return &FetchResult{
		Content:        content,
		Size:           int64(len(content)),
		FromCache:      false,
		FetchLatencyMs: latency,
	}, nil
}

func (gb *GoModBackend) FetchBlobStream(ctx context.Context, req *FetchRequest) (io.ReadCloser, int64, error) {
	// For Go modules, we don't stream - files are typically small
	result, err := gb.FetchBlob(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(result.Content)), result.Size, nil
}

func (gb *GoModBackend) Warmup(ctx context.Context, sourceKey string, config map[string]string) error {
	modulePath := config["module_path"]
	version := config["version"]
	if modulePath == "" {
		modulePath = sourceKey
	}
	if version == "" {
		version = "latest"
	}

	_, err := gb.getOrDownloadModule(ctx, modulePath, version)
	return err
}

func (gb *GoModBackend) CachedSources() []string {
	gb.mu.RLock()
	defer gb.mu.RUnlock()

	sources := make([]string, 0, len(gb.modules))
	for key := range gb.modules {
		sources = append(sources, key)
	}
	return sources
}

func (gb *GoModBackend) Cleanup(ctx context.Context, sourceKey string) error {
	gb.mu.Lock()
	defer gb.mu.Unlock()

	cached, ok := gb.modules[sourceKey]
	if !ok {
		return nil
	}

	delete(gb.modules, sourceKey)

	// Remove from disk
	if cached.zipPath != "" {
		os.RemoveAll(filepath.Dir(cached.zipPath))
	}

	return nil
}

func (gb *GoModBackend) Close() error {
	gb.mu.Lock()
	defer gb.mu.Unlock()

	gb.modules = make(map[string]*cachedModule)
	return nil
}

func (gb *GoModBackend) Stats() BackendStats {
	return *gb.stats.Load()
}

// getOrDownloadModule returns a cached module or downloads it.
func (gb *GoModBackend) getOrDownloadModule(ctx context.Context, modulePath, version string) (*cachedModule, error) {
	key := modulePath + "@" + version

	gb.mu.RLock()
	cached, ok := gb.modules[key]
	gb.mu.RUnlock()

	if ok {
		cached.mu.Lock()
		cached.lastAccess = time.Now()
		cached.mu.Unlock()
		return cached, nil
	}

	gb.mu.Lock()
	defer gb.mu.Unlock()

	// Double-check
	if cached, ok = gb.modules[key]; ok {
		return cached, nil
	}

	// Resolve "latest" version
	if version == "latest" {
		resolvedVersion, err := gb.resolveLatestVersion(ctx, modulePath)
		if err != nil {
			return nil, err
		}
		version = resolvedVersion
		key = modulePath + "@" + version
	}

	// Download module zip
	zipPath, err := gb.downloadModule(ctx, modulePath, version)
	if err != nil {
		return nil, err
	}

	cached = &cachedModule{
		modulePath: modulePath,
		version:    version,
		zipPath:    zipPath,
		files:      make(map[string][]byte),
		lastAccess: time.Now(),
	}
	gb.modules[key] = cached

	return cached, nil
}

func (gb *GoModBackend) resolveLatestVersion(ctx context.Context, modulePath string) (string, error) {
	// Escape module path for URL
	escaped := escapeModulePath(modulePath)
	url := fmt.Sprintf("%s/%s/@latest", gb.proxyURL, escaped)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}

	resp, err := gb.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to resolve latest version: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// Response is JSON: {"Version":"v1.2.3","Time":"..."}
	// Simple parsing
	for _, line := range strings.Split(string(body), ",") {
		if strings.Contains(line, "Version") {
			parts := strings.Split(line, ":")
			if len(parts) >= 2 {
				version := strings.Trim(parts[1], `"} `)
				return version, nil
			}
		}
	}

	return "", fmt.Errorf("failed to parse version from response")
}

func (gb *GoModBackend) downloadModule(ctx context.Context, modulePath, version string) (string, error) {
	escaped := escapeModulePath(modulePath)
	url := fmt.Sprintf("%s/%s/@v/%s.zip", gb.proxyURL, escaped, version)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}

	resp, err := gb.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download module: %s", resp.Status)
	}

	// Save to cache
	cacheDir := filepath.Join(gb.config.CacheDir, hashString(modulePath), version)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", err
	}

	zipPath := filepath.Join(cacheDir, "module.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	if err != nil {
		os.Remove(zipPath)
		return "", err
	}

	return zipPath, nil
}

func (gb *GoModBackend) readFileFromModule(cached *cachedModule, filePath string) ([]byte, error) {
	cached.mu.Lock()
	defer cached.mu.Unlock()

	// Check in-memory cache
	if content, ok := cached.files[filePath]; ok {
		return content, nil
	}

	// Read from zip
	r, err := zip.OpenReader(cached.zipPath)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	// Files in zip are prefixed with module@version/
	prefix := cached.modulePath + "@" + cached.version + "/"
	targetPath := prefix + filePath

	for _, f := range r.File {
		if f.Name == targetPath || strings.TrimPrefix(f.Name, prefix) == filePath {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			content, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return nil, err
			}

			// Cache in memory (limit size)
			if len(content) < 1024*1024 { // < 1MB
				cached.files[filePath] = content
			}

			return content, nil
		}
	}

	return nil, fmt.Errorf("file not found in module: %s", filePath)
}

func (gb *GoModBackend) cleanupLoop(ctx context.Context) {
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
			gb.cleanupOldModules(maxAge)
		}
	}
}

func (gb *GoModBackend) cleanupOldModules(maxAge time.Duration) {
	gb.mu.Lock()
	defer gb.mu.Unlock()

	now := time.Now()
	for key, cached := range gb.modules {
		if now.Sub(cached.lastAccess) > maxAge {
			delete(gb.modules, key)
			if cached.zipPath != "" {
				os.RemoveAll(filepath.Dir(cached.zipPath))
			}
		}
	}
}

func (gb *GoModBackend) recordSuccess(bytes int64, latencyMs int64, fromCache bool) {
	for {
		old := gb.stats.Load()
		new := &BackendStats{
			Requests:     old.Requests + 1,
			Errors:       old.Errors,
			BytesFetched: old.BytesFetched + bytes,
			CachedItems:  int64(len(gb.modules)),
		}
		if fromCache {
			new.CacheHits = old.CacheHits + 1
		} else {
			new.CacheMisses = old.CacheMisses + 1
		}
		new.AvgLatencyMs = (old.AvgLatencyMs*float64(old.Requests) + float64(latencyMs)) / float64(new.Requests)
		if gb.stats.CompareAndSwap(old, new) {
			return
		}
	}
}

func (gb *GoModBackend) recordError() {
	for {
		old := gb.stats.Load()
		new := &BackendStats{
			Requests:     old.Requests + 1,
			Errors:       old.Errors + 1,
			BytesFetched: old.BytesFetched,
			CacheHits:    old.CacheHits,
			CacheMisses:  old.CacheMisses + 1,
			CachedItems:  old.CachedItems,
			AvgLatencyMs: old.AvgLatencyMs,
		}
		if gb.stats.CompareAndSwap(old, new) {
			return
		}
	}
}

// parseGoModContentID parses "module@version/path" format.
func parseGoModContentID(contentID string) (modulePath, version, filePath string, err error) {
	// Format: github.com/owner/repo@v1.2.3/path/to/file.go
	atIdx := strings.Index(contentID, "@")
	if atIdx == -1 {
		return "", "", "", fmt.Errorf("invalid content ID: missing @version")
	}

	modulePath = contentID[:atIdx]
	rest := contentID[atIdx+1:]

	slashIdx := strings.Index(rest, "/")
	if slashIdx == -1 {
		return "", "", "", fmt.Errorf("invalid content ID: missing file path")
	}

	version = rest[:slashIdx]
	filePath = rest[slashIdx+1:]

	return modulePath, version, filePath, nil
}

// escapeModulePath escapes a module path for use in URLs.
func escapeModulePath(path string) string {
	// Capital letters are escaped as !lowercase
	var result strings.Builder
	for _, r := range path {
		if r >= 'A' && r <= 'Z' {
			result.WriteRune('!')
			result.WriteRune(r + ('a' - 'A'))
		} else {
			result.WriteRune(r)
		}
	}
	return result.String()
}

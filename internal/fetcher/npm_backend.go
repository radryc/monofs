package fetcher

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

// NpmBackend fetches files from npm registry.
type NpmBackend struct {
	BaseBackend
	config BackendConfig

	// HTTP client for npm registry
	client *http.Client

	// Registry URL (default: registry.npmjs.org)
	registryURL string

	mu       sync.RWMutex
	packages map[string]*cachedNpmPackage // package@version -> cached package
}

type cachedNpmPackage struct {
	packageName string
	version     string
	tarballPath string            // Path to cached tarball
	files       map[string][]byte // Extracted files (lazy loaded)
	lastAccess  time.Time
	mu          sync.Mutex
}

type npmPackageMetadata struct {
	Name     string                        `json:"name"`
	Versions map[string]npmVersionMetadata `json:"versions"`
}

type npmVersionMetadata struct {
	Name    string          `json:"name"`
	Version string          `json:"version"`
	Dist    npmDistMetadata `json:"dist"`
}

type npmDistMetadata struct {
	Tarball string `json:"tarball"`
}

// NewNpmBackend creates a new npm registry backend.
func NewNpmBackend() *NpmBackend {
	nb := &NpmBackend{
		packages: make(map[string]*cachedNpmPackage),
		client: &http.Client{
			Timeout: 5 * time.Minute,
		},
		registryURL: "https://registry.npmjs.org",
	}
	nb.InitStats()
	return nb
}

func (nb *NpmBackend) Type() pb.SourceType {
	return pb.SourceType_SOURCE_TYPE_NPM
}

func (nb *NpmBackend) Initialize(ctx context.Context, config BackendConfig) error {
	nb.config = config

	// Override registry URL if configured
	if url, ok := config.Extra["registry_url"]; ok && url != "" {
		nb.registryURL = url
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(config.CacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create npm cache dir: %w", err)
	}

	// Scan existing packages in cache
	entries, err := os.ReadDir(config.CacheDir)
	if err != nil {
		return nil // Empty cache is fine
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Directory structure: package_hash/version/
		packagePath := filepath.Join(config.CacheDir, entry.Name())
		versions, _ := os.ReadDir(packagePath)
		for _, ver := range versions {
			if !ver.IsDir() {
				continue
			}
			tarballPath := filepath.Join(packagePath, ver.Name(), "package.tgz")
			if _, err := os.Stat(tarballPath); err == nil {
				// Found a cached package
				key := entry.Name() + "@" + ver.Name()
				nb.packages[key] = &cachedNpmPackage{
					packageName: entry.Name(),
					version:     ver.Name(),
					tarballPath: tarballPath,
					files:       make(map[string][]byte),
					lastAccess:  time.Now(),
				}
			}
		}
	}

	// Start cleanup goroutine
	go nb.cleanupLoop(ctx)

	return nil
}

func (nb *NpmBackend) FetchBlob(ctx context.Context, req *FetchRequest) (*FetchResult, error) {
	start := time.Now()

	// ContentID format: package@version/path/to/file
	packageName, version, filePath, err := parseNpmContentID(req.ContentID)
	if err != nil {
		// ContentID is likely a blob hash, not a package path
		// Get package info from source config
		packageName = req.SourceConfig["package_name"]
		version = req.SourceConfig["version"]
		// Get file path from source config
		filePath = req.SourceConfig["file_path"]
		if filePath == "" {
			filePath = req.SourceConfig["display_path"]
		}
	}

	if packageName == "" || version == "" {
		nb.RecordError()
		return nil, fmt.Errorf("invalid npm request: missing package_name or version")
	}

	// Get or download package
	cached, err := nb.getOrDownloadPackage(ctx, packageName, version)
	if err != nil {
		nb.RecordError()
		return nil, fmt.Errorf("failed to get package: %w", err)
	}

	// Read file from package
	content, err := nb.readFileFromPackage(cached, filePath)
	if err != nil {
		nb.RecordError()
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	latency := time.Since(start).Milliseconds()
	nb.RecordSuccess(int64(len(content)), latency, false, int64(len(nb.packages)))

	return &FetchResult{
		Content:   content,
		Size:      int64(len(content)),
		FromCache: false,
	}, nil
}

func (nb *NpmBackend) FetchBlobStream(ctx context.Context, req *FetchRequest) (io.ReadCloser, int64, error) {
	return DefaultFetchBlobStream(nb, ctx, req)
}

func (nb *NpmBackend) Warmup(ctx context.Context, sourceKey string, config map[string]string) error {
	packageName := config["package_name"]
	version := config["version"]

	if packageName == "" || version == "" {
		return fmt.Errorf("warmup requires package_name and version")
	}

	_, err := nb.getOrDownloadPackage(ctx, packageName, version)
	return err
}

func (nb *NpmBackend) CachedSources() []string {
	nb.mu.RLock()
	defer nb.mu.RUnlock()

	sources := make([]string, 0, len(nb.packages))
	for key := range nb.packages {
		sources = append(sources, key)
	}
	return sources
}

func (nb *NpmBackend) Cleanup(ctx context.Context, sourceKey string) error {
	nb.mu.Lock()
	defer nb.mu.Unlock()

	cached, ok := nb.packages[sourceKey]
	if !ok {
		return nil
	}

	// Remove from memory
	delete(nb.packages, sourceKey)

	// Remove from disk
	if cached.tarballPath != "" {
		dir := filepath.Dir(cached.tarballPath)
		return os.RemoveAll(dir)
	}

	return nil
}

func (nb *NpmBackend) Close() error {
	return nil
}

// getOrDownloadPackage retrieves a package from cache or downloads it.
func (nb *NpmBackend) getOrDownloadPackage(ctx context.Context, packageName, version string) (*cachedNpmPackage, error) {
	key := packageName + "@" + version

	// Check cache first
	nb.mu.RLock()
	cached, ok := nb.packages[key]
	nb.mu.RUnlock()

	if ok {
		cached.mu.Lock()
		cached.lastAccess = time.Now()
		cached.mu.Unlock()
		return cached, nil
	}

	// Download package
	nb.mu.Lock()
	defer nb.mu.Unlock()

	// Double-check (another goroutine may have downloaded it)
	if cached, ok := nb.packages[key]; ok {
		return cached, nil
	}

	// Get package metadata to find tarball URL
	tarballURL, err := nb.getTarballURL(ctx, packageName, version)
	if err != nil {
		return nil, fmt.Errorf("failed to get tarball URL: %w", err)
	}

	// Download tarball
	tarballPath, err := nb.downloadTarball(ctx, packageName, version, tarballURL)
	if err != nil {
		return nil, fmt.Errorf("failed to download tarball: %w", err)
	}

	cached = &cachedNpmPackage{
		packageName: packageName,
		version:     version,
		tarballPath: tarballPath,
		files:       make(map[string][]byte),
		lastAccess:  time.Now(),
	}

	nb.packages[key] = cached
	return cached, nil
}

// getTarballURL fetches package metadata from npm registry to get tarball URL.
func (nb *NpmBackend) getTarballURL(ctx context.Context, packageName, version string) (string, error) {
	// npm registry API: GET https://registry.npmjs.org/<package>
	// For scoped packages: GET https://registry.npmjs.org/@scope/package
	url := fmt.Sprintf("%s/%s", nb.registryURL, packageName)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}

	resp, err := nb.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("npm registry returned status %d for %s", resp.StatusCode, packageName)
	}

	var metadata npmPackageMetadata
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return "", fmt.Errorf("failed to parse package metadata: %w", err)
	}

	versionMeta, ok := metadata.Versions[version]
	if !ok {
		return "", fmt.Errorf("version %s not found for package %s", version, packageName)
	}

	if versionMeta.Dist.Tarball == "" {
		return "", fmt.Errorf("no tarball URL found for %s@%s", packageName, version)
	}

	return versionMeta.Dist.Tarball, nil
}

// downloadTarball downloads the tarball and saves it to cache.
func (nb *NpmBackend) downloadTarball(ctx context.Context, packageName, version, tarballURL string) (string, error) {
	// Create cache directory: cache/npm/<package_hash>/<version>/
	packageHash := sanitizePackageName(packageName)
	cacheDir := filepath.Join(nb.config.CacheDir, packageHash, version)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", err
	}

	tarballPath := filepath.Join(cacheDir, "package.tgz")

	// Download tarball
	req, err := http.NewRequestWithContext(ctx, "GET", tarballURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := nb.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download tarball: HTTP %d", resp.StatusCode)
	}

	// Save to file
	f, err := os.Create(tarballPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(tarballPath)
		return "", err
	}

	return tarballPath, nil
}

// readFileFromPackage extracts and reads a file from the cached package tarball.
func (nb *NpmBackend) readFileFromPackage(cached *cachedNpmPackage, filePath string) ([]byte, error) {
	cached.mu.Lock()
	defer cached.mu.Unlock()

	// Check if already extracted
	if content, ok := cached.files[filePath]; ok {
		return content, nil
	}

	// Open tarball
	f, err := os.Open(cached.tarballPath)
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

	// npm tarballs have a "package/" prefix in all paths
	targetPath := "package/" + strings.TrimPrefix(filePath, "/")

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

	return nil, fmt.Errorf("file not found in package: %s", filePath)
}

// cleanupLoop periodically removes old cached packages.
func (nb *NpmBackend) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			nb.cleanupOldPackages()
		}
	}
}

func (nb *NpmBackend) cleanupOldPackages() {
	nb.mu.Lock()
	defer nb.mu.Unlock()

	cutoff := time.Now().Add(-24 * time.Hour)

	for key, cached := range nb.packages {
		cached.mu.Lock()
		if cached.lastAccess.Before(cutoff) {
			// Remove from disk
			if cached.tarballPath != "" {
				dir := filepath.Dir(cached.tarballPath)
				os.RemoveAll(dir)
			}
			delete(nb.packages, key)
		}
		cached.mu.Unlock()
	}
}

// parseNpmContentID parses package@version/path/to/file format.
func parseNpmContentID(contentID string) (packageName, version, filePath string, err error) {
	// Format: package@version/path/to/file or @scope/package@version/path/to/file

	// Handle scoped packages (@scope/package@version)
	if strings.HasPrefix(contentID, "@") {
		// Find the second @ which separates package from version
		parts := strings.SplitN(contentID, "@", 3)
		if len(parts) < 3 {
			return "", "", "", fmt.Errorf("invalid npm content ID format: %s", contentID)
		}

		packageName = "@" + parts[1] // @scope/package
		rest := parts[2]

		// Split version and file path
		slashIdx := strings.Index(rest, "/")
		if slashIdx == -1 {
			version = rest
			filePath = ""
		} else {
			version = rest[:slashIdx]
			filePath = rest[slashIdx+1:]
		}
		return packageName, version, filePath, nil
	}

	// Regular packages (package@version)
	parts := strings.SplitN(contentID, "@", 2)
	if len(parts) != 2 {
		return "", "", "", fmt.Errorf("invalid npm content ID format: %s", contentID)
	}

	packageName = parts[0]
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

	return packageName, version, filePath, nil
}

// sanitizePackageName converts package name to safe directory name.
func sanitizePackageName(name string) string {
	// Replace slashes and @ with underscores for scoped packages
	s := strings.ReplaceAll(name, "/", "_")
	s = strings.ReplaceAll(s, "@", "_")
	return s
}

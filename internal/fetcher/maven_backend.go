package fetcher

import (
	"archive/zip"
	"context"
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

// MavenBackend fetches files from Maven Central repository.
type MavenBackend struct {
	BaseBackend
	config BackendConfig

	// HTTP client for Maven Central
	client *http.Client

	// Repository URL (default: repo1.maven.org/maven2)
	repoURL string

	mu        sync.RWMutex
	artifacts map[string]*cachedMavenArtifact // groupId:artifactId:version -> cached artifact
}

type cachedMavenArtifact struct {
	groupID    string
	artifactID string
	version    string
	jarPath    string            // Path to cached JAR file
	files      map[string][]byte // Extracted files (lazy loaded)
	lastAccess time.Time
	mu         sync.Mutex
}

// NewMavenBackend creates a new Maven Central backend.
func NewMavenBackend() *MavenBackend {
	mb := &MavenBackend{
		artifacts: make(map[string]*cachedMavenArtifact),
		client: &http.Client{
			Timeout: 5 * time.Minute,
		},
		repoURL: "https://repo1.maven.org/maven2",
	}
	mb.InitStats()
	return mb
}

func (mb *MavenBackend) Type() pb.SourceType {
	return pb.SourceType_SOURCE_TYPE_MAVEN
}

func (mb *MavenBackend) Initialize(ctx context.Context, config BackendConfig) error {
	mb.config = config

	// Override repository URL if configured
	if url, ok := config.Extra["repo_url"]; ok && url != "" {
		mb.repoURL = url
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(config.CacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create maven cache dir: %w", err)
	}

	// Scan existing artifacts in cache
	entries, err := os.ReadDir(config.CacheDir)
	if err != nil {
		return nil // Empty cache is fine
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Directory structure: artifact_hash/version/
		artifactPath := filepath.Join(config.CacheDir, entry.Name())
		versions, _ := os.ReadDir(artifactPath)
		for _, ver := range versions {
			if !ver.IsDir() {
				continue
			}
			jarPath := filepath.Join(artifactPath, ver.Name(), "artifact.jar")
			if _, err := os.Stat(jarPath); err == nil {
				// Found a cached artifact
				key := entry.Name() + ":" + ver.Name()
				mb.artifacts[key] = &cachedMavenArtifact{
					groupID:    "", // Will be populated on demand
					artifactID: entry.Name(),
					version:    ver.Name(),
					jarPath:    jarPath,
					files:      make(map[string][]byte),
					lastAccess: time.Now(),
				}
			}
		}
	}

	// Start cleanup goroutine
	go mb.cleanupLoop(ctx)

	return nil
}

func (mb *MavenBackend) FetchBlob(ctx context.Context, req *FetchRequest) (*FetchResult, error) {
	start := time.Now()

	// ContentID format: groupId:artifactId:version/path/to/file
	groupID, artifactID, version, filePath, err := parseMavenContentID(req.ContentID)
	if err != nil {
		// ContentID is likely a blob hash, not Maven coordinates
		// Get artifact info from source config
		coords := req.SourceConfig["coordinates"]
		if coords != "" {
			parts := strings.Split(coords, ":")
			if len(parts) >= 3 {
				groupID = parts[0]
				artifactID = parts[1]
				version = parts[2]
			}
		}

		if groupID == "" {
			groupID = req.SourceConfig["group_id"]
		}
		if artifactID == "" {
			artifactID = req.SourceConfig["artifact_id"]
		}
		if version == "" {
			version = req.SourceConfig["version"]
		}

		// Get file path from source config
		filePath = req.SourceConfig["file_path"]
		if filePath == "" {
			filePath = req.SourceConfig["display_path"]
		}
	}

	if groupID == "" || artifactID == "" || version == "" {
		mb.RecordError()
		return nil, fmt.Errorf("invalid maven request: missing groupId, artifactId, or version")
	}

	// Get or download artifact
	cached, err := mb.getOrDownloadArtifact(ctx, groupID, artifactID, version)
	if err != nil {
		mb.RecordError()
		return nil, fmt.Errorf("failed to get artifact: %w", err)
	}

	// Read file from artifact
	content, err := mb.readFileFromArtifact(cached, filePath)
	if err != nil {
		mb.RecordError()
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	latency := time.Since(start).Milliseconds()
	mb.RecordSuccess(int64(len(content)), latency, false, int64(len(mb.artifacts)))

	return &FetchResult{
		Content:   content,
		Size:      int64(len(content)),
		FromCache: false,
	}, nil
}

func (mb *MavenBackend) FetchBlobStream(ctx context.Context, req *FetchRequest) (io.ReadCloser, int64, error) {
	return DefaultFetchBlobStream(mb, ctx, req)
}

func (mb *MavenBackend) Warmup(ctx context.Context, sourceKey string, config map[string]string) error {
	coords := config["coordinates"]
	if coords == "" {
		return fmt.Errorf("warmup requires coordinates (groupId:artifactId:version)")
	}

	parts := strings.Split(coords, ":")
	if len(parts) < 3 {
		return fmt.Errorf("invalid coordinates format: %s", coords)
	}

	groupID := parts[0]
	artifactID := parts[1]
	version := parts[2]

	_, err := mb.getOrDownloadArtifact(ctx, groupID, artifactID, version)
	return err
}

func (mb *MavenBackend) CachedSources() []string {
	mb.mu.RLock()
	defer mb.mu.RUnlock()

	sources := make([]string, 0, len(mb.artifacts))
	for key := range mb.artifacts {
		sources = append(sources, key)
	}
	return sources
}

func (mb *MavenBackend) Cleanup(ctx context.Context, sourceKey string) error {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	cached, ok := mb.artifacts[sourceKey]
	if !ok {
		return nil
	}

	// Remove from memory
	delete(mb.artifacts, sourceKey)

	// Remove from disk
	if cached.jarPath != "" {
		dir := filepath.Dir(cached.jarPath)
		return os.RemoveAll(dir)
	}

	return nil
}

func (mb *MavenBackend) Close() error {
	return nil
}

// getOrDownloadArtifact retrieves an artifact from cache or downloads it.
func (mb *MavenBackend) getOrDownloadArtifact(ctx context.Context, groupID, artifactID, version string) (*cachedMavenArtifact, error) {
	key := groupID + ":" + artifactID + ":" + version

	// Check cache first
	mb.mu.RLock()
	cached, ok := mb.artifacts[key]
	mb.mu.RUnlock()

	if ok {
		cached.mu.Lock()
		cached.lastAccess = time.Now()
		cached.mu.Unlock()
		return cached, nil
	}

	// Download artifact
	mb.mu.Lock()
	defer mb.mu.Unlock()

	// Double-check (another goroutine may have downloaded it)
	if cached, ok := mb.artifacts[key]; ok {
		return cached, nil
	}

	// Download JAR
	jarPath, err := mb.downloadJar(ctx, groupID, artifactID, version)
	if err != nil {
		return nil, fmt.Errorf("failed to download JAR: %w", err)
	}

	cached = &cachedMavenArtifact{
		groupID:    groupID,
		artifactID: artifactID,
		version:    version,
		jarPath:    jarPath,
		files:      make(map[string][]byte),
		lastAccess: time.Now(),
	}

	mb.artifacts[key] = cached
	return cached, nil
}

// downloadJar downloads the JAR file from Maven Central.
func (mb *MavenBackend) downloadJar(ctx context.Context, groupID, artifactID, version string) (string, error) {
	// Maven Central URL format:
	// https://repo1.maven.org/maven2/group/path/artifactId/version/artifactId-version.jar
	// Example: https://repo1.maven.org/maven2/junit/junit/4.13.2/junit-4.13.2.jar

	groupPath := strings.ReplaceAll(groupID, ".", "/")
	jarURL := fmt.Sprintf("%s/%s/%s/%s/%s-%s.jar",
		mb.repoURL, groupPath, artifactID, version, artifactID, version)

	// Create cache directory: cache/maven/<artifact_hash>/<version>/
	artifactHash := sanitizeMavenCoords(groupID, artifactID)
	cacheDir := filepath.Join(mb.config.CacheDir, artifactHash, version)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", err
	}

	jarPath := filepath.Join(cacheDir, "artifact.jar")

	// Download JAR
	req, err := http.NewRequestWithContext(ctx, "GET", jarURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := mb.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download JAR: HTTP %d from %s", resp.StatusCode, jarURL)
	}

	// Save to file
	f, err := os.Create(jarPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(jarPath)
		return "", err
	}

	return jarPath, nil
}

// readFileFromArtifact extracts and reads a file from the cached JAR file.
func (mb *MavenBackend) readFileFromArtifact(cached *cachedMavenArtifact, filePath string) ([]byte, error) {
	cached.mu.Lock()
	defer cached.mu.Unlock()

	// Check if already extracted
	if content, ok := cached.files[filePath]; ok {
		return content, nil
	}

	// Open JAR (it's a ZIP file)
	zipReader, err := zip.OpenReader(cached.jarPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open JAR: %w", err)
	}
	defer zipReader.Close()

	// Clean file path
	targetPath := strings.TrimPrefix(filePath, "/")

	// Find file in JAR
	for _, file := range zipReader.File {
		if file.Name == targetPath {
			rc, err := file.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()

			content, err := io.ReadAll(rc)
			if err != nil {
				return nil, err
			}

			// Cache the extracted file
			cached.files[filePath] = content
			return content, nil
		}
	}

	return nil, fmt.Errorf("file not found in JAR: %s", filePath)
}

// cleanupLoop periodically removes old cached artifacts.
func (mb *MavenBackend) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mb.cleanupOldArtifacts()
		}
	}
}

func (mb *MavenBackend) cleanupOldArtifacts() {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	cutoff := time.Now().Add(-24 * time.Hour)

	for key, cached := range mb.artifacts {
		cached.mu.Lock()
		if cached.lastAccess.Before(cutoff) {
			// Remove from disk
			if cached.jarPath != "" {
				dir := filepath.Dir(cached.jarPath)
				os.RemoveAll(dir)
			}
			delete(mb.artifacts, key)
		}
		cached.mu.Unlock()
	}
}

// parseMavenContentID parses groupId:artifactId:version/path/to/file format.
func parseMavenContentID(contentID string) (groupID, artifactID, version, filePath string, err error) {
	// Format: groupId:artifactId:version/path/to/file
	// Example: junit:junit:4.13.2/org/junit/Test.class

	// Split by first slash to separate coordinates from file path
	slashIdx := strings.Index(contentID, "/")
	var coords string
	if slashIdx == -1 {
		coords = contentID
		filePath = ""
	} else {
		coords = contentID[:slashIdx]
		filePath = contentID[slashIdx+1:]
	}

	// Parse coordinates
	parts := strings.Split(coords, ":")
	if len(parts) < 3 {
		return "", "", "", "", fmt.Errorf("invalid maven content ID format: %s", contentID)
	}

	groupID = parts[0]
	artifactID = parts[1]
	version = parts[2]

	return groupID, artifactID, version, filePath, nil
}

// sanitizeMavenCoords converts Maven coordinates to safe directory name.
func sanitizeMavenCoords(groupID, artifactID string) string {
	// Combine groupId and artifactId into a safe directory name
	s := groupID + "_" + artifactID
	s = strings.ReplaceAll(s, ".", "_")
	s = strings.ReplaceAll(s, "/", "_")
	return s
}

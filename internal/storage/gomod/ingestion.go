package gomod

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/radryc/monofs/internal/storage"
)

// GoModIngestionBackend implements IngestionBackend for Go modules
type GoModIngestionBackend struct {
	moduleURL    string // Format: module@version
	modulePath   string // e.g., github.com/user/repo
	version      string // e.g., v1.2.3
	proxy        string
	cacheDir     string
	zipPath      string
	client       *http.Client
	modulePrefix string // Prefix within zip (e.g., "module@version/")
}

// ModuleInfo represents Go module metadata from proxy
type ModuleInfo struct {
	Version string    `json:"Version"`
	Time    time.Time `json:"Time"`
}

// NewGoModIngestionBackend creates a new Go module ingestion backend
func NewGoModIngestionBackend() storage.IngestionBackend {
	return &GoModIngestionBackend{}
}

func (g *GoModIngestionBackend) Type() storage.IngestionType {
	return storage.IngestionTypeGo
}

func (g *GoModIngestionBackend) Initialize(ctx context.Context, sourceURL string, config map[string]string) error {
	g.moduleURL = sourceURL

	// Parse module@version format
	parts := strings.Split(sourceURL, "@")
	if len(parts) != 2 {
		return fmt.Errorf("invalid module URL format, expected module@version: %s", sourceURL)
	}
	g.modulePath = parts[0]
	g.version = parts[1]

	// Use GOPROXY environment variable or default
	g.proxy = config["proxy"]
	if g.proxy == "" {
		g.proxy = os.Getenv("GOPROXY")
		if g.proxy == "" {
			g.proxy = "https://proxy.golang.org"
		}
	}

	// Create HTTP client
	g.client = &http.Client{
		Timeout: 60 * time.Second,
	}

	// Setup cache directory
	cacheDir := config["cache_dir"]
	if cacheDir == "" {
		cacheDir = "/tmp/monofs-gomod-cache"
	}
	g.cacheDir = filepath.Join(cacheDir, g.modulePath+"@"+g.version)

	if err := os.MkdirAll(g.cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Download module zip if not already cached
	g.zipPath = filepath.Join(g.cacheDir, "module.zip")
	if _, err := os.Stat(g.zipPath); os.IsNotExist(err) {
		if err := g.downloadModule(ctx); err != nil {
			return fmt.Errorf("failed to download module: %w", err)
		}
	}

	// The module zip contains files with prefix "module@version/"
	g.modulePrefix = g.modulePath + "@" + g.version + "/"

	return nil
}

func (g *GoModIngestionBackend) downloadModule(ctx context.Context) error {
	// Build Go proxy URL for module zip
	// Format: https://proxy.golang.org/module/@v/version.zip
	proxyURL := fmt.Sprintf("%s/%s/@v/%s.zip", g.proxy, g.modulePath, g.version)

	req, err := http.NewRequestWithContext(ctx, "GET", proxyURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch module: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch module: HTTP %d", resp.StatusCode)
	}

	// Write to temporary file first
	tmpPath := g.zipPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("failed to create zip file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to write zip file: %w", err)
	}

	// Rename to final path
	if err := os.Rename(tmpPath, g.zipPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename zip file: %w", err)
	}

	return nil
}

func (g *GoModIngestionBackend) Validate(ctx context.Context, sourceURL string, config map[string]string) error {
	// Parse module@version
	parts := strings.Split(sourceURL, "@")
	if len(parts) != 2 {
		return fmt.Errorf("invalid module URL format, expected module@version")
	}
	modulePath := parts[0]
	version := parts[1]

	// Get proxy URL
	proxy := config["proxy"]
	if proxy == "" {
		proxy = os.Getenv("GOPROXY")
		if proxy == "" {
			proxy = "https://proxy.golang.org"
		}
	}

	// Check if module version exists
	// Format: https://proxy.golang.org/module/@v/version.info
	infoURL := fmt.Sprintf("%s/%s/@v/%s.info", proxy, modulePath, version)

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", infoURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to validate module: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("module not found: %s@%s", modulePath, version)
	}

	// Parse and validate info
	var info ModuleInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return fmt.Errorf("invalid module info: %w", err)
	}

	if info.Version != version {
		return fmt.Errorf("version mismatch: expected %s, got %s", version, info.Version)
	}

	return nil
}

func (g *GoModIngestionBackend) WalkFiles(ctx context.Context, fn func(storage.FileMetadata) error) error {
	if g.zipPath == "" {
		return fmt.Errorf("backend not initialized")
	}

	// Open zip file
	reader, err := zip.OpenReader(g.zipPath)
	if err != nil {
		return fmt.Errorf("failed to open module zip: %w", err)
	}
	defer reader.Close()

	// Walk through all files in zip
	for _, file := range reader.File {
		// Skip directories
		if file.FileInfo().IsDir() {
			continue
		}

		// Remove module prefix from path
		relPath := file.Name
		relPath = strings.TrimPrefix(relPath, g.modulePrefix)

		// Skip if path is empty after removing prefix
		if relPath == "" {
			continue
		}

		// Compute content hash by reading file
		rc, err := file.Open()
		if err != nil {
			return fmt.Errorf("failed to open file in zip: %w", err)
		}

		hasher := sha256.New()
		size, err := io.Copy(hasher, rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("failed to read file content: %w", err)
		}

		contentHash := hex.EncodeToString(hasher.Sum(nil))

		meta := storage.FileMetadata{
			Path:        relPath,
			Size:        uint64(size),
			Mode:        uint32(file.Mode()),
			ModTime:     file.Modified.Unix(),
			ContentHash: contentHash,
			Metadata: map[string]string{
				"module":  g.modulePath,
				"version": g.version,
			},
		}

		if err := fn(meta); err != nil {
			return err
		}
	}

	return nil
}

func (g *GoModIngestionBackend) GetMetadata(ctx context.Context, filePath string) (*storage.FileMetadata, error) {
	if g.zipPath == "" {
		return nil, fmt.Errorf("backend not initialized")
	}

	// Open zip file
	reader, err := zip.OpenReader(g.zipPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open module zip: %w", err)
	}
	defer reader.Close()

	// Look for the file with module prefix
	targetPath := path.Join(g.modulePrefix, filePath)

	for _, file := range reader.File {
		if file.Name == targetPath {
			// Compute content hash
			rc, err := file.Open()
			if err != nil {
				return nil, fmt.Errorf("failed to open file in zip: %w", err)
			}

			hasher := sha256.New()
			size, err := io.Copy(hasher, rc)
			rc.Close()
			if err != nil {
				return nil, fmt.Errorf("failed to read file content: %w", err)
			}

			contentHash := hex.EncodeToString(hasher.Sum(nil))

			return &storage.FileMetadata{
				Path:        filePath,
				Size:        uint64(size),
				Mode:        uint32(file.Mode()),
				ModTime:     file.Modified.Unix(),
				ContentHash: contentHash,
				Metadata: map[string]string{
					"module":  g.modulePath,
					"version": g.version,
				},
			}, nil
		}
	}

	return nil, fmt.Errorf("file not found: %s", filePath)
}

func (g *GoModIngestionBackend) Cleanup() error {
	// Optionally remove cache directory
	// For now, we keep it for reuse
	return nil
}

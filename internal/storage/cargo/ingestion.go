package cargo

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/radryc/monofs/internal/storage"
)

// CargoIngestionBackend implements IngestionBackend for Cargo crates
type CargoIngestionBackend struct {
	crateName     string
	version       string
	registry      string
	cacheDir      string
	extractDir    string
	client        *http.Client
	repositoryURL string // Source repository URL
}

// NewCargoIngestionBackend creates a new Cargo ingestion backend
func NewCargoIngestionBackend() storage.IngestionBackend {
	return &CargoIngestionBackend{}
}

func (c *CargoIngestionBackend) Type() storage.IngestionType {
	return storage.IngestionTypeCargo
}

func (c *CargoIngestionBackend) Initialize(ctx context.Context, sourceURL string, config map[string]string) error {
	// Parse crate@version format: serde@1.0.188
	c.crateName = sourceURL
	c.version = config["version"]

	if strings.Contains(sourceURL, "@") {
		parts := strings.Split(sourceURL, "@")
		if len(parts) == 2 {
			c.crateName = parts[0]
			if c.version == "" {
				c.version = parts[1]
			}
		}
	}

	if c.version == "" {
		return fmt.Errorf("version required for Cargo crate: %s", sourceURL)
	}

	// Setup registry
	c.registry = config["registry"]
	if c.registry == "" {
		c.registry = "https://crates.io"
	}

	// Create HTTP client
	c.client = &http.Client{
		Timeout: 60 * time.Second,
	}

	// Setup cache directory
	cacheDir := config["cache_dir"]
	if cacheDir == "" {
		cacheDir = "/tmp/monofs-cargo-cache"
	}
	c.cacheDir = filepath.Join(cacheDir, c.crateName+"@"+c.version)

	if err := os.MkdirAll(c.cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	c.extractDir = filepath.Join(c.cacheDir, "extracted")

	// Download and extract if not cached
	if _, err := os.Stat(c.extractDir); os.IsNotExist(err) {
		if err := c.downloadAndExtract(ctx); err != nil {
			return fmt.Errorf("failed to download crate: %w", err)
		}
	}

	return nil
}

func (c *CargoIngestionBackend) downloadAndExtract(ctx context.Context) error {
	// First, get crate metadata to extract repository URL
	metadataURL := fmt.Sprintf("%s/api/v1/crates/%s/%s",
		c.registry,
		c.crateName,
		c.version,
	)

	metaReq, err := http.NewRequestWithContext(ctx, "GET", metadataURL, nil)
	if err != nil {
		return err
	}
	metaReq.Header.Set("User-Agent", "monofs/0.1.0")

	metaResp, err := c.client.Do(metaReq)
	if err == nil && metaResp.StatusCode == http.StatusOK {
		var metadata struct {
			Version struct {
				Repository string `json:"repository"`
			} `json:"version"`
		}
		if err := json.NewDecoder(metaResp.Body).Decode(&metadata); err == nil {
			c.repositoryURL = metadata.Version.Repository
		}
		metaResp.Body.Close()
	}

	// Build download URL for crates.io
	downloadURL := fmt.Sprintf("%s/api/v1/crates/%s/%s/download",
		c.registry,
		c.crateName,
		c.version,
	)

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return err
	}

	// crates.io requires a User-Agent header
	req.Header.Set("User-Agent", "monofs/0.1.0")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("download crate: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("crates.io returned status %d", resp.StatusCode)
	}

	// Extract the .crate file (it's a gzipped tarball)
	if err := c.extractCrate(resp.Body); err != nil {
		return fmt.Errorf("extract crate: %w", err)
	}

	return nil
}

func (c *CargoIngestionBackend) extractCrate(reader io.Reader) error {
	gzr, err := gzip.NewReader(reader)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Cargo crates have "<crate>-<version>/" prefix
		target := header.Name
		prefix := fmt.Sprintf("%s-%s/", c.crateName, c.version)
		target = strings.TrimPrefix(target, prefix)

		target = filepath.Join(c.extractDir, target)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			dir := filepath.Dir(target)
			if err := os.MkdirAll(dir, 0755); err != nil {
				return err
			}

			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return err
			}

			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}

	return nil
}

func (c *CargoIngestionBackend) Validate(ctx context.Context, sourceURL string, config map[string]string) error {
	// Basic validation
	return nil
}

// skipCargoDirs are directories that should not be walked during cargo ingestion.
var skipCargoDirs = map[string]bool{
	"target": true, ".git": true, ".hg": true, ".svn": true, "node_modules": true,
}

func (c *CargoIngestionBackend) WalkFiles(ctx context.Context, fn func(storage.FileMetadata) error) error {
	// Use WalkDir instead of Walk to avoid following symlinks
	return filepath.WalkDir(c.extractDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip symlinks entirely to prevent cycles
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		if d.IsDir() {
			if skipCargoDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		relPath, err := filepath.Rel(c.extractDir, path)
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return nil // skip files we can't stat
		}

		contentHash, err := computeFileHash(path)
		if err != nil {
			return err
		}

		return fn(storage.FileMetadata{
			Path:        relPath,
			Size:        uint64(info.Size()),
			Mode:        uint32(info.Mode()),
			ModTime:     info.ModTime().Unix(),
			ContentHash: contentHash,
			Metadata: map[string]string{
				"crate_name":     c.crateName,
				"version":        c.version,
				"repository_url": c.repositoryURL,
			},
		})
	})
}

func (c *CargoIngestionBackend) GetMetadata(ctx context.Context, filePath string) (*storage.FileMetadata, error) {
	fullPath := filepath.Join(c.extractDir, filePath)

	info, err := os.Stat(fullPath)
	if err != nil {
		return nil, err
	}

	contentHash, err := computeFileHash(fullPath)
	if err != nil {
		return nil, err
	}

	return &storage.FileMetadata{
		Path:        filePath,
		Size:        uint64(info.Size()),
		Mode:        uint32(info.Mode()),
		ModTime:     info.ModTime().Unix(),
		ContentHash: contentHash,
		Metadata: map[string]string{
			"crate_name":     c.crateName,
			"version":        c.version,
			"repository_url": c.repositoryURL,
		},
	}, nil
}

func (c *CargoIngestionBackend) Cleanup() error {
	if c.cacheDir != "" {
		return os.RemoveAll(c.cacheDir)
	}
	return nil
}

func computeFileHash(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

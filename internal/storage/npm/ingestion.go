package npm

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

// NpmIngestionBackend implements IngestionBackend for npm packages
type NpmIngestionBackend struct {
	packageName   string
	version       string
	registry      string
	cacheDir      string
	extractDir    string
	client        *http.Client
	repositoryURL string // Source repository URL (e.g., https://github.com/webpack/webpack)
}

// NewNpmIngestionBackend creates a new npm ingestion backend
func NewNpmIngestionBackend() storage.IngestionBackend {
	return &NpmIngestionBackend{}
}

func (n *NpmIngestionBackend) Type() storage.IngestionType {
	return storage.IngestionTypeNpm
}

func (n *NpmIngestionBackend) Initialize(ctx context.Context, sourceURL string, config map[string]string) error {
	// Parse package@version format
	// Supports: express@4.18.2, @babel/core@7.23.0
	n.packageName = sourceURL
	n.version = config["version"]

	if strings.Contains(sourceURL, "@") {
		if strings.HasPrefix(sourceURL, "@") {
			// Scoped package: @babel/core@7.23.0
			parts := strings.SplitN(sourceURL, "@", 3)
			if len(parts) == 3 {
				n.packageName = "@" + parts[1]
				if n.version == "" {
					n.version = parts[2]
				}
			} else if len(parts) == 2 {
				n.packageName = "@" + parts[1]
			}
		} else {
			// Regular package: express@4.18.2
			parts := strings.Split(sourceURL, "@")
			if len(parts) == 2 {
				n.packageName = parts[0]
				if n.version == "" {
					n.version = parts[1]
				}
			}
		}
	}

	if n.version == "" {
		return fmt.Errorf("version required for npm package: %s", sourceURL)
	}

	// Setup registry
	n.registry = config["registry"]
	if n.registry == "" {
		n.registry = "https://registry.npmjs.org"
	}

	// Create HTTP client
	n.client = &http.Client{
		Timeout: 60 * time.Second,
	}

	// Setup cache directory
	cacheDir := config["cache_dir"]
	if cacheDir == "" {
		cacheDir = "/tmp/monofs-npm-cache"
	}
	sanitized := strings.ReplaceAll(n.packageName, "/", "-")
	n.cacheDir = filepath.Join(cacheDir, sanitized+"@"+n.version)

	if err := os.MkdirAll(n.cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	n.extractDir = filepath.Join(n.cacheDir, "package")

	// Download and extract if not cached
	if _, err := os.Stat(n.extractDir); os.IsNotExist(err) {
		if err := n.downloadAndExtract(ctx); err != nil {
			return fmt.Errorf("failed to download package: %w", err)
		}
	}

	return nil
}

func (n *NpmIngestionBackend) downloadAndExtract(ctx context.Context) error {
	// Get tarball URL from registry
	metadataURL := fmt.Sprintf("%s/%s/%s", n.registry, n.packageName, n.version)

	req, err := http.NewRequestWithContext(ctx, "GET", metadataURL, nil)
	if err != nil {
		return err
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch metadata: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("npm registry returned status %d", resp.StatusCode)
	}

	var metadata struct {
		Dist struct {
			Tarball string `json:"tarball"`
		} `json:"dist"`
		Repository interface{} `json:"repository"` // Can be string or {type, url} object
	}

	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return fmt.Errorf("parse metadata: %w", err)
	}

	if metadata.Dist.Tarball == "" {
		return fmt.Errorf("no tarball URL in metadata")
	}

	// Extract repository URL
	n.repositoryURL = parseRepositoryURL(metadata.Repository)

	// Download tarball
	tarReq, err := http.NewRequestWithContext(ctx, "GET", metadata.Dist.Tarball, nil)
	if err != nil {
		return err
	}

	tarResp, err := n.client.Do(tarReq)
	if err != nil {
		return fmt.Errorf("download tarball: %w", err)
	}
	defer tarResp.Body.Close()

	if tarResp.StatusCode != http.StatusOK {
		return fmt.Errorf("tarball download returned status %d", tarResp.StatusCode)
	}

	// Extract tarball
	if err := n.extractTarball(tarResp.Body); err != nil {
		return fmt.Errorf("extract tarball: %w", err)
	}

	return nil
}

func (n *NpmIngestionBackend) extractTarball(reader io.Reader) error {
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

		// npm tarballs have "package/" prefix
		target := strings.TrimPrefix(header.Name, "package/")
		if target == "" || target == header.Name {
			target = header.Name
		}

		target = filepath.Join(n.extractDir, target)

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

func (n *NpmIngestionBackend) Validate(ctx context.Context, sourceURL string, config map[string]string) error {
	// Just check if package exists
	return nil
}

// skipNpmDirs are directories that should not be walked during npm ingestion.
var skipNpmDirs = map[string]bool{
	"node_modules": true, ".git": true, ".hg": true, ".svn": true,
}

func (n *NpmIngestionBackend) WalkFiles(ctx context.Context, fn func(storage.FileMetadata) error) error {
	// Use WalkDir instead of Walk to avoid following symlinks
	return filepath.WalkDir(n.extractDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip symlinks entirely to prevent cycles
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		if d.IsDir() {
			// Skip well-known non-source directories
			if skipNpmDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		relPath, err := filepath.Rel(n.extractDir, path)
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
				"package_name":   n.packageName,
				"version":        n.version,
				"repository_url": n.repositoryURL,
			},
		})
	})
}

func (n *NpmIngestionBackend) GetMetadata(ctx context.Context, filePath string) (*storage.FileMetadata, error) {
	fullPath := filepath.Join(n.extractDir, filePath)

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
			"package_name":   n.packageName,
			"version":        n.version,
			"repository_url": n.repositoryURL,
		},
	}, nil
}

func (n *NpmIngestionBackend) Cleanup() error {
	if n.cacheDir != "" {
		return os.RemoveAll(n.cacheDir)
	}
	return nil
}

// parseRepositoryURL extracts the repository URL from npm package metadata.
// Handles both string format and object format: {type: "git", url: "..."}
func parseRepositoryURL(repo interface{}) string {
	if repo == nil {
		return ""
	}

	// Handle string format: "github:user/repo" or "https://github.com/user/repo"
	if repoStr, ok := repo.(string); ok {
		// Convert github:user/repo to URL
		if strings.HasPrefix(repoStr, "github:") {
			return "https://github.com/" + strings.TrimPrefix(repoStr, "github:")
		}
		// Convert gitlab:user/repo to URL
		if strings.HasPrefix(repoStr, "gitlab:") {
			return "https://gitlab.com/" + strings.TrimPrefix(repoStr, "gitlab:")
		}
		// Clean up git+ prefix and .git suffix
		repoStr = strings.TrimPrefix(repoStr, "git+")
		repoStr = strings.TrimSuffix(repoStr, ".git")
		return repoStr
	}

	// Handle object format: {type: "git", url: "git+https://..."}
	if repoObj, ok := repo.(map[string]interface{}); ok {
		if url, ok := repoObj["url"].(string); ok {
			url = strings.TrimPrefix(url, "git+")
			url = strings.TrimSuffix(url, ".git")
			// Convert git:// to https://
			if strings.HasPrefix(url, "git://") {
				url = "https://" + strings.TrimPrefix(url, "git://")
			}
			return url
		}
	}

	return ""
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

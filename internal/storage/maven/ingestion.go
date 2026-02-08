package maven

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/radryc/monofs/internal/storage"
)

// MavenIngestionBackend implements IngestionBackend for Maven artifacts
type MavenIngestionBackend struct {
	groupID    string
	artifactID string
	version    string
	repository string
	cacheDir   string
	extractDir string
	client     *http.Client
}

// NewMavenIngestionBackend creates a new Maven ingestion backend
func NewMavenIngestionBackend() storage.IngestionBackend {
	return &MavenIngestionBackend{}
}

func (m *MavenIngestionBackend) Type() storage.IngestionType {
	return storage.IngestionTypeMaven
}

func (m *MavenIngestionBackend) Initialize(ctx context.Context, sourceURL string, config map[string]string) error {
	// Parse Maven coordinates: groupId:artifactId:version
	// Example: org.springframework.boot:spring-boot-starter-web:3.1.0
	parts := strings.Split(sourceURL, ":")
	if len(parts) != 3 {
		return fmt.Errorf("invalid Maven coordinates format: %s (expected groupId:artifactId:version)", sourceURL)
	}

	m.groupID = parts[0]
	m.artifactID = parts[1]
	m.version = parts[2]

	// Setup repository
	m.repository = config["repository"]
	if m.repository == "" {
		m.repository = "https://repo1.maven.org/maven2"
	}

	// Create HTTP client
	m.client = &http.Client{
		Timeout: 120 * time.Second,
	}

	// Setup cache directory
	cacheDir := config["cache_dir"]
	if cacheDir == "" {
		cacheDir = "/tmp/monofs-maven-cache"
	}
	sanitized := strings.ReplaceAll(fmt.Sprintf("%s-%s-%s", m.groupID, m.artifactID, m.version), ":", "-")
	m.cacheDir = filepath.Join(cacheDir, sanitized)

	if err := os.MkdirAll(m.cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	m.extractDir = filepath.Join(m.cacheDir, "extracted")

	// Download and extract if not cached
	if _, err := os.Stat(m.extractDir); os.IsNotExist(err) {
		if err := m.downloadAndExtract(ctx); err != nil {
			return fmt.Errorf("failed to download artifact: %w", err)
		}
	}

	return nil
}

func (m *MavenIngestionBackend) downloadAndExtract(ctx context.Context) error {
	// Convert groupId to path: org.springframework.boot -> org/springframework/boot
	groupPath := strings.ReplaceAll(m.groupID, ".", "/")

	// Build JAR URL
	jarURL := fmt.Sprintf("%s/%s/%s/%s/%s-%s.jar",
		m.repository,
		groupPath,
		m.artifactID,
		m.version,
		m.artifactID,
		m.version,
	)

	// Download JAR file
	jarPath := filepath.Join(m.cacheDir, fmt.Sprintf("%s-%s.jar", m.artifactID, m.version))

	req, err := http.NewRequestWithContext(ctx, "GET", jarURL, nil)
	if err != nil {
		return err
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("download JAR: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Maven repository returned status %d for %s", resp.StatusCode, jarURL)
	}

	// Save JAR file
	jarFile, err := os.Create(jarPath)
	if err != nil {
		return err
	}

	if _, err := io.Copy(jarFile, resp.Body); err != nil {
		jarFile.Close()
		return err
	}
	jarFile.Close()

	// Extract JAR (it's a ZIP file)
	if err := m.extractJar(jarPath); err != nil {
		return fmt.Errorf("extract JAR: %w", err)
	}

	return nil
}

func (m *MavenIngestionBackend) extractJar(jarPath string) error {
	r, err := zip.OpenReader(jarPath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		targetPath := filepath.Join(m.extractDir, f.Name)

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return err
			}
			continue
		}

		dir := filepath.Dir(targetPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}

		outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}

		if _, err := io.Copy(outFile, rc); err != nil {
			rc.Close()
			outFile.Close()
			return err
		}

		rc.Close()
		outFile.Close()
	}

	return nil
}

func (m *MavenIngestionBackend) Validate(ctx context.Context, sourceURL string, config map[string]string) error {
	// Basic validation
	parts := strings.Split(sourceURL, ":")
	if len(parts) != 3 {
		return fmt.Errorf("invalid Maven coordinates format: %s", sourceURL)
	}
	return nil
}

func (m *MavenIngestionBackend) WalkFiles(ctx context.Context, fn func(storage.FileMetadata) error) error {
	return filepath.Walk(m.extractDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(m.extractDir, path)
		if err != nil {
			return err
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
				"group_id":    m.groupID,
				"artifact_id": m.artifactID,
				"version":     m.version,
			},
		})
	})
}

func (m *MavenIngestionBackend) GetMetadata(ctx context.Context, filePath string) (*storage.FileMetadata, error) {
	fullPath := filepath.Join(m.extractDir, filePath)

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
			"group_id":    m.groupID,
			"artifact_id": m.artifactID,
			"version":     m.version,
		},
	}, nil
}

func (m *MavenIngestionBackend) Cleanup() error {
	if m.cacheDir != "" {
		return os.RemoveAll(m.cacheDir)
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

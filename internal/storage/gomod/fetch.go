package gomod

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/radryc/monofs/internal/storage"
)

// GoModFetchBackend implements FetchBackend for Go modules
type GoModFetchBackend struct {
	cacheDir string
	proxy    string
	client   *http.Client
}

// NewGoModFetchBackend creates a new Go module fetch backend
func NewGoModFetchBackend() storage.FetchBackend {
	return &GoModFetchBackend{}
}

func (g *GoModFetchBackend) Type() storage.FetchType {
	return storage.FetchType("gomod")
}

func (g *GoModFetchBackend) Initialize(ctx context.Context, config map[string]string) error {
	cacheDir := config["cache_dir"]
	if cacheDir == "" {
		return fmt.Errorf("cache_dir config required")
	}
	g.cacheDir = cacheDir

	// Use GOPROXY environment variable or default
	g.proxy = config["proxy"]
	if g.proxy == "" {
		g.proxy = os.Getenv("GOPROXY")
		if g.proxy == "" {
			g.proxy = "https://proxy.golang.org"
		}
	}

	// Create HTTP client with reasonable timeouts
	g.client = &http.Client{
		Timeout: 30 * time.Second,
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(g.cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	return nil
}

func (g *GoModFetchBackend) FetchBlob(ctx context.Context, blobID string) ([]byte, error) {
	if g.client == nil {
		return nil, fmt.Errorf("backend not initialized")
	}

	// Check cache first
	cachePath := filepath.Join(g.cacheDir, blobID)
	if data, err := os.ReadFile(cachePath); err == nil {
		return data, nil
	}

	// Parse blobID format: module@version/path
	// For now, we'll use a simple implementation
	// In production, you'd want to properly parse and fetch from GOPROXY

	// Placeholder: In a real implementation, you would:
	// 1. Parse the blobID to extract module path, version, and file path
	// 2. Fetch the module zip from proxy
	// 3. Extract the specific file
	// 4. Cache it

	return nil, fmt.Errorf("blob not found: %s", blobID)
}

func (g *GoModFetchBackend) FetchBlobStream(ctx context.Context, blobID string) (io.ReadCloser, error) {
	// For Go modules, we read into memory first for simplicity
	// Large files are rare in Go modules
	data, err := g.FetchBlob(ctx, blobID)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (g *GoModFetchBackend) StoreBlob(ctx context.Context, data []byte) (string, error) {
	// Compute SHA256 hash as blob ID
	hash := sha256.Sum256(data)
	blobID := hex.EncodeToString(hash[:])

	// Store in cache
	cachePath := filepath.Join(g.cacheDir, blobID)
	if err := os.WriteFile(cachePath, data, 0644); err != nil {
		return "", fmt.Errorf("failed to store blob: %w", err)
	}

	return blobID, nil
}

func (g *GoModFetchBackend) StoreBlobStream(ctx context.Context, reader io.Reader) (string, error) {
	// Read all data and compute hash
	var buf bytes.Buffer
	hasher := sha256.New()

	tee := io.TeeReader(reader, hasher)
	if _, err := io.Copy(&buf, tee); err != nil {
		return "", fmt.Errorf("failed to read stream: %w", err)
	}

	blobID := hex.EncodeToString(hasher.Sum(nil))

	// Store in cache
	cachePath := filepath.Join(g.cacheDir, blobID)
	if err := os.WriteFile(cachePath, buf.Bytes(), 0644); err != nil {
		return "", fmt.Errorf("failed to store blob: %w", err)
	}

	return blobID, nil
}

func (g *GoModFetchBackend) Cleanup() error {
	// No persistent resources to cleanup
	return nil
}

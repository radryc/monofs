package logengine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	chunkListCacheTTL = 10 * time.Second
	manifestCacheTTL  = time.Minute
	cacheSweepInterval = 30 * time.Second
)

type chunkListCacheEntry struct {
	chunkIDs  []string
	expiresAt time.Time
}

type manifestCacheEntry struct {
	manifest  ChunkManifest
	expiresAt time.Time
}

// ErrGhostChunk is returned when a chunk is requested but not found in the remote storage.
var ErrGhostChunk = errors.New("ghost chunk: not found in storage")

// ObjectStoreBackend defines the interface for underlying chunk storage (S3/Local).
type ObjectStoreBackend interface {
	Write(ctx context.Context, path string, reader io.Reader) error
	// Read should return ErrGhostChunk if the file does not exist (e.g. S3 NoSuchKey).
	Read(ctx context.Context, path string) (io.ReadSeekCloser, error)
	ListChunks(ctx context.Context, prefix string) ([]string, error)
}

// CachedStore implements a caching decorator around a remote ObjectStoreBackend.
// It caches index files to a local NVMe drive and uses singleflight to
// prevent cache stampedes.
type CachedStore struct {
	remote    ObjectStoreBackend
	localDir  string
	sf        singleflight.Group
	mu        sync.RWMutex
	chunks    map[string]chunkListCacheEntry
	manifests map[string]manifestCacheEntry
	nextSweep time.Time
}

// NewCachedStore creates a new CachedStore.
func NewCachedStore(remote ObjectStoreBackend, localDir string) *CachedStore {
	return &CachedStore{
		remote:    remote,
		localDir:  localDir,
		chunks:    make(map[string]chunkListCacheEntry),
		manifests: make(map[string]manifestCacheEntry),
		nextSweep: time.Now().Add(cacheSweepInterval),
	}
}

func (c *CachedStore) pruneExpiredLocked(now time.Time) {
	if !c.nextSweep.IsZero() && now.Before(c.nextSweep) {
		return
	}
	for prefix, entry := range c.chunks {
		if !now.Before(entry.expiresAt) {
			delete(c.chunks, prefix)
		}
	}
	for path, entry := range c.manifests {
		if !now.Before(entry.expiresAt) {
			delete(c.manifests, path)
		}
	}
	c.nextSweep = now.Add(cacheSweepInterval)
}

// Write passes through to the remote storage.
func (c *CachedStore) Write(ctx context.Context, path string, reader io.Reader) error {
	if err := c.remote.Write(ctx, path, reader); err != nil {
		return err
	}
	c.invalidatePath(path)
	return nil
}

// ListChunks passes through to the remote storage.
func (c *CachedStore) ListChunks(ctx context.Context, prefix string) ([]string, error) {
	normalizedPrefix := normalizeChunkPrefix(prefix)
	if normalizedPrefix == "" {
		return c.remote.ListChunks(ctx, prefix)
	}

	now := time.Now()
	c.mu.RLock()
	entry, ok := c.chunks[normalizedPrefix]
	c.mu.RUnlock()
	if ok && now.Before(entry.expiresAt) {
		return append([]string(nil), entry.chunkIDs...), nil
	}
	if ok {
		c.mu.Lock()
		if staleEntry, staleOK := c.chunks[normalizedPrefix]; staleOK && !now.Before(staleEntry.expiresAt) {
			delete(c.chunks, normalizedPrefix)
		}
		c.mu.Unlock()
	}

	chunkIDs, err := c.remote.ListChunks(ctx, normalizedPrefix)
	if err != nil {
		return nil, err
	}
	sort.Strings(chunkIDs)

	c.mu.Lock()
	c.pruneExpiredLocked(now)
	c.chunks[normalizedPrefix] = chunkListCacheEntry{
		chunkIDs:  append([]string(nil), chunkIDs...),
		expiresAt: now.Add(chunkListCacheTTL),
	}
	c.mu.Unlock()

	return chunkIDs, nil
}

// ReadManifest returns a cached chunk manifest when available.
func (c *CachedStore) ReadManifest(ctx context.Context, signal Signal, chunkID string) (ChunkManifest, error) {
	manifestPath := filepath.Join("chunks", string(signal), chunkID, "metadata.json")
	now := time.Now()

	c.mu.RLock()
	entry, ok := c.manifests[manifestPath]
	c.mu.RUnlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.manifest, nil
	}
	if ok {
		c.mu.Lock()
		if staleEntry, staleOK := c.manifests[manifestPath]; staleOK && !now.Before(staleEntry.expiresAt) {
			delete(c.manifests, manifestPath)
		}
		c.mu.Unlock()
	}

	rc, err := c.remote.Read(ctx, manifestPath)
	if err != nil {
		return ChunkManifest{}, err
	}
	defer rc.Close()

	var manifest ChunkManifest
	if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
		return ChunkManifest{}, err
	}

	c.mu.Lock()
	c.pruneExpiredLocked(now)
	c.manifests[manifestPath] = manifestCacheEntry{
		manifest:  manifest,
		expiresAt: now.Add(manifestCacheTTL),
	}
	c.mu.Unlock()

	return manifest, nil
}

func normalizeChunkPrefix(prefix string) string {
	cleaned := strings.Trim(strings.TrimSpace(prefix), "/")
	if cleaned == "" {
		return ""
	}
	parts := strings.Split(cleaned, "/")
	if len(parts) >= 2 && parts[0] == "chunks" {
		return filepath.Join(parts[0], parts[1])
	}
	return cleaned
}

func (c *CachedStore) invalidatePath(path string) {
	cleaned := strings.Trim(strings.TrimSpace(path), "/")
	if cleaned == "" {
		return
	}

	parts := strings.Split(cleaned, "/")
	if len(parts) < 3 || parts[0] != "chunks" {
		return
	}

	prefix := filepath.Join(parts[0], parts[1])
	c.mu.Lock()
	delete(c.chunks, prefix)
	if strings.HasSuffix(cleaned, "/metadata.json") {
		delete(c.manifests, cleaned)
	}
	c.mu.Unlock()
}

// Read handles caching for index files and pass-through for Parquet/metadata.
func (c *CachedStore) Read(ctx context.Context, path string) (io.ReadSeekCloser, error) {
	// Only cache index files
	if !strings.HasSuffix(path, ".index.tar.gz") {
		return c.remote.Read(ctx, path)
	}

	localPath := filepath.Join(c.localDir, path)

	// Singleflight ensures multiple requests for the same missing chunk
	// trigger only a single download.
	_, err, _ := c.sf.Do(path, func() (interface{}, error) {
		// Check if it already exists locally
		if _, err := os.Stat(localPath); err == nil {
			return nil, nil // Cache hit
		}

		// Cache miss: download and extract
		rc, err := c.remote.Read(ctx, path)
		if err != nil {
			return nil, err
		}
		defer rc.Close()

		// Extract the tar.gz to the local directory
		if err := extractTarGz(rc, localPath); err != nil {
			return nil, fmt.Errorf("failed to extract index: %w", err)
		}

		return nil, nil
	})

	if err != nil {
		return nil, err
	}

	// At this point, the file/directory exists locally.
	// We return a dummy ReadSeekCloser because Bluge uses memory-mapped access
	// on the directory rather than standard IO. The query engine will just use
	// the local path. We can just return a file to satisfy the interface, or
	// we can add a method specifically for getting the local path of an index.
	// For now, return the open file if we were just caching a file, but since
	// it's an extracted directory, returning nil or a custom closer is better.
	// Since bluge opens the path directly, we might not need this Read to return the file.
	// Let's return a dummy closer for the directory or handle it differently in the query layer.
	// Returning the local directory path is what Bluge actually needs.
	return nil, fmt.Errorf("use GetLocalIndexPath for index directories")
}

// GetLocalIndexPath retrieves the local path for an index, downloading it if necessary.
func (c *CachedStore) GetLocalIndexPath(ctx context.Context, path string) (string, error) {
	localPath := filepath.Join(c.localDir, strings.TrimSuffix(path, ".tar.gz"))

	_, err, _ := c.sf.Do(path, func() (interface{}, error) {
		if _, err := os.Stat(localPath); err == nil {
			return nil, nil // Cache hit
		}

		rc, err := c.remote.Read(ctx, path)
		if err != nil {
			return nil, err
		}
		defer rc.Close()

		if err := extractTarGz(rc, localPath); err != nil {
			return nil, fmt.Errorf("failed to extract index: %w", err)
		}

		return nil, nil
	})

	if err != nil {
		return "", err
	}

	return localPath, nil
}

// extractTarGz extracts a gzipped tarball to a destination directory.
func extractTarGz(r io.Reader, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	gzr, err := gzip.NewReader(r)
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

		target := filepath.Join(destDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
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

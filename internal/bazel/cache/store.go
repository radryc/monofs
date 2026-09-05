// Package cache implements a Bazel-compatible remote cache backed by
// local filesystem storage. Both the Action Cache (AC) and
// Content-Addressable Storage (CAS) are stored as files named by
// their digest.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Store provides a key-value store for Bazel cache entries.
// Keys are digest strings (lowercase hex of SHA-256).
// Values are raw bytes.
//
// Layout on disk:
//
//	data/
//	  ac/<digest>        # Action Cache entries (small, < 10 KB typical)
//	  cas/<digest>       # Content-Addressed blobs (variable size)
//
// This is intentionally simple; for production, the CAS directory
// can be backed by S3/GCS via the existing FetchBackend, with this
// local store as a hot cache layer.
type Store struct {
	dir  string
	mu   sync.RWMutex
	opts StoreOptions
}

// StoreOptions configures a Store.
type StoreOptions struct {
	// Dir is the root data directory.
	Dir string

	// MaxCASBlobSize is the maximum allowed CAS blob size in bytes.
	// Default: 512 MB. Blobs larger than this are rejected.
	MaxCASBlobSize int64

	// MaxACEntries limits the number of Action Cache entries.
	// 0 = unlimited.
	MaxACEntries int64
}

// DefaultStoreOptions returns sensible defaults.
func DefaultStoreOptions(dir string) StoreOptions {
	return StoreOptions{
		Dir:            dir,
		MaxCASBlobSize: 512 * 1024 * 1024, // 512 MB
		MaxACEntries:   0,                 // unlimited
	}
}

// NewStore creates a Store and ensures the data directories exist.
func NewStore(opts StoreOptions) (*Store, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("cache store dir is required")
	}
	for _, sub := range []string{"ac", "cas"} {
		if err := os.MkdirAll(filepath.Join(opts.Dir, sub), 0755); err != nil {
			return nil, fmt.Errorf("mkdir %s/%s: %w", opts.Dir, sub, err)
		}
	}
	return &Store{dir: opts.Dir, opts: opts}, nil
}

// Dir returns the root data directory.
func (s *Store) Dir() string { return s.dir }

// --- Action Cache (AC) ---

// GetAC retrieves an Action Cache entry by digest.
// Returns (nil, ErrNotFound) if not present.
func (s *Store) GetAC(ctx context.Context, digest string) ([]byte, error) {
	return s.readBlob("ac", digest)
}

// PutAC stores an Action Cache entry.
func (s *Store) PutAC(ctx context.Context, digest string, data []byte) error {
	return s.writeBlob("ac", digest, data)
}

// HasAC reports whether an AC entry exists.
func (s *Store) HasAC(ctx context.Context, digest string) (bool, error) {
	return s.blobExists("ac", digest)
}

// ACStats returns the number of AC entries and total bytes.
func (s *Store) ACStats(ctx context.Context) (entries int64, bytes int64, _ error) {
	return s.dirStats("ac")
}

// --- Content-Addressable Storage (CAS) ---

// GetCAS retrieves a CAS blob by digest.
// Returns (nil, 0, ErrNotFound) if not present.
func (s *Store) GetCAS(ctx context.Context, digest string) ([]byte, int64, error) {
	data, err := s.readBlob("cas", digest)
	if err != nil {
		return nil, 0, err
	}
	return data, int64(len(data)), nil
}

// GetCASStream is like GetCAS but returns a reader for streaming.
func (s *Store) GetCASStream(ctx context.Context, digest string) (io.ReadCloser, int64, error) {
	path := s.blobPath("cas", digest)
	s.mu.RLock()
	defer s.mu.RUnlock()

	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("stat CAS blob: %w", err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open CAS blob: %w", err)
	}
	return f, fi.Size(), nil
}

// PutCAS stores a CAS blob. If the blob exceeds MaxCASBlobSize, it is rejected.
// The digest is validated against the data. digest must be in "hash/size" format.
func (s *Store) PutCAS(ctx context.Context, digest string, data []byte) error {
	if s.opts.MaxCASBlobSize > 0 && int64(len(data)) > s.opts.MaxCASBlobSize {
		return fmt.Errorf("%w: %d bytes exceeds max %d", ErrBlobTooLarge, len(data), s.opts.MaxCASBlobSize)
	}
	// Parse hash from digest string, validate against data.
	expectedHash, _, err := ParseDigest(digest)
	if err != nil {
		return fmt.Errorf("parse digest: %w", err)
	}
	actualHash := hex.EncodeToString(sha256Hash(data))
	if expectedHash != actualHash {
		return fmt.Errorf("%w: expected %s, computed %s", ErrDigestMismatch, expectedHash, actualHash)
	}
	return s.writeBlob("cas", digest, data)
}

// PutCASNoValidate stores a CAS blob without validating the digest.
// Use only when the digest is already verified (e.g. during streaming upload).
func (s *Store) PutCASNoValidate(ctx context.Context, digest string, data []byte) error {
	if s.opts.MaxCASBlobSize > 0 && int64(len(data)) > s.opts.MaxCASBlobSize {
		return fmt.Errorf("%w: %d bytes exceeds max %d", ErrBlobTooLarge, len(data), s.opts.MaxCASBlobSize)
	}
	return s.writeBlob("cas", digest, data)
}

// HasCAS reports whether a CAS blob exists.
func (s *Store) HasCAS(ctx context.Context, digest string) (bool, error) {
	return s.blobExists("cas", digest)
}

// CASStats returns the number of CAS blobs and total bytes.
func (s *Store) CASStats(ctx context.Context) (blobs int64, bytes int64, _ error) {
	return s.dirStats("cas")
}

// DeleteCAS removes a CAS blob.
func (s *Store) DeleteCAS(ctx context.Context, digest string) error {
	path := s.blobPath("cas", digest)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete CAS blob: %w", err)
	}
	return nil
}

// --- internal helpers ---

func (s *Store) blobPath(kind, digest string) string {
	// Bazel digest format: "<sha256hex>/<size>"
	// Store as: data/<kind>/<sha256hex>_<size>
	digest = strings.ReplaceAll(digest, "/", "_")
	digest = filepath.Base(digest)
	return filepath.Join(s.dir, kind, digest)
}

func (s *Store) readBlob(kind, digest string) ([]byte, error) {
	path := s.blobPath(kind, digest)
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read %s blob: %w", kind, err)
	}
	return data, nil
}

func (s *Store) writeBlob(kind, digest string, data []byte) error {
	path := s.blobPath(kind, digest)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Write to temp file, then rename atomically.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write %s blob tmp: %w", kind, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s blob: %w", kind, err)
	}
	return nil
}

func (s *Store) blobExists(kind, digest string) (bool, error) {
	path := s.blobPath(kind, digest)
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (s *Store) dirStats(kind string) (entries int64, bytes int64, _ error) {
	dir := filepath.Join(s.dir, kind)
	s.mu.RLock()
	defer s.mu.RUnlock()

	des, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, fmt.Errorf("readdir %s: %w", kind, err)
	}
	for _, de := range des {
		if de.IsDir() {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		entries++
		bytes += fi.Size()
	}
	return entries, bytes, nil
}

// CASBlobAge returns the modification time of a CAS blob.
// Used by the eviction job to find old blobs.
func (s *Store) CASBlobAge(ctx context.Context, digest string) (time.Time, error) {
	path := s.blobPath("cas", digest)
	s.mu.RLock()
	defer s.mu.RUnlock()
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// ListCAS returns all CAS blob digests.
func (s *Store) ListCAS(ctx context.Context) ([]string, error) {
	dir := filepath.Join(s.dir, "cas")
	s.mu.RLock()
	defer s.mu.RUnlock()

	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("readdir cas: %w", err)
	}
	var digests []string
	for _, de := range des {
		if de.IsDir() {
			continue
		}
		digests = append(digests, de.Name())
	}
	return digests, nil
}

// Close releases any resources. Currently a no-op for filesystem storage.
func (s *Store) Close() error { return nil }

// --- errors ---

var (
	ErrNotFound       = fmt.Errorf("cache entry not found")
	ErrBlobTooLarge   = fmt.Errorf("blob too large")
	ErrDigestMismatch = fmt.Errorf("digest mismatch")
)

// --- helpers ---

func sha256Hash(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}

// DigestString returns the lowercase hex SHA-256 of data, prefixed
// with the byte size: "<hash>/<size>". This is the format Bazel uses.
func DigestString(data []byte) string {
	h := sha256Hash(data)
	return fmt.Sprintf("%s/%d", hex.EncodeToString(h), len(data))
}

// ParseDigest extracts the hex hash and size from a Bazel-style digest string.
// "abc123.../42" → ("abc123...", 42, nil)
func ParseDigest(s string) (hash string, size int64, err error) {
	for i, c := range s {
		if c == '/' {
			hash = s[:i]
			_, scanErr := fmt.Sscanf(s[i+1:], "%d", &size)
			if scanErr != nil {
				return "", 0, fmt.Errorf("parse digest size: %w", scanErr)
			}
			if size < 0 {
				return "", 0, fmt.Errorf("negative size in digest: %s", s)
			}
			return hash, size, nil
		}
	}
	return "", 0, fmt.Errorf("invalid digest format (missing /): %s", s)
}

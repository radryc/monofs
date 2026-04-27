package logengine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/radryc/monofs/internal/storage"
)

// LogEngine is the main interface for the high-compression, searchable log & trace engine.
type LogEngine struct {
	store    *CachedStore
	ingester *Ingester
	query    *QueryEngine
}

// Config holds configuration for the LogEngine.
type Config struct {
	LocalCacheDir string
	ChunkDuration time.Duration
}

// New creates a new LogEngine instance with the given storage backend and configuration.
func New(backend ObjectStoreBackend, cfg Config) *LogEngine {
	cachedStore := NewCachedStore(backend, cfg.LocalCacheDir)
	return &LogEngine{
		store:    cachedStore,
		ingester: NewIngester(cachedStore, cfg.ChunkDuration),
		query:    NewQueryEngine(cachedStore),
	}
}

// Type returns the storage type identifier.
func (e *LogEngine) Type() string {
	return "logengine"
}

// Initialize prepares the backend.
func (e *LogEngine) Initialize(ctx context.Context, config storage.BackendConfig) error {
	// Not fully implemented yet
	return nil
}

// Ingest writes a batch of log records to the engine. Note: this uses LogRecord specifically.
func (e *LogEngine) IngestLogs(ctx context.Context, chunkID string, logs []LogRecord) error {
	return e.ingester.FlushChunk(ctx, chunkID, logs)
}

// Ingest implements storage.StorageBackend interface.
func (e *LogEngine) Ingest(ctx context.Context, id string, data []byte) error {
	// In a real implementation, data would be unmarshaled into []LogRecord
	return nil
}

// Query executes a LogQL query and returns the matching log records.
func (e *LogEngine) QueryLogs(ctx context.Context, queryStr string) ([]LogRecord, error) {
	return e.query.Query(ctx, queryStr)
}

func (e *LogEngine) Query(ctx context.Context, queryStr string) ([]byte, error) {
	logs, err := e.query.Query(ctx, queryStr)
	if err != nil {
		return nil, err
	}
	
	// Encode logs to JSON
	return json.Marshal(logs)
}

// Close cleans up resources.
func (e *LogEngine) Close() error {
	// Clean up cache dir if necessary or flush pending buffers
	return nil
}

// --- Mock S3 Storage Implementation for demonstration ---

// MockS3Store implements StorageBackend over the local filesystem
// but simulates S3 lifecycle expirations (Ghost Chunks).
type MockS3Store struct {
	baseDir string
}

// NewMockS3Store creates a new MockS3Store.
func NewMockS3Store(baseDir string) *MockS3Store {
	return &MockS3Store{baseDir: baseDir}
}

func (s *MockS3Store) Write(ctx context.Context, path string, reader io.Reader) error {
	fullPath := filepath.Join(s.baseDir, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return err
	}
	f, err := os.Create(fullPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, reader)
	return err
}

func (s *MockS3Store) Read(ctx context.Context, path string) (io.ReadSeekCloser, error) {
	fullPath := filepath.Join(s.baseDir, path)
	f, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Map NotExist to ErrGhostChunk to simulate S3 NoSuchKey
			return nil, ErrGhostChunk
		}
		return nil, err
	}
	return f, nil
}

func (s *MockS3Store) ListChunks(ctx context.Context, prefix string) ([]string, error) {
	// Simple mock listing
	fullPrefix := filepath.Join(s.baseDir, prefix)
	entries, err := os.ReadDir(fullPrefix)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var chunks []string
	for _, e := range entries {
		if e.IsDir() {
			chunks = append(chunks, e.Name())
		}
	}
	return chunks, nil
}

// dummyReadSeekCloser is a utility for testing
type dummyReadSeekCloser struct {
	*bytes.Reader
}

func (d *dummyReadSeekCloser) Close() error {
	return nil
}

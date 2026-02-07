// Package server implements the MonoFS gRPC server with NutsDB storage.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nutsdb/nutsdb"
	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/fetcher"
	"google.golang.org/grpc"
)

// Server implements the MonoFS gRPC server with NutsDB storage.
type Server struct {
	pb.UnimplementedMonoFSServer

	nodeID       string
	address      string
	dbPath       string // Path to the NutsDB data directory
	gitCachePath string // Path to the git repository cache directory (unused, kept for disk stats)
	startTime    time.Time
	logger       *slog.Logger

	// NutsDB for metadata storage
	db *nutsdb.DB

	// Fetcher client for external blob retrieval (required)
	fetcherClient *fetcher.Client

	// Predictor for access pattern learning and prefetching
	predictor *Predictor

	// Cache for intermediate directories (path -> exists)
	intermediateDirCache sync.Map

	// Stats
	filesServed    atomic.Uint64
	totalFiles     atomic.Int64 // Total files owned by this node
	prefetchHits   atomic.Uint64
	prefetchMisses atomic.Uint64
}

type repoInfo struct {
	StorageID   string `json:"storage_id"`   // SHA-256 hash (primary key)
	DisplayPath string `json:"display_path"` // User-visible path
	RepoURL     string `json:"repo_url"`
	Branch      string `json:"branch"`
}

type storedMetadata struct {
	Path        string `json:"path"`         // Full path: "github.com/owner/repo/path/to/file"
	StorageID   string `json:"storage_id"`   // SHA-256 hash of display path
	DisplayPath string `json:"display_path"` // User-visible repo path
	FilePath    string `json:"file_path"`    // Path within repo: "path/to/file"
	Size        uint64 `json:"size"`
	Mode        uint32 `json:"mode"`
	Mtime       int64  `json:"mtime"`
	BlobHash    string `json:"blob_hash"`
	Branch      string `json:"branch"`
	RepoURL     string `json:"repo_url"`
	IsDir       bool   `json:"is_dir"`
}

// dirIndexEntry represents a file entry in the directory index.
type dirIndexEntry struct {
	Name    string `json:"name"`     // Filename (not full path)
	Mode    uint32 `json:"mode"`     // File mode
	Size    uint64 `json:"size"`     // File size
	Mtime   int64  `json:"mtime"`    // Modification time
	HashKey string `json:"hash_key"` // Reference to metadata bucket
	IsDir   bool   `json:"is_dir"`   // Whether this is a directory
}

const (
	bucketMetadata         = "metadata"          // File metadata storage (key: SHA-256 hash)
	bucketRepos            = "repos"             // Repository information (key: storageID)
	bucketPathIndex        = "pathindex"         // Path to hash mapping (key: "storageID:filePath", value: SHA-256 hash)
	bucketRepoLookup       = "repolookup"        // Display path to storageID mapping (key: displayPath, value: storageID)
	bucketDirIndex         = "dirindex"          // Directory index (key: "storageID:sha256(dirPath)", value: []dirIndexEntry)
	bucketOwnedFiles       = "ownedfiles"        // Files owned by this node (key: "storageID:filePath", value: "1")
	bucketReplicaFiles     = "replicafiles"      // Replica file tracking (key: "storageID:filePath", value: ownerNodeID)
	bucketOnboardingStatus = "onboarding_status" // Repository onboarding status (key: storage_id, value: "true"/"false")
)

// makeStorageKey generates a SHA-256 hash key for database storage.
// This avoids key length limitations while ensuring uniqueness.
// Format: sha256("storageID:filePath")
func makeStorageKey(storageID, filePath string) []byte {
	compositeKey := storageID + ":" + filePath
	hash := sha256.Sum256([]byte(compositeKey))
	return []byte(hex.EncodeToString(hash[:]))
}

// makeDirIndexKey generates a directory index key.
// Format: "storageID:sha256(directoryPath)"
func makeDirIndexKey(storageID, dirPath string) []byte {
	hash := sha256.Sum256([]byte(dirPath))
	return []byte(storageID + ":" + hex.EncodeToString(hash[:]))
}

// extractDirPath extracts the directory path from a file path.
// Returns empty string for root files.
func extractDirPath(filePath string) string {
	if filePath == "" {
		return ""
	}
	idx := strings.LastIndex(filePath, "/")
	if idx < 0 {
		return "" // Root file
	}
	return filePath[:idx]
}

// extractFileName extracts the filename from a file path.
func extractFileName(filePath string) string {
	if filePath == "" {
		return ""
	}
	idx := strings.LastIndex(filePath, "/")
	if idx < 0 {
		return filePath // Root file
	}
	return filePath[idx+1:]
}

// makeFullPath constructs the full filesystem path from displayPath and filePath.
// Example: displayPath="github.com/owner/repo", filePath="README.md" -> "github.com/owner/repo/README.md"
func makeFullPath(displayPath, filePath string) string {
	if filePath == "" {
		return displayPath
	}
	return displayPath + "/" + filePath
}

// dirSize calculates the total size (bytes) of regular files under a directory.
// Missing directories return size 0 without error.
func dirSize(path string) (int64, error) {
	if path == "" {
		return 0, nil
	}
	var total int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			info, infoErr := d.Info()
			if infoErr != nil {
				return infoErr
			}
			total += info.Size()
		}
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return total, err
}

// getHashFromPath retrieves the stored hash for a given storageID:filePath from the index.
// Returns the hash and true if found, or calculates it and returns false if not cached.
func (s *Server) getHashFromPath(storageID, filePath string) ([]byte, bool) {
	indexKey := []byte(storageID + ":" + filePath)
	var hash []byte
	found := false

	s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketPathIndex, indexKey)
		if err == nil {
			hash = value
			found = true
		}
		return nil
	})

	if found {
		return hash, true
	}

	// Not in index, calculate it
	return makeStorageKey(storageID, filePath), false
}

// NewServer creates a new MonoFS server.
func NewServer(nodeID, address, dbPath, gitCacheDir string, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "server", "node_id", nodeID)

	// Open NutsDB with performance optimizations
	opt := nutsdb.DefaultOptions
	opt.Dir = dbPath
	opt.SegmentSize = 64 * 1024 * 1024             // 64MB segments
	opt.EntryIdxMode = nutsdb.HintKeyAndRAMIdxMode // Use hint file for faster startup (only keys in RAM)
	opt.RWMode = nutsdb.MMap                       // Use mmap for faster reads
	opt.SyncEnable = false                         // Async writes for better performance (trade durability for speed)
	db, err := nutsdb.Open(opt)
	if err != nil {
		return nil, fmt.Errorf("failed to open nutsdb: %w", err)
	}

	// Initialize buckets
	if err := db.Update(func(tx *nutsdb.Tx) error {
		return tx.NewBucket(nutsdb.DataStructureBTree, bucketMetadata)
	}); err != nil && err != nutsdb.ErrBucketAlreadyExist {
		db.Close()
		return nil, fmt.Errorf("failed to create metadata bucket: %w", err)
	}

	if err := db.Update(func(tx *nutsdb.Tx) error {
		return tx.NewBucket(nutsdb.DataStructureBTree, bucketRepos)
	}); err != nil && err != nutsdb.ErrBucketAlreadyExist {
		db.Close()
		return nil, fmt.Errorf("failed to create repos bucket: %w", err)
	}

	if err := db.Update(func(tx *nutsdb.Tx) error {
		return tx.NewBucket(nutsdb.DataStructureBTree, bucketPathIndex)
	}); err != nil && err != nutsdb.ErrBucketAlreadyExist {
		db.Close()
		return nil, fmt.Errorf("failed to create path index bucket: %w", err)
	}

	if err := db.Update(func(tx *nutsdb.Tx) error {
		return tx.NewBucket(nutsdb.DataStructureBTree, bucketRepoLookup)
	}); err != nil && err != nutsdb.ErrBucketAlreadyExist {
		db.Close()
		return nil, fmt.Errorf("failed to create repo lookup bucket: %w", err)
	}

	if err := db.Update(func(tx *nutsdb.Tx) error {
		return tx.NewBucket(nutsdb.DataStructureBTree, bucketDirIndex)
	}); err != nil && err != nutsdb.ErrBucketAlreadyExist {
		db.Close()
		return nil, fmt.Errorf("failed to create directory index bucket: %w", err)
	}

	if err := db.Update(func(tx *nutsdb.Tx) error {
		return tx.NewBucket(nutsdb.DataStructureBTree, bucketFailover)
	}); err != nil && err != nutsdb.ErrBucketAlreadyExist {
		db.Close()
		return nil, fmt.Errorf("failed to create failover bucket: %w", err)
	}

	if err := db.Update(func(tx *nutsdb.Tx) error {
		return tx.NewBucket(nutsdb.DataStructureBTree, bucketOwnedFiles)
	}); err != nil && err != nutsdb.ErrBucketAlreadyExist {
		db.Close()
		return nil, fmt.Errorf("failed to create owned files bucket: %w", err)
	}

	if err := db.Update(func(tx *nutsdb.Tx) error {
		return tx.NewBucket(nutsdb.DataStructureBTree, bucketReplicaFiles)
	}); err != nil && err != nutsdb.ErrBucketAlreadyExist {
		db.Close()
		return nil, fmt.Errorf("failed to create replica files bucket: %w", err)
	}

	if err := db.Update(func(tx *nutsdb.Tx) error {
		return tx.NewBucket(nutsdb.DataStructureBTree, bucketOnboardingStatus)
	}); err != nil && err != nutsdb.ErrBucketAlreadyExist {
		db.Close()
		return nil, fmt.Errorf("failed to create onboarding status bucket: %w", err)
	}

	s := &Server{
		nodeID:       nodeID,
		address:      address,
		dbPath:       dbPath,
		gitCachePath: gitCacheDir, // Kept for disk stats reporting
		startTime:    time.Now(),
		logger:       logger,
		db:           db,
	}

	// Initialize file count from database
	var initialCount int64
	s.db.View(func(tx *nutsdb.Tx) error {
		keys, err := tx.GetKeys(bucketOwnedFiles)
		if err == nil {
			initialCount = int64(len(keys))
		}
		return nil
	})
	s.totalFiles.Store(initialCount)
	logger.Info("initialized file count", "total_files", initialCount)

	return s, nil
}

// ConfigureFetcher sets up the fetcher client for external blob retrieval.
// This MUST be called after NewServer - storage nodes require fetchers for blob access.
func (s *Server) ConfigureFetcher(fetcherAddrs []string) error {
	if len(fetcherAddrs) == 0 {
		return fmt.Errorf("fetcher addresses required: storage nodes cannot operate without fetchers")
	}

	config := fetcher.DefaultClientConfig()
	config.FetcherAddresses = fetcherAddrs

	client, err := fetcher.NewClient(config, s.logger)
	if err != nil {
		return fmt.Errorf("failed to create fetcher client: %w", err)
	}

	s.fetcherClient = client

	// Initialize predictor with fetcher client for prefetch requests
	predictorConfig := DefaultPredictorConfig()
	// Ignore search indexer access patterns - they scan all files sequentially
	// which pollutes the Markov chains with non-user access patterns
	predictorConfig.IgnoreClientIDs = []string{"search-indexer"}
	s.predictor = NewPredictor(client, predictorConfig, s.logger)

	s.logger.Info("fetcher client configured",
		"fetcher_addrs", fetcherAddrs,
		"predictor_enabled", true)

	return nil
}

// GetPredictorStats returns prediction statistics.
func (s *Server) GetPredictorStats() *PredictorStats {
	if s.predictor == nil {
		return nil
	}
	stats := s.predictor.GetStats()
	return &stats
}

// GetPrefetchStats returns prefetch hit/miss statistics.
func (s *Server) GetPrefetchStats() (hits, misses uint64) {
	return s.prefetchHits.Load(), s.prefetchMisses.Load()
}

// lookupStorageID finds the storage ID for a given display path.
// Returns the storage ID and true if found, or empty string and false if not found.
func (s *Server) lookupStorageID(displayPath string) (string, bool) {
	var storageID string
	found := false

	s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketRepoLookup, []byte(displayPath))
		if err == nil {
			storageID = string(value)
			found = true
		}
		return nil
	})

	return storageID, found
}

// resolvePathToStorage converts a filesystem path to (storageID, filePath).
// It tries longest-to-shortest prefix matching against registered display paths.
// Returns storageID, filePath, and ok=true if a match is found.
func (s *Server) resolvePathToStorage(path string) (storageID, filePath string, ok bool) {
	if path == "" {
		return "", "", false
	}

	parts := strings.Split(strings.Trim(path, "/"), "/")

	// Try longest-to-shortest prefix matching
	for i := len(parts); i > 0; i-- {
		displayPath := strings.Join(parts[:i], "/")

		// Lookup: displayPath -> storageID
		storageID, exists := s.lookupStorageID(displayPath)
		if exists {
			var filePath string
			if i < len(parts) {
				filePath = strings.Join(parts[i:], "/")
			}
			return storageID, filePath, true
		}

		// Fallback for Go modules: try without version suffix
		// This handles repos ingested with displayPath like "google.golang.org/grpc"
		// when user requests "google.golang.org/grpc@v1.75.0/file.go"
		if strings.Contains(displayPath, "@") {
			// Extract module path without version (e.g., "google.golang.org/grpc@v1.75.0" -> "google.golang.org/grpc")
			atIdx := strings.LastIndex(displayPath, "@")
			modulePathOnly := displayPath[:atIdx]
			storageID, exists := s.lookupStorageID(modulePathOnly)
			if exists {
				// File path includes everything after the module path
				var filePath string
				if i < len(parts) {
					filePath = strings.Join(parts[i:], "/")
				}
				return storageID, filePath, true
			}
		}
	}

	return "", "", false
}

// repoExistsByStorageID checks if a repository exists by storage ID.
func (s *Server) repoExistsByStorageID(storageID string) bool {
	exists := false
	s.db.View(func(tx *nutsdb.Tx) error {
		_, err := tx.Get(bucketRepos, []byte(storageID))
		exists = (err == nil)
		return nil
	})
	return exists
}

// repoExistsByStorageIDTx checks if a repository exists within an existing transaction.
// Use this version when already inside a transaction to avoid deadlocks.
func (s *Server) repoExistsByStorageIDTx(tx *nutsdb.Tx, storageID string) bool {
	_, err := tx.Get(bucketRepos, []byte(storageID))
	return err == nil
}

// repoExists checks if a repository exists by display path (legacy method).
func (s *Server) repoExists(displayPath string) bool {
	storageID, found := s.lookupStorageID(displayPath)
	if !found {
		return false
	}
	return s.repoExistsByStorageID(storageID)
}

// isIntermediateDir checks if a path is an intermediate directory (prefix of any repo)
// Results are cached in memory to avoid repeated database scans
//
// CRITICAL: This function MUST scan bucketRepoLookup (not bucketRepos) because:
// - bucketRepoLookup: key = display_path (e.g., "github.com/user/repo")
// - bucketRepos: key = storage_id (SHA256 hash, e.g., "a7f3b2c...")
// When checking if "github_com" is a prefix, we need to compare against display paths,
// not SHA256 hashes. Using bucketRepos would cause all intermediate directory lookups to fail.
//
// Test coverage: See TestIsIntermediateDir in lookup_test.go
func (s *Server) isIntermediateDir(path string) bool {
	// Check cache first
	if cached, ok := s.intermediateDirCache.Load(path); ok {
		return cached.(bool)
	}

	// Check database - scan bucketRepoLookup for display paths that start with this prefix
	pathPrefix := path + "/"
	isIntermediate := false
	s.db.View(func(tx *nutsdb.Tx) error {
		// Use GetKeys to get only keys (lighter than GetAll), then check prefixes
		keys, err := tx.GetKeys(bucketRepoLookup)
		if err != nil {
			return nil
		}
		for _, key := range keys {
			displayPath := string(key)
			if strings.HasPrefix(displayPath, pathPrefix) {
				isIntermediate = true
				return nil // Early exit on first match
			}
		}
		return nil
	})

	// Cache the result
	s.intermediateDirCache.Store(path, isIntermediate)
	return isIntermediate
}

// Register registers the server with a gRPC server.
func (s *Server) Register(grpcServer *grpc.Server) {
	pb.RegisterMonoFSServer(grpcServer, s)
}

// RegisterRepository registers repository metadata on this node.
// This is called by the router BEFORE file ingestion to ensure all nodes
// know about the repository and can resolve display paths.
func (s *Server) RegisterRepository(ctx context.Context, req *pb.RegisterRepositoryRequest) (*pb.RegisterRepositoryResponse, error) {
	s.logger.Info("registering repository",
		"storage_id", req.StorageId,
		"display_path", req.DisplayPath,
		"source", req.Source)

	// Store repository info in database
	err := s.db.Update(func(tx *nutsdb.Tx) error {
		// Check if storage ID already exists (repo metadata)
		repoExists := s.repoExistsByStorageIDTx(tx, req.StorageId)
		
		if !repoExists {
			// Store repo metadata only if new storage ID
			info := &repoInfo{
				StorageID:   req.StorageId,
				DisplayPath: req.DisplayPath,
				RepoURL:     req.Source,
				Branch:      "", // Branch will be set during file ingestion
			}
			repoKey := []byte(req.StorageId)
			repoValue, err := json.Marshal(info)
			if err != nil {
				return fmt.Errorf("marshal repo info: %w", err)
			}

			if err := tx.Put(bucketRepos, repoKey, repoValue, 0); err != nil {
				return fmt.Errorf("store repo info: %w", err)
			}
		} else {
			s.logger.Debug("repository storage already exists, adding display path mapping",
				"storage_id", req.StorageId,
				"display_path", req.DisplayPath)
		}

		// ALWAYS store display path → storage_id mapping, even if storage ID exists.
		// This supports hard links: multiple display paths can point to the same storage.
		// Example: both "github.com/google/uuid@v1.6.0" and "go-modules/pkg/mod/github.com/google/uuid@v1.6.0"
		// can map to the same storageID for virtual build tool layouts.
		lookupKey := []byte(req.DisplayPath)
		if err := tx.Put(bucketRepoLookup, lookupKey, []byte(req.StorageId), 0); err != nil {
			return fmt.Errorf("store path lookup: %w", err)
		}

		s.logger.Info("repository registered successfully",
			"storage_id", req.StorageId,
			"display_path", req.DisplayPath,
			"is_new_storage", !repoExists)

		return nil
	})

	if err != nil {
		s.logger.Error("failed to register repository", "error", err)
		return &pb.RegisterRepositoryResponse{
			Success: false,
			Message: err.Error(),
		}, err
	}

	// Verify registration by reading it back
	var verifyInfo repoInfo
	verifyErr := s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketRepos, []byte(req.StorageId))
		if err != nil {
			return err
		}
		return json.Unmarshal(value, &verifyInfo)
	})

	if verifyErr != nil {
		s.logger.Error("registration verification failed - repo not found after writing",
			"storage_id", req.StorageId,
			"error", verifyErr)
		return &pb.RegisterRepositoryResponse{
			Success: false,
			Message: fmt.Sprintf("registration verification failed: %v", verifyErr),
		}, verifyErr
	}

	s.logger.Info("repository registration verified",
		"storage_id", req.StorageId,
		"display_path", verifyInfo.DisplayPath)

	// Invalidate intermediate directory cache for all path components
	parts := strings.Split(req.DisplayPath, "/")
	for i := 1; i < len(parts); i++ {
		prefix := strings.Join(parts[:i], "/")
		s.intermediateDirCache.Delete(prefix)
	}

	return &pb.RegisterRepositoryResponse{
		Success: true,
		Message: "repository registered",
	}, nil
}

// IngestFile stores file metadata from router ingestion.
func (s *Server) IngestFile(ctx context.Context, req *pb.IngestFileRequest) (*pb.IngestFileResponse, error) {
	meta := req.Metadata

	storageID := meta.StorageId
	displayPath := meta.DisplayPath

	if storageID == "" || displayPath == "" {
		return &pb.IngestFileResponse{Success: false},
			fmt.Errorf("storage_id and display_path are required")
	}

	s.logger.Info("ingesting file",
		"path", meta.Path,
		"display_path", displayPath,
		"storage_id", storageID,
		"size", meta.Size,
		"mode", fmt.Sprintf("0%o", meta.Mode),
		"blob_hash", meta.BlobHash)

	// Generate SHA-256 hash key from storageID:filePath
	key := makeStorageKey(storageID, meta.Path)
	fullPath := makeFullPath(displayPath, meta.Path)

	s.logger.Info("storing key",
		"hash_key", string(key),
		"full_path", fullPath,
		"storage_id", storageID,
		"display_path", displayPath,
		"file_path", meta.Path)

	stored := storedMetadata{
		Path:        fullPath,
		StorageID:   storageID,
		DisplayPath: displayPath,
		FilePath:    meta.Path,
		Size:        meta.Size,
		Mode:        meta.Mode,
		Mtime:       meta.Mtime,
		BlobHash:    meta.BlobHash,
		Branch:      meta.Ref,
		RepoURL:     meta.Source,
		IsDir:       false,
	}

	value, err := json.Marshal(stored)
	if err != nil {
		s.logger.Error("failed to marshal metadata", "error", err, "path", meta.Path)
		return &pb.IngestFileResponse{Success: false}, err
	}

	// Store in NutsDB (metadata, path index, repo info, and directory index)
	err = s.db.Update(func(tx *nutsdb.Tx) error {
		// 1. Store metadata with hash key
		if err := tx.Put(bucketMetadata, key, value, 0); err != nil {
			return err
		}

		// 2. Store path-to-hash mapping for fast lookups
		indexKey := []byte(storageID + ":" + meta.Path)
		if err := tx.Put(bucketPathIndex, indexKey, key, 0); err != nil {
			return err
		}

		// 3. Update directory index incrementally for single-file operations
		// This provides immediate directory consistency for single file ingestion
		// (Batch operations skip this for performance - see IngestFileBatch)
		if err := s.updateDirectoryIndexHierarchy(tx, storageID, meta.Path, key, meta.Mode, meta.Size, meta.Mtime); err != nil {
			s.logger.Warn("failed to update directory index",
				"storage_id", storageID,
				"path", meta.Path,
				"error", err)
			// Don't fail the entire operation - directory index can be rebuilt
		}

		// 4. Store or update repo info - always update to ensure branch is set
		// This handles the case where RegisterRepository was called first without branch
		repoKey := []byte(storageID)
		existingRepoData, existsErr := tx.Get(bucketRepos, repoKey)
		isNewRepo := existsErr == nutsdb.ErrKeyNotFound

		// Build repo info - if existing, preserve fields but ensure branch is set
		info := &repoInfo{
			StorageID:   storageID,
			DisplayPath: displayPath,
			Branch:      meta.Ref,
			RepoURL:     meta.Source,
		}

		// If repo exists but branch is empty and we have a branch, update it
		if !isNewRepo && existsErr == nil {
			var existing repoInfo
			if json.Unmarshal(existingRepoData, &existing) == nil {
				if existing.Branch == "" && meta.Ref != "" {
					// Update with new branch
					s.logger.Info("updating repo branch",
						"storage_id", storageID,
						"branch", meta.Ref)
				} else if existing.Branch != "" {
					// Keep existing branch
					info.Branch = existing.Branch
				}
			}
		}

		repoValue, _ := json.Marshal(info)
		if err := tx.Put(bucketRepos, repoKey, repoValue, 0); err != nil {
			return err
		}

		if isNewRepo {
			// 5. Store display path lookup mapping for new repos
			lookupKey := []byte(displayPath)
			if err := tx.Put(bucketRepoLookup, lookupKey, []byte(storageID), 0); err != nil {
				return err
			}

			s.logger.Info("registered new repository",
				"storage_id", storageID,
				"display_path", displayPath,
				"source", meta.Source)
		}

		// 6. Mark repository as NOT fully onboarded yet (ingestion in progress)
		// Router will mark it as onboarded after all files are ingested
		onboardKey := []byte(storageID)
		_, onboardErr := tx.Get(bucketOnboardingStatus, onboardKey)
		if onboardErr == nutsdb.ErrKeyNotFound {
			// First file for this repo - mark as pending onboarding
			if err := tx.Put(bucketOnboardingStatus, onboardKey, []byte("false"), 0); err != nil {
				return err
			}
		}

		// 7. Mark this file as owned by this node
		ownershipKey := []byte(storageID + ":" + meta.Path)
		_, ownershipErr := tx.Get(bucketOwnedFiles, ownershipKey)
		fileExists := (ownershipErr == nil)

		if err := tx.Put(bucketOwnedFiles, ownershipKey, []byte("1"), 0); err != nil {
			return fmt.Errorf("mark file ownership: %w", err)
		}

		// Increment counter only for new files
		if !fileExists {
			s.totalFiles.Add(1)
		}

		return nil
	})

	if err != nil {
		s.logger.Error("failed to store metadata in db", "error", err, "key", string(key))
		return &pb.IngestFileResponse{Success: false}, err
	}

	s.logger.Info("file ingested successfully", "path", meta.Path, "key", string(key))

	// Invalidate intermediate directory cache for all prefixes
	parts := strings.Split(displayPath, "/")
	for i := 1; i < len(parts); i++ {
		prefix := strings.Join(parts[:i], "/")
		s.intermediateDirCache.Delete(prefix)
	}

	return &pb.IngestFileResponse{Success: true}, nil
}

// IngestFileBatch stores multiple file metadata in a single database transaction.
// This is significantly faster than calling IngestFile repeatedly (10-50x improvement).
func (s *Server) IngestFileBatch(ctx context.Context, req *pb.IngestFileBatchRequest) (*pb.IngestFileBatchResponse, error) {
	if len(req.Files) == 0 {
		return &pb.IngestFileBatchResponse{
			Success:       true,
			FilesIngested: 0,
			FilesFailed:   0,
		}, nil
	}

	storageID := req.StorageId
	displayPath := req.DisplayPath
	repoURL := req.Source
	branch := req.Ref

	s.logger.Info("ingesting file batch",
		"batch_size", len(req.Files),
		"storage_id", storageID,
		"display_path", displayPath)

	// Pre-serialize all metadata to avoid doing it inside transaction
	type preparedFile struct {
		key          []byte
		value        []byte
		indexKey     []byte
		filePath     string
		ownershipKey []byte
	}

	prepared := make([]preparedFile, 0, len(req.Files))

	// Prepare all files (serialize JSON outside transaction)
	for _, meta := range req.Files {
		key := makeStorageKey(storageID, meta.Path)
		fullPath := makeFullPath(displayPath, meta.Path)

		stored := storedMetadata{
			Path:        fullPath,
			StorageID:   storageID,
			DisplayPath: displayPath,
			FilePath:    meta.Path,
			Size:        meta.Size,
			Mode:        meta.Mode,
			Mtime:       meta.Mtime,
			BlobHash:    meta.BlobHash,
			Branch:      branch,
			RepoURL:     repoURL,
			IsDir:       false,
		}

		value, err := json.Marshal(stored)
		if err != nil {
			s.logger.Error("failed to marshal metadata", "error", err, "path", meta.Path)
			continue
		}

		indexKey := []byte(storageID + ":" + meta.Path)
		ownershipKey := []byte(storageID + ":" + meta.Path)

		prepared = append(prepared, preparedFile{
			key:          key,
			value:        value,
			indexKey:     indexKey,
			filePath:     meta.Path,
			ownershipKey: ownershipKey,
		})
	}

	// Single database transaction for ALL files
	var filesIngested int64
	var filesFailed int64
	var newFiles int64

	err := s.db.Update(func(tx *nutsdb.Tx) error {
		// Store or update repository info - always update to ensure branch is set
		repoKey := []byte(storageID)
		existingRepoData, existsErr := tx.Get(bucketRepos, repoKey)
		isNewRepo := existsErr == nutsdb.ErrKeyNotFound

		info := &repoInfo{
			StorageID:   storageID,
			DisplayPath: displayPath,
			Branch:      branch,
			RepoURL:     repoURL,
		}

		// If repo exists but branch is empty and we have a branch, update it
		if !isNewRepo && existsErr == nil {
			var existing repoInfo
			if json.Unmarshal(existingRepoData, &existing) == nil {
				if existing.Branch == "" && branch != "" {
					s.logger.Info("updating repo branch in batch",
						"storage_id", storageID,
						"branch", branch)
				} else if existing.Branch != "" {
					info.Branch = existing.Branch
				}
			}
		}

		repoValue, _ := json.Marshal(info)
		if err := tx.Put(bucketRepos, repoKey, repoValue, 0); err != nil {
			return fmt.Errorf("store repo info: %w", err)
		}

		if isNewRepo {
			// Store display path lookup mapping
			lookupKey := []byte(displayPath)
			if err := tx.Put(bucketRepoLookup, lookupKey, []byte(storageID), 0); err != nil {
				return fmt.Errorf("store repo lookup: %w", err)
			}

			// Mark as pending onboarding
			onboardKey := []byte(storageID)
			if err := tx.Put(bucketOnboardingStatus, onboardKey, []byte("false"), 0); err != nil {
				return fmt.Errorf("store onboarding status: %w", err)
			}

			s.logger.Info("registered new repository in batch",
				"storage_id", storageID,
				"display_path", displayPath)
		}

		// Batch insert all files
		for _, pf := range prepared {
			// Check if file already exists
			_, existErr := tx.Get(bucketOwnedFiles, pf.ownershipKey)
			fileExists := (existErr == nil)

			// Store metadata
			if err := tx.Put(bucketMetadata, pf.key, pf.value, 0); err != nil {
				s.logger.Error("failed to store metadata", "error", err, "path", pf.filePath)
				filesFailed++
				continue
			}

			// Store path index
			if err := tx.Put(bucketPathIndex, pf.indexKey, pf.key, 0); err != nil {
				s.logger.Error("failed to store path index", "error", err, "path", pf.filePath)
				filesFailed++
				continue
			}

			// Mark ownership
			if err := tx.Put(bucketOwnedFiles, pf.ownershipKey, []byte("1"), 0); err != nil {
				s.logger.Error("failed to mark ownership", "error", err, "path", pf.filePath)
				filesFailed++
				continue
			}

			if filesIngested < 3 { // Log first 3 keys for debugging
				s.logger.Info("stored ownership key", "key", string(pf.ownershipKey))
			}

			filesIngested++
			if !fileExists {
				newFiles++
			}
		}

		return nil
	})

	if err != nil {
		s.logger.Error("batch transaction failed", "error", err)
		return &pb.IngestFileBatchResponse{
			Success:       false,
			FilesIngested: filesIngested,
			FilesFailed:   int64(len(req.Files)) - filesIngested,
			ErrorMessage:  err.Error(),
		}, err
	}

	// Update file count
	s.totalFiles.Add(newFiles)

	s.logger.Info("batch ingestion completed",
		"files_ingested", filesIngested,
		"files_failed", filesFailed,
		"new_files", newFiles)

	// Invalidate intermediate directory cache (batch operation)
	parts := strings.Split(displayPath, "/")
	for i := 1; i < len(parts); i++ {
		prefix := strings.Join(parts[:i], "/")
		s.intermediateDirCache.Delete(prefix)
	}

	return &pb.IngestFileBatchResponse{
		Success:       true,
		FilesIngested: filesIngested,
		FilesFailed:   filesFailed,
	}, nil
}

// IngestReplicaBatch stores replica metadata for failover purposes.
// Unlike IngestFileBatch (which stores primary ownership), this stores backup copies
// in bucketReplicaFiles so they can be used for instant failover.
func (s *Server) IngestReplicaBatch(ctx context.Context, req *pb.IngestReplicaBatchRequest) (*pb.IngestReplicaBatchResponse, error) {
	if len(req.Files) == 0 {
		return &pb.IngestReplicaBatchResponse{
			Success:         true,
			FilesReplicated: 0,
			FilesFailed:     0,
		}, nil
	}

	storageID := req.StorageId
	displayPath := req.DisplayPath
	primaryNodeID := req.PrimaryNodeId
	repoURL := req.Source
	branch := req.Ref

	s.logger.Info("ingesting replica batch",
		"batch_size", len(req.Files),
		"storage_id", storageID,
		"display_path", displayPath,
		"primary_node", primaryNodeID)

	// Pre-serialize all metadata
	type preparedReplica struct {
		metadataKey []byte
		metadataVal []byte
		replicaKey  []byte
		replicaVal  []byte
		filePath    string
	}

	prepared := make([]preparedReplica, 0, len(req.Files))

	for _, meta := range req.Files {
		// Metadata storage (same as primary, for serving during failover)
		metadataKey := makeStorageKey(storageID, meta.Path)
		fullPath := makeFullPath(displayPath, meta.Path)

		stored := storedMetadata{
			Path:        fullPath,
			StorageID:   storageID,
			DisplayPath: displayPath,
			FilePath:    meta.Path,
			Size:        meta.Size,
			Mode:        meta.Mode,
			Mtime:       meta.Mtime,
			BlobHash:    meta.BlobHash,
			Branch:      branch,
			RepoURL:     repoURL,
			IsDir:       false,
		}

		metadataVal, err := json.Marshal(stored)
		if err != nil {
			s.logger.Error("failed to marshal replica metadata", "error", err, "path", meta.Path)
			continue
		}

		// Replica tracking (key: "storageID:filePath:primary:nodeID")
		replicaKey := makeReplicaKey(storageID, meta.Path, primaryNodeID)
		replicaVal, _ := json.Marshal(stored)

		prepared = append(prepared, preparedReplica{
			metadataKey: metadataKey,
			metadataVal: metadataVal,
			replicaKey:  replicaKey,
			replicaVal:  replicaVal,
			filePath:    meta.Path,
		})
	}

	// Single database transaction for all replicas
	var filesReplicated int64
	var filesFailed int64

	err := s.db.Update(func(tx *nutsdb.Tx) error {
		// Ensure repo is registered (same as IngestFileBatch)
		repoKey := []byte(storageID)
		_, existsErr := tx.Get(bucketRepos, repoKey)
		if existsErr == nutsdb.ErrKeyNotFound {
			info := &repoInfo{
				StorageID:   storageID,
				DisplayPath: displayPath,
				Branch:      branch,
				RepoURL:     repoURL,
			}
			repoValue, _ := json.Marshal(info)
			if err := tx.Put(bucketRepos, repoKey, repoValue, 0); err != nil {
				return fmt.Errorf("store repo info: %w", err)
			}

			// Store display path lookup
			lookupKey := []byte(displayPath)
			if err := tx.Put(bucketRepoLookup, lookupKey, []byte(storageID), 0); err != nil {
				return fmt.Errorf("store repo lookup: %w", err)
			}
		}

		// Store replica metadata
		for _, pr := range prepared {
			// Store in metadata bucket (for serving during failover)
			if err := tx.Put(bucketMetadata, pr.metadataKey, pr.metadataVal, 0); err != nil {
				s.logger.Error("failed to store replica metadata", "error", err, "path", pr.filePath)
				filesFailed++
				continue
			}

			// Store in replica tracking bucket (for failover lookup)
			if err := tx.Put(bucketReplicaFiles, pr.replicaKey, pr.replicaVal, 0); err != nil {
				s.logger.Error("failed to store replica tracking", "error", err, "path", pr.filePath)
				filesFailed++
				continue
			}

			filesReplicated++
		}

		return nil
	})

	if err != nil {
		s.logger.Error("replica batch transaction failed", "error", err)
		return &pb.IngestReplicaBatchResponse{
			Success:         false,
			FilesReplicated: filesReplicated,
			FilesFailed:     int64(len(req.Files)) - filesReplicated,
			ErrorMessage:    err.Error(),
		}, err
	}

	s.logger.Info("replica batch completed",
		"files_replicated", filesReplicated,
		"files_failed", filesFailed,
		"primary_node", primaryNodeID)

	return &pb.IngestReplicaBatchResponse{
		Success:         true,
		FilesReplicated: filesReplicated,
		FilesFailed:     filesFailed,
	}, nil
}

// Authenticate implements the Authenticate RPC.
func (s *Server) Authenticate(ctx context.Context, req *pb.AuthRequest) (*pb.AuthResponse, error) {
	s.logger.Debug("authenticate", "token_len", len(req.Token))

	// Stub: accept any token
	return &pb.AuthResponse{
		Success:   true,
		SessionId: "session-" + s.nodeID,
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix(),
	}, nil
}

// GetNodeInfo implements the GetNodeInfo RPC.
// This is called frequently (every 10s) by the router for health checks,
// so we use a cached atomic counter instead of scanning the database.
func (s *Server) GetNodeInfo(ctx context.Context, req *pb.NodeInfoRequest) (*pb.NodeInfoResponse, error) {
	// Get filesystem stats for actual total and free space
	var diskTotal, diskFree int64
	var stat syscall.Statfs_t
	if err := syscall.Statfs(s.dbPath, &stat); err == nil {
		diskTotal = int64(stat.Blocks) * int64(stat.Bsize)
		diskFree = int64(stat.Bavail) * int64(stat.Bsize) // Available to unprivileged users
	}

	// Actual disk usage: database only (blobs are on fetchers now)
	var diskUsed int64
	if size, err := dirSize(s.dbPath); err == nil {
		diskUsed += size
	} else {
		return nil, fmt.Errorf("failed to measure db usage: %w", err)
	}

	return &pb.NodeInfoResponse{
		NodeId:         s.nodeID,
		Address:        s.address,
		UptimeSeconds:  int64(time.Since(s.startTime).Seconds()),
		FilesServed:    s.filesServed.Load(),
		TotalFiles:     s.totalFiles.Load(),
		DiskUsedBytes:  diskUsed,
		DiskTotalBytes: diskTotal,
		DiskFreeBytes:  diskFree,
	}, nil
}

// ListRepositories returns all repository IDs stored on this node.
func (s *Server) ListRepositories(ctx context.Context, req *pb.ListRepositoriesRequest) (*pb.ListRepositoriesResponse, error) {
	var repoIDs []string

	err := s.db.View(func(tx *nutsdb.Tx) error {
		keys, err := tx.GetKeys(bucketRepos)
		if err != nil {
			if err == nutsdb.ErrBucketNotFound || err == nutsdb.ErrNotFoundKey {
				return nil
			}
			return err
		}

		for _, key := range keys {
			repoIDs = append(repoIDs, string(key))
		}
		return nil
	})

	if err != nil {
		s.logger.Error("failed to list repositories", "error", err)
		return nil, fmt.Errorf("failed to list repositories: %w", err)
	}

	s.logger.Debug("listed repositories", "count", len(repoIDs))
	return &pb.ListRepositoriesResponse{
		RepositoryIds: repoIDs,
	}, nil
}

// GetRepositoryInfo returns metadata for a specific repository.
func (s *Server) GetRepositoryInfo(ctx context.Context, req *pb.GetRepositoryInfoRequest) (*pb.GetRepositoryInfoResponse, error) {
	var repoInfoData repoInfo

	err := s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketRepos, []byte(req.StorageId))
		if err != nil {
			return err
		}
		return json.Unmarshal(value, &repoInfoData)
	})

	if err != nil {
		s.logger.Error("failed to get repository info", "storage_id", req.StorageId, "error", err)
		return nil, fmt.Errorf("repository not found: %w", err)
	}

	return &pb.GetRepositoryInfoResponse{
		StorageId:   repoInfoData.StorageID,
		DisplayPath: repoInfoData.DisplayPath,
		Source:      repoInfoData.RepoURL,
		Ref:         repoInfoData.Branch,
	}, nil
}

// GetOnboardingStatus returns onboarding status for all repositories on this node.
func (s *Server) GetOnboardingStatus(ctx context.Context, req *pb.OnboardingStatusRequest) (*pb.OnboardingStatusResponse, error) {
	repositories := make(map[string]bool)

	err := s.db.View(func(tx *nutsdb.Tx) error {
		keys, err := tx.GetKeys(bucketOnboardingStatus)
		if err != nil {
			if err == nutsdb.ErrBucketNotFound || err == nutsdb.ErrNotFoundKey {
				return nil
			}
			return err
		}

		for _, key := range keys {
			value, err := tx.Get(bucketOnboardingStatus, key)
			if err != nil {
				continue
			}
			storageID := string(key)
			onboarded := string(value) == "true"
			repositories[storageID] = onboarded
		}

		return nil
	})

	if err != nil {
		s.logger.Error("failed to get onboarding status", "error", err)
		return nil, fmt.Errorf("failed to get onboarding status: %w", err)
	}

	s.logger.Debug("returning onboarding status", "count", len(repositories))
	return &pb.OnboardingStatusResponse{
		Repositories: repositories,
	}, nil
}

// MarkRepositoryOnboarded marks a repository as fully onboarded.
func (s *Server) MarkRepositoryOnboarded(ctx context.Context, req *pb.MarkRepositoryOnboardedRequest) (*pb.MarkRepositoryOnboardedResponse, error) {
	err := s.db.Update(func(tx *nutsdb.Tx) error {
		return tx.Put(bucketOnboardingStatus, []byte(req.StorageId), []byte("true"), 0)
	})

	if err != nil {
		s.logger.Error("failed to mark repository onboarded",
			"storage_id", req.StorageId,
			"error", err)
		return &pb.MarkRepositoryOnboardedResponse{Success: false}, err
	}

	s.logger.Info("repository marked as onboarded", "storage_id", req.StorageId)
	return &pb.MarkRepositoryOnboardedResponse{Success: true}, nil
}

// NodeID returns the server's node ID.
func (s *Server) NodeID() string {
	return s.nodeID
}

// Close closes the server resources.
func (s *Server) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// hashPath generates a simple inode number from path.
func hashPath(path string) uint64 {
	if path == "" {
		return 1
	}
	var h uint64 = 5381
	for _, c := range path {
		h = h*33 + uint64(c)
	}
	return h
}

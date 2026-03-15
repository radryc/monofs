// Command monofs-fetcher runs the blob fetcher service.
//
// Fetchers are stateless proxies that handle external network access
// (Git remotes, etc.) on behalf of storage nodes.
// They run in the DMZ with external connectivity while storage nodes
// remain on internal network only.
//
// Features:
//   - Multi-backend support (Git optional, Blob default via packager archives)
//   - Local blob/repo cache for efficiency
//   - Background prefetch queue
//   - Repo-affinity for cache locality
//
// Configuration (in order of precedence):
//  1. CLI flags
//  2. Environment variables (MONOFS_ENCRYPTION_KEY, MONOFS_FETCHER_PORT, etc.)
//  3. Config file (--config or /etc/monofs/fetcher.json)
//  4. Built-in defaults
//
// Usage:
//
//	monofs-fetcher --port 9200 --cache-dir /data/fetcher-cache --encryption-key <hex-key>
//	monofs-fetcher --config /etc/monofs/fetcher.json
//	MONOFS_ENCRYPTION_KEY=<hex> monofs-fetcher
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/radryc/monofs/internal/fetcher"
	"github.com/radryc/monofs/internal/storage"
	"github.com/radryc/monofs/internal/storage/blob"
	storagegit "github.com/radryc/monofs/internal/storage/git"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

// fetcherConfig holds all fetcher configuration, loadable from JSON.
type fetcherConfig struct {
	Port            int           `json:"port"`
	CacheDir        string        `json:"cache_dir"`
	MaxCacheGB      int           `json:"max_cache_gb"`
	CacheAgeHours   int           `json:"cache_age_hours"`
	PrefetchWorkers int           `json:"prefetch_workers"`
	EncryptionKey   string        `json:"encryption_key"`
	EnableGit       bool          `json:"enable_git"`
	LogLevel        string        `json:"log_level"`
	Storage         storageConfig `json:"storage"`
}

type storageConfig struct {
	Type      string `json:"type"`       // "local" (default), "s3", or "gcs"
	LocalPath string `json:"local_path"` // path on disk for blob archives (also used as local cache for cloud)

	// S3 settings (used when type == "s3")
	S3Region          string `json:"s3_region"`
	S3Bucket          string `json:"s3_bucket"`
	S3Prefix          string `json:"s3_prefix"`
	S3Endpoint        string `json:"s3_endpoint"`      // for MinIO / S3-compatible
	S3AccessKeyID     string `json:"s3_access_key_id"` // empty = use default AWS credential chain
	S3SecretAccessKey string `json:"s3_secret_access_key"`
	S3SessionToken    string `json:"s3_session_token"`
	S3UsePathStyle    bool   `json:"s3_use_path_style"`

	// GCS settings (used when type == "gcs")
	GCSBucket          string `json:"gcs_bucket"`
	GCSPrefix          string `json:"gcs_prefix"`
	GCSCredentialsFile string `json:"gcs_credentials_file"` // empty = use ADC
}

// defaultConfig returns built-in defaults.
func defaultConfig() fetcherConfig {
	return fetcherConfig{
		Port:            9200,
		CacheDir:        "/data/fetcher-cache",
		MaxCacheGB:      50,
		CacheAgeHours:   2,
		PrefetchWorkers: 4,
		LogLevel:        "info",
		Storage: storageConfig{
			Type: "local",
		},
	}
}

// loadConfigFile reads a JSON config file, returning defaults on missing file.
func loadConfigFile(path string) (fetcherConfig, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // missing file → use defaults
		}
		return cfg, fmt.Errorf("reading config %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config %s: %w", path, err)
	}
	return cfg, nil
}

func main() {
	// Flags (override config file values)
	configFile := flag.String("config", "", "Path to JSON config file (default: /etc/monofs/fetcher.json)")
	port := flag.Int("port", 0, "gRPC server port (default: 9200)")
	cacheDir := flag.String("cache-dir", "", "Directory for caching repos/modules")
	maxCacheGB := flag.Int("max-cache-gb", 0, "Maximum cache size in GB")
	cacheAgeHours := flag.Int("cache-age-hours", 0, "Max age for cached repos before eviction")
	prefetchWorkers := flag.Int("prefetch-workers", 0, "Number of background prefetch workers")
	encryptionKeyHex := flag.String("encryption-key", "", "32-byte hex-encoded encryption key for packager archives")
	enableGit := flag.Bool("enable-git", false, "Enable optional Git backend")
	logLevel := flag.String("log-level", "", "Log level (debug, info, warn, error)")
	flag.Parse()

	// Load config: file → env → flags (flags win)
	cfgPath := *configFile
	if cfgPath == "" {
		cfgPath = os.Getenv("MONOFS_FETCHER_CONFIG")
	}
	if cfgPath == "" {
		cfgPath = "/etc/monofs/fetcher.json"
	}
	cfg, err := loadConfigFile(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	// Apply environment variables (override config file)
	if v := os.Getenv("MONOFS_ENCRYPTION_KEY"); v != "" {
		cfg.EncryptionKey = v
	}
	if v := os.Getenv("MONOFS_FETCHER_PORT"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.Port)
	}
	if v := os.Getenv("MONOFS_FETCHER_CACHE_DIR"); v != "" {
		cfg.CacheDir = v
	}
	if v := os.Getenv("MONOFS_FETCHER_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}

	// Apply S3 configuration from environment variables
	if v := os.Getenv("MONOFS_S3_REGION"); v != "" {
		cfg.Storage.S3Region = v
	}
	if v := os.Getenv("MONOFS_S3_BUCKET"); v != "" {
		cfg.Storage.S3Bucket = v
	}
	if v := os.Getenv("MONOFS_S3_PREFIX"); v != "" {
		cfg.Storage.S3Prefix = v
	}
	if v := os.Getenv("MONOFS_S3_ENDPOINT"); v != "" {
		cfg.Storage.S3Endpoint = v
	}
	if v := os.Getenv("MONOFS_S3_ACCESS_KEY_ID"); v != "" {
		cfg.Storage.S3AccessKeyID = v
	}
	if v := os.Getenv("MONOFS_S3_SECRET_ACCESS_KEY"); v != "" {
		cfg.Storage.S3SecretAccessKey = v
	}
	if v := os.Getenv("MONOFS_S3_USE_PATH_STYLE"); v != "" {
		cfg.Storage.S3UsePathStyle = v == "true" || v == "1"
	}

	// Apply CLI flags (highest precedence)
	if *port != 0 {
		cfg.Port = *port
	}
	if *cacheDir != "" {
		cfg.CacheDir = *cacheDir
		// Re-derive storage path from the new cache dir so --cache-dir always
		// governs where archives land, overriding any path set in the config file.
		cfg.Storage.LocalPath = ""
	}
	if *maxCacheGB != 0 {
		cfg.MaxCacheGB = *maxCacheGB
	}
	if *cacheAgeHours != 0 {
		cfg.CacheAgeHours = *cacheAgeHours
	}
	if *prefetchWorkers != 0 {
		cfg.PrefetchWorkers = *prefetchWorkers
	}
	if *encryptionKeyHex != "" {
		cfg.EncryptionKey = *encryptionKeyHex
	}
	if *enableGit {
		cfg.EnableGit = true
	}
	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}

	// Derive blob storage path from cache dir if not set explicitly
	if cfg.Storage.LocalPath == "" {
		cfg.Storage.LocalPath = cfg.CacheDir + "/blobs"
	}

	// Setup logger
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Generate fetcher ID
	hostname, _ := os.Hostname()
	fetcherID := fmt.Sprintf("fetcher-%s-%d", hostname, cfg.Port)

	logger.Info("starting monofs-fetcher",
		"id", fetcherID,
		"port", cfg.Port,
		"cache_dir", cfg.CacheDir,
		"blob_storage", cfg.Storage.LocalPath,
	)

	// Ensure cache directory exists
	if err := os.MkdirAll(cfg.CacheDir, 0755); err != nil {
		logger.Error("failed to create cache directory", "error", err)
		os.Exit(1)
	}

	// Parse encryption key
	var encryptionKey []byte
	if cfg.EncryptionKey != "" {
		var err error
		encryptionKey, err = hex.DecodeString(cfg.EncryptionKey)
		if err != nil || len(encryptionKey) != 32 {
			logger.Error("encryption key must be 32 bytes (64 hex chars)", "len", len(encryptionKey), "error", err)
			os.Exit(1)
		}
	} else {
		logger.Error("encryption key is required: set --encryption-key flag, MONOFS_ENCRYPTION_KEY env var, or encryption_key in config file")
		os.Exit(1)
	}

	// Create backend registry
	registry := fetcher.NewRegistry()

	// Initialize Blob backend (default, packager-based)
	blobBackend := blob.NewBlobBackend()
	if err := blobBackend.Initialize(ctx, fetcher.BackendConfig{
		CacheDir:      cfg.Storage.LocalPath,
		MaxCacheSize:  int64(cfg.MaxCacheGB) * 1024 * 1024 * 1024,
		Concurrency:   10,
		EncryptionKey: encryptionKey,
		StorageType:   storage.StorageType(cfg.Storage.Type),
		Cloud: storage.CloudStorageConfig{
			S3Region:           cfg.Storage.S3Region,
			S3Bucket:           cfg.Storage.S3Bucket,
			S3Prefix:           cfg.Storage.S3Prefix,
			S3Endpoint:         cfg.Storage.S3Endpoint,
			S3AccessKeyID:      cfg.Storage.S3AccessKeyID,
			S3SecretAccessKey:  cfg.Storage.S3SecretAccessKey,
			S3SessionToken:     cfg.Storage.S3SessionToken,
			S3UsePathStyle:     cfg.Storage.S3UsePathStyle,
			GCSBucket:          cfg.Storage.GCSBucket,
			GCSPrefix:          cfg.Storage.GCSPrefix,
			GCSCredentialsFile: cfg.Storage.GCSCredentialsFile,
		},
	}); err != nil {
		logger.Error("failed to initialize blob backend", "error", err)
		os.Exit(1)
	}
	blobBackend.SetLogger(logger)
	registry.Register(blobBackend)
	logger.Info("blob backend initialized",
		"storage_type", cfg.Storage.Type,
		"storage_path", cfg.Storage.LocalPath,
		"archives", blobBackend.ArchiveCount(),
	)

	// Optional Git backend
	if cfg.EnableGit {
		gitBackend := storagegit.NewGitBackend()
		if err := gitBackend.Initialize(ctx, fetcher.BackendConfig{
			CacheDir:        cfg.CacheDir + "/git",
			MaxCacheSize:    int64(cfg.MaxCacheGB) * 1024 * 1024 * 1024 / 4,
			MaxCacheAgeSecs: int64(cfg.CacheAgeHours) * 3600,
			Concurrency:     10,
		}); err != nil {
			logger.Error("failed to initialize git backend", "error", err)
			os.Exit(1)
		}
		registry.Register(gitBackend)
		logger.Info("git backend initialized", "cached_repos", len(gitBackend.CachedSources()))
	}

	// Create fetcher service
	serviceConfig := fetcher.DefaultServiceConfig()
	serviceConfig.PrefetchWorkers = cfg.PrefetchWorkers
	service := fetcher.NewService(fetcherID, registry, serviceConfig, logger)

	// Create gRPC server
	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(100*1024*1024), // 100MB max message
		grpc.MaxSendMsgSize(100*1024*1024),
	)
	service.RegisterService(grpcServer)
	reflection.Register(grpcServer) // For debugging

	// Start listening
	addr := fmt.Sprintf(":%d", cfg.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("failed to listen", "address", addr, "error", err)
		os.Exit(1)
	}

	// Handle shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		logger.Info("shutting down...")

		// Graceful shutdown with timeout
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()

		// Stop accepting new connections
		grpcServer.GracefulStop()

		// Close service
		if err := service.Close(); err != nil {
			logger.Warn("error closing service", "error", err)
		}

		// Close backends
		if err := registry.Close(); err != nil {
			logger.Warn("error closing backends", "error", err)
		}

		cancel()
		_ = shutdownCtx // Satisfy linter
	}()

	// Start stats reporter
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				stats, _ := service.GetStats(ctx, nil)
				if stats != nil {
					logger.Info("fetcher stats",
						"total_requests", stats.TotalRequests,
						"active_fetches", stats.ActiveFetches,
						"queued_prefetches", stats.QueuedPrefetches,
						"cache_hit_rate", fmt.Sprintf("%.2f%%", stats.CacheHitRate*100),
						"bytes_served", formatBytes(stats.BytesServed),
					)
				}
			}
		}
	}()

	logger.Info("fetcher ready", "address", addr)
	if err := grpcServer.Serve(listener); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

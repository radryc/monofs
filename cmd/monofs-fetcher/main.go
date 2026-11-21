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
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/radryc/monofs/internal/diagnostics"
	"github.com/radryc/monofs/internal/fetcher"
	"github.com/radryc/monofs/internal/storage"
	"github.com/radryc/monofs/internal/storage/blob"
	"github.com/radryc/monofs/internal/storage/blob/layout"
	storagegit "github.com/radryc/monofs/internal/storage/git"
	"github.com/radryc/monofs/internal/telemetry"
	"github.com/radryc/monofs/internal/transport"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
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

	CloudScannerIntervalSecs int64 `json:"cloud_scanner_interval_secs"` // >0 overrides default 3600s (1h)
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
	diagnosticsAddr := flag.String("diagnostics-addr", ":9201", "Listen address for Prometheus /metrics (pprof only when MONOFS_DIAGNOSTICS_PPROF=true; empty disables)")
	logLevel := flag.String("log-level", "", "Log level (debug, info, warn, error)")
	blobLayout := flag.String("blob-layout", "", "Blob layout: v2 (default, content-addressed sharded packs) or legacy (per-object archives)")
	blobCompactor := flag.Bool("blob-compactor", false, "Run the background v2 pack compactor")
	blobCompactorInterval := flag.Duration("blob-compactor-interval", 5*time.Minute, "Interval between v2 compaction cycles")
	blobCompactorGrace := flag.Duration("blob-compactor-grace", time.Hour, "Grace period before retired packs are deleted")
	blobCompactorMaxShards := flag.Int("blob-compactor-max-shards", layout.ShardCount, "Shards compacted per cycle")
	blobCompactorTargetBytes := flag.Int64("blob-compactor-target-bytes", 512*1024*1024, "Target live bytes per compacted pack")
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

	tlsTransport, err := transport.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load transport config: %v\n", err)
		os.Exit(1)
	}

	// Apply environment variables (override config file)
	if v := os.Getenv("MONOFS_ENCRYPTION_KEY"); v != "" {
		cfg.EncryptionKey = v
	}
	if v := os.Getenv("MONOFS_FETCHER_PORT"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &cfg.Port); err != nil {
			fmt.Fprintf(os.Stderr, "warning: invalid MONOFS_FETCHER_PORT %q: %v\n", v, err)
		}
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
	if v := os.Getenv("MONOFS_CLOUD_SCANNER_INTERVAL_SECS"); v != "" {
		var secs int64
		if _, err := fmt.Sscanf(v, "%d", &secs); err == nil {
			cfg.Storage.CloudScannerIntervalSecs = secs
		}
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
	encryptionKeySource := "config-file"
	if *encryptionKeyHex != "" {
		cfg.EncryptionKey = *encryptionKeyHex
		encryptionKeySource = "cli-flag"
	}
	if os.Getenv("MONOFS_ENCRYPTION_KEY") != "" {
		encryptionKeySource = "env-MONOFS_ENCRYPTION_KEY"
	}
	if *enableGit {
		cfg.EnableGit = true
	}
	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}
	telemetryCfg, err := telemetry.LoadConfig("monofs-fetcher")
	if err != nil {
		fmt.Fprintf(os.Stderr, "load telemetry config: %v\n", err)
		os.Exit(1)
	}
	telemetryHandle, err := telemetry.Setup(context.Background(), telemetryCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup telemetry: %v\n", err)
		os.Exit(1)
	}
	if telemetryHandle.Enabled() {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := telemetryHandle.Shutdown(shutdownCtx); err != nil {
				fmt.Fprintf(os.Stderr, "shutdown telemetry: %v\n", err)
			}
		}()
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
	var handler slog.Handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	if telemetryHandle.Enabled() {
		handler = telemetry.WrapSlogHandler(handler, "monofs.fetcher")
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	if telemetryHandle.Enabled() {
		telemetry.EmitInfo(context.Background(), "monofs.fetcher", "monofs fetcher telemetry enabled")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Generate fetcher ID
	hostname, _ := os.Hostname()
	fetcherID := fmt.Sprintf("fetcher-%s-%d", hostname, cfg.Port)

	logger.Info("starting monofs-fetcher",
		"id", fetcherID,
		"port", cfg.Port,
		"diagnostics_addr", *diagnosticsAddr,
		"cache_dir", cfg.CacheDir,
		"blob_storage", cfg.Storage.LocalPath,
	)

	// Set up diagnostics server with custom key-guard routes so operators can
	// see whether the encryption key has changed and confirm it explicitly.
	diagMux := diagnostics.NewHandler()
	diagServer := diagnostics.StartServerWithMux(logger, "monofs-fetcher", strings.TrimSpace(*diagnosticsAddr), diagMux)
	defer diagnostics.ShutdownServer(logger, "monofs-fetcher", diagServer)

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

	// Blob layout: flag > env > v2. The backend reads this env during
	// Initialize, so resolve it before constructing the backend.
	layoutMode := strings.ToLower(strings.TrimSpace(*blobLayout))
	if layoutMode == "" {
		switch strings.ToLower(strings.TrimSpace(os.Getenv("MONOFS_BLOB_LAYOUT_V2"))) {
		case "false", "0", "no", "off":
			layoutMode = "legacy"
		default:
			layoutMode = "v2"
		}
	}
	if layoutMode == "v2" {
		_ = os.Setenv("MONOFS_BLOB_LAYOUT_V2", "true")
	} else {
		_ = os.Setenv("MONOFS_BLOB_LAYOUT_V2", "false")
	}

	compactorEnabled := *blobCompactor
	if v := strings.TrimSpace(os.Getenv("MONOFS_BLOB_COMPACT_ENABLED")); v != "" {
		compactorEnabled = v == "true" || v == "1"
	}
	compactorInterval := *blobCompactorInterval
	if v := strings.TrimSpace(os.Getenv("MONOFS_BLOB_COMPACT_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			compactorInterval = d
		}
	}

	// Create backend registry
	registry := fetcher.NewRegistry()

	// Initialize Blob backend (default, packager-based)
	blobBackend := blob.NewBlobBackend()
	if err := blobBackend.Initialize(ctx, fetcher.BackendConfig{
		CacheDir:            cfg.Storage.LocalPath,
		MaxCacheSize:        int64(cfg.MaxCacheGB) * 1024 * 1024 * 1024,
		Concurrency:         10,
		EncryptionKey:       encryptionKey,
		EncryptionKeySource: encryptionKeySource,
		StorageType:         storage.StorageType(cfg.Storage.Type),
		Cloud: storage.CloudStorageConfig{
			S3Region:                 cfg.Storage.S3Region,
			S3Bucket:                 cfg.Storage.S3Bucket,
			S3Prefix:                 cfg.Storage.S3Prefix,
			S3Endpoint:               cfg.Storage.S3Endpoint,
			S3AccessKeyID:            cfg.Storage.S3AccessKeyID,
			S3SecretAccessKey:        cfg.Storage.S3SecretAccessKey,
			S3SessionToken:           cfg.Storage.S3SessionToken,
			S3UsePathStyle:           cfg.Storage.S3UsePathStyle,
			GCSBucket:                cfg.Storage.GCSBucket,
			GCSPrefix:                cfg.Storage.GCSPrefix,
			GCSCredentialsFile:       cfg.Storage.GCSCredentialsFile,
			CloudScannerIntervalSecs: cfg.Storage.CloudScannerIntervalSecs,
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
		"layout", blobBackend.LayoutMode(),
	)

	if compactorEnabled && blobBackend.LayoutMode() == "v2" && cfg.Storage.Type == "s3" {
		compactorCfg := layout.CompactionConfig{
			TargetBytes:      *blobCompactorTargetBytes,
			MaxPacksPerShard: 2,
			DeadRatio:        0.30,
			GracePeriod:      *blobCompactorGrace,
			MaxShardsPerRun:  *blobCompactorMaxShards,
			MaxBytesPerRun:   8 * 1024 * 1024 * 1024,
			RetryLimit:       5,
		}
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			ticker := time.NewTicker(compactorInterval)
			defer ticker.Stop()
			for {
				res, err := blobBackend.RunCompactor(ctx, compactorCfg)
				if err != nil {
					logger.Warn("blob compaction failed", "error", err)
				} else if res.ShardsScanned > 0 {
					logger.Info("blob compaction",
						"shards", res.ShardsScanned,
						"plans", res.PlansApplied,
						"retired", res.PacksRetired,
						"deleted", res.PacksDeleted,
						"bytes_rewritten", res.BytesRewritten,
					)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
		logger.Info("blob compactor enabled",
			"interval", compactorInterval,
			"grace", *blobCompactorGrace,
			"max_shards", *blobCompactorMaxShards,
		)
	}

	// Register encryption-key guard diagnostics endpoints.
	diagMux.HandleFunc("/key-status", func(w http.ResponseWriter, r *http.Request) {
		status := blobBackend.EncryptionKeyStatus()
		w.Header().Set("Content-Type", "application/json")
		if status.Pending {
			w.WriteHeader(http.StatusLocked) // 423: resource locked until confirmed
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_ = json.NewEncoder(w).Encode(status)
	})
	diagMux.HandleFunc("/confirm-key", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		status, err := blobBackend.ConfirmEncryptionKey()
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status})
	})

	// Expose objects stored in the configured backend (S3/GCS/local archives).
	diagMux.HandleFunc("/storage-objects", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		objects, err := blobBackend.ListStorageObjects(ctx)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"objects": objects,
				"error":   err.Error(),
				"healthy": false,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"objects": objects,
			"error":   "",
			"healthy": true,
		})
	})

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
	serviceConfig.SyncRepoCacheDir = cfg.CacheDir + "/sync"
	service := fetcher.NewService(fetcherID, registry, serviceConfig, logger)

	// Expose WAL and S3 read-through state to the router UI.
	diagMux.HandleFunc("/observability", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(service.Observability())
	})

	// Create gRPC server
	transportServerOpt, err := tlsTransport.ServerOption()
	if err != nil {
		logger.Error("failed to configure transport security", "error", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer(
		transportServerOpt,
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.MaxRecvMsgSize(100*1024*1024),
		grpc.MaxSendMsgSize(100*1024*1024),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    2 * time.Minute,
			Timeout: 20 * time.Second,
		}),
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

	logger.Info("fetcher ready", "address", addr)
	if err := grpcServer.Serve(listener); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
}

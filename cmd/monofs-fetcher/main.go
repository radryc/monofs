// Command monofs-fetcher runs the blob fetcher service.
//
// Fetchers are stateless proxies that handle external network access
// (Git remotes, Go module proxies, etc.) on behalf of storage nodes.
// They run in the DMZ with external connectivity while storage nodes
// remain on internal network only.
//
// Features:
//   - Multi-backend support (Git, Go modules, extensible)
//   - Local repo/module cache for efficiency
//   - Background prefetch queue
//   - Repo-affinity for cache locality
//
// Usage:
//
//	monofs-fetcher --port 9200 --cache-dir /data/fetcher-cache
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/radryc/monofs/internal/fetcher"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

func main() {
	// Flags
	port := flag.Int("port", 9200, "gRPC server port")
	cacheDir := flag.String("cache-dir", "/data/fetcher-cache", "Directory for caching repos/modules")
	maxCacheGB := flag.Int("max-cache-gb", 50, "Maximum cache size in GB")
	cacheAgeHours := flag.Int("cache-age-hours", 2, "Max age for cached repos before eviction")
	prefetchWorkers := flag.Int("prefetch-workers", 4, "Number of background prefetch workers")
	goModProxy := flag.String("gomod-proxy", "https://proxy.golang.org", "Go module proxy URL")
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	flag.Parse()

	// Setup logger
	var level slog.Level
	switch *logLevel {
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
	fetcherID := fmt.Sprintf("fetcher-%s-%d", hostname, *port)

	logger.Info("starting monofs-fetcher",
		"id", fetcherID,
		"port", *port,
		"cache_dir", *cacheDir,
	)

	// Ensure cache directory exists
	if err := os.MkdirAll(*cacheDir, 0755); err != nil {
		logger.Error("failed to create cache directory", "error", err)
		os.Exit(1)
	}

	// Create backend registry
	registry := fetcher.NewRegistry()

	// Initialize Git backend
	gitBackend := fetcher.NewGitBackend()
	if err := gitBackend.Initialize(ctx, fetcher.BackendConfig{
		CacheDir:        *cacheDir + "/git",
		MaxCacheSize:    int64(*maxCacheGB) * 1024 * 1024 * 1024 / 2, // Half for Git
		MaxCacheAgeSecs: int64(*cacheAgeHours) * 3600,
		Concurrency:     10,
	}); err != nil {
		logger.Error("failed to initialize git backend", "error", err)
		os.Exit(1)
	}
	registry.Register(gitBackend)
	logger.Info("git backend initialized", "cached_repos", len(gitBackend.CachedSources()))

	// Initialize Go modules backend
	gomodBackend := fetcher.NewGoModBackend()
	if err := gomodBackend.Initialize(ctx, fetcher.BackendConfig{
		CacheDir:        *cacheDir + "/gomod",
		MaxCacheSize:    int64(*maxCacheGB) * 1024 * 1024 * 1024 / 2, // Half for GoMod
		MaxCacheAgeSecs: int64(*cacheAgeHours) * 3600,
		Concurrency:     10,
		Extra: map[string]string{
			"proxy_url": *goModProxy,
		},
	}); err != nil {
		logger.Error("failed to initialize gomod backend", "error", err)
		os.Exit(1)
	}
	registry.Register(gomodBackend)
	logger.Info("gomod backend initialized", "cached_modules", len(gomodBackend.CachedSources()))

	// Create fetcher service
	serviceConfig := fetcher.DefaultServiceConfig()
	serviceConfig.PrefetchWorkers = *prefetchWorkers
	service := fetcher.NewService(fetcherID, registry, serviceConfig, logger)

	// Create gRPC server
	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(100*1024*1024), // 100MB max message
		grpc.MaxSendMsgSize(100*1024*1024),
	)
	service.RegisterService(grpcServer)
	reflection.Register(grpcServer) // For debugging

	// Start listening
	addr := fmt.Sprintf(":%d", *port)
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

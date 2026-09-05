// monofs-cache is a Bazel-compatible remote cache server backed by
// local filesystem storage. It implements the Bazel Remote Cache
// HTTP/1.1 protocol for both the Action Cache (AC) and
// Content-Addressable Storage (CAS).
//
// Usage:
//
//	monofs-cache --port=9092 --dir=/data/cache
//
// Bazel .bazelrc:
//
//	build:remote-cache --remote_cache=http://monofs-cache:9092
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/radryc/monofs/internal/bazel/cache"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	port := flag.Int("port", 9092, "HTTP port")
	dir := flag.String("dir", "/data/cache", "Cache data directory")
	maxCASMB := flag.Int("max-cas-mb", 512, "Maximum CAS blob size in MB")
	evictionMaxAgeDays := flag.Int("eviction-max-age-days", 30, "Evict CAS blobs older than N days")
	evictionMaxBytesGB := flag.Int("eviction-max-bytes-gb", 100, "Soft limit on total CAS bytes in GB")
	debug := flag.Bool("debug", false, "Enable debug logging")
	showVersion := flag.Bool("version", false, "Show version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("monofs-cache version=%s commit=%s build_time=%s\n", Version, Commit, BuildTime)
		return
	}

	// Logging
	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))

	logger.Info("starting monofs-cache",
		"version", Version,
		"port", *port,
		"dir", *dir,
	)

	// Create store.
	opts := cache.DefaultStoreOptions(*dir)
	opts.MaxCASBlobSize = int64(*maxCASMB) * 1024 * 1024
	store, err := cache.NewStore(opts)
	if err != nil {
		logger.Error("create store", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	// Create server.
	srv := cache.NewServer(store, logger)

	// Start eviction.
	evictionCfg := cache.DefaultEvictionConfig()
	evictionCfg.MaxAge = time.Duration(*evictionMaxAgeDays) * 24 * time.Hour
	evictionCfg.MaxBytes = int64(*evictionMaxBytesGB) * 1024 * 1024 * 1024
	ctx, cancelEviction := context.WithCancel(context.Background())
	defer cancelEviction()
	srv.RunEviction(ctx, evictionCfg)

	// HTTP server
	addr := fmt.Sprintf(":%d", *port)
	httpSrv := &http.Server{
		Addr:         addr,
		Handler:      srv.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second, // CAS PUT can take time for large blobs
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cancelEviction()
		httpSrv.Shutdown(shutdownCtx)
	}()

	logger.Info("listening", "addr", addr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}

func init() {
	// Strip "monofs/" prefix from default log source.
	_ = strings.TrimPrefix
}

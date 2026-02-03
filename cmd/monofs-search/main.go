// Package main implements the MonoFS search service.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/search"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	// Parse flags
	port := flag.Int("port", 9100, "gRPC port")
	indexDir := flag.String("index-dir", "/data/index", "Directory for Zoekt indexes")
	cacheDir := flag.String("cache-dir", "/data/cache", "Directory for git clones during indexing")
	workers := flag.Int("workers", 2, "Number of concurrent indexing workers")
	queueSize := flag.Int("queue-size", 100, "Size of indexing job queue")
	routerAddr := flag.String("router-addr", "", "Router address for cluster access (optional, enables fetching from storage nodes)")
	debug := flag.Bool("debug", false, "Enable debug logging")
	showVersion := flag.Bool("version", false, "Show version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("monofs-search %s (commit: %s, built: %s)\n", Version, Commit, BuildTime)
		os.Exit(0)
	}

	// Setup logging
	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))

	logger.Info("starting monofs-search",
		"version", Version,
		"commit", Commit,
		"port", *port,
		"index_dir", *indexDir,
		"cache_dir", *cacheDir,
		"workers", *workers)

	// Create search service
	cfg := search.Config{
		IndexDir:   *indexDir,
		CacheDir:   *cacheDir,
		Workers:    *workers,
		QueueSize:  *queueSize,
		RouterAddr: *routerAddr,
		Logger:     logger,
	}

	svc, err := search.NewService(cfg)
	if err != nil {
		logger.Error("failed to create search service", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	// Create gRPC server
	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(100*1024*1024), // 100MB max message size
		grpc.MaxSendMsgSize(100*1024*1024),
	)

	// Register services
	pb.RegisterMonoFSSearchServer(grpcServer, svc)

	// Health check
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("monofs.MonoFSSearch", grpc_health_v1.HealthCheckResponse_SERVING)

	// Reflection for debugging
	reflection.Register(grpcServer)

	// Listen
	addr := fmt.Sprintf(":%d", *port)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("failed to listen", "addr", addr, "error", err)
		os.Exit(1)
	}

	// Handle shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		logger.Info("received signal, shutting down", "signal", sig)
		healthServer.SetServingStatus("monofs.MonoFSSearch", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
		grpcServer.GracefulStop()
	}()

	logger.Info("gRPC server listening", "addr", addr)
	if err := grpcServer.Serve(lis); err != nil {
		logger.Error("gRPC server failed", "error", err)
		os.Exit(1)
	}
}

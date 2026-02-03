// MonoFS Server - gRPC backend for MonoFS filesystem
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
)

var (
	// Version information (injected at build time)
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// stringSlice is a flag that can be specified multiple times
type stringSlice []string

func (s *stringSlice) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func main() {
	addr := flag.String("addr", ":9000", "Server listen address")
	nodeID := flag.String("node-id", "", "Unique node identifier (required)")
	routerAddr := flag.String("router", "", "Router address for failover coordination (optional)")
	dbPath := flag.String("db-path", "/tmp/monofs-db", "NutsDB database path")
	gitCache := flag.String("git-cache", "/tmp/monofs-git-cache", "Git repository cache directory")
	debug := flag.Bool("debug", false, "Enable debug logging")

	// Fetcher configuration
	var fetcherAddrs stringSlice
	flag.Var(&fetcherAddrs, "fetcher", "Fetcher service address (can be specified multiple times)")
	enablePrediction := flag.Bool("enable-prediction", false, "Enable access pattern prediction and prefetching")

	flag.Parse()

	if *nodeID == "" {
		fmt.Fprintln(os.Stderr, "Error: --node-id is required")
		flag.Usage()
		os.Exit(1)
	}

	// Setup logger
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))
	slog.SetDefault(logger)

	logger.Info("starting monofs-server",
		"version", Version,
		"commit", Commit,
		"build_time", BuildTime,
		"addr", *addr,
		"node_id", *nodeID)

	// Create listener
	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Error("failed to listen", "error", err)
		os.Exit(1)
	}

	// Create gRPC server
	grpcServer := grpc.NewServer()

	// Create server with NutsDB backend
	srv, err := server.NewServer(*nodeID, *addr, *dbPath, *gitCache, logger)
	if err != nil {
		logger.Error("failed to create server", "error", err)
		os.Exit(1)
	}

	// Configure fetcher client and prediction if enabled
	if len(fetcherAddrs) > 0 && *enablePrediction {
		if err := srv.ConfigureFetcher([]string(fetcherAddrs)); err != nil {
			logger.Warn("failed to configure fetcher client, continuing without prediction",
				"error", err)
		} else {
			logger.Info("prediction and prefetching enabled",
				"fetcher_count", len(fetcherAddrs),
				"fetcher_addrs", fetcherAddrs)
		}
	} else if len(fetcherAddrs) > 0 {
		logger.Info("fetcher addresses provided but prediction not enabled, use --enable-prediction to enable")
	}

	srv.Register(grpcServer)

	// Enable reflection for debugging with grpcurl
	reflection.Register(grpcServer)

	// Start serving in background
	go func() {
		logger.Info("server listening", "addr", *addr)
		if err := grpcServer.Serve(lis); err != nil {
			logger.Error("server error", "error", err)
		}
	}()

	// Handle shutdown with graceful failover
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	sig := <-sigCh
	logger.Info("received signal, initiating graceful shutdown", "signal", sig)

	// Attempt graceful failover if router address is configured
	if *routerAddr != "" {
		if err := requestFailover(*routerAddr, *nodeID, logger); err != nil {
			logger.Warn("failover request failed", "error", err)
		} else {
			logger.Info("failover completed successfully")
		}
	}

	// Close server resources
	srv.Close()

	// Graceful stop with timeout
	stopCh := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopCh)
	}()

	select {
	case <-stopCh:
		logger.Info("server stopped gracefully")
	case <-time.After(30 * time.Second):
		logger.Warn("graceful shutdown timeout, forcing stop")
		grpcServer.Stop()
	}

	logger.Info("server stopped")
}

func requestFailover(routerAddr, nodeID string, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	logger.Info("connecting to router for failover", "router", routerAddr)

	conn, err := grpc.DialContext(ctx, routerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock())
	if err != nil {
		return fmt.Errorf("failed to connect to router: %w", err)
	}
	defer conn.Close()

	client := pb.NewMonoFSRouterClient(conn)

	resp, err := client.RequestFailover(ctx, &pb.FailoverRequest{
		SourceNodeId: nodeID,
		Timestamp:    time.Now().Unix(),
	})
	if err != nil {
		return fmt.Errorf("failover RPC failed: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("failover rejected: %s", resp.Message)
	}

	logger.Info("failover accepted",
		"target_node", resp.TargetNodeId,
		"message", resp.Message)

	return nil
}

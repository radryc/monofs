// MonoFS Client - FUSE filesystem client for MonoFS
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/radryc/monofs/internal/cache"
	"github.com/radryc/monofs/internal/client"
	monofuse "github.com/radryc/monofs/internal/fuse"
)

func main() {
	routerAddr := flag.String("router", "localhost:9090", "MonoFS router address")
	mountpoint := flag.String("mount", "", "Mount point (required)")
	cacheDir := flag.String("cache", "", "Cache directory (optional, disables cache if empty)")
	overlayDir := flag.String("overlay", "", "Override default overlay storage location (~/.monofs/overlay)")
	writable := flag.Bool("writable", false, "Enable write support (changes stored client-side)")
	debug := flag.Bool("debug", false, "Enable debug logging")
	keepCache := flag.Bool("keep-cache", false, "Keep existing cache on mount (default: clear cache)")
	rpcTimeout := flag.Duration("rpc-timeout", 10*time.Second, "Timeout for RPC calls to nodes")
	flag.Parse()

	if *mountpoint == "" {
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

	// Determine overlay directory - default to ~/.monofs/overlay if writable enabled
	if *writable && *overlayDir == "" {
		homeDir, _ := os.UserHomeDir()
		*overlayDir = filepath.Join(homeDir, ".monofs", "overlay")
	}
	// If overlay specified without --writable, enable writable mode
	if *overlayDir != "" {
		*writable = true
	}

	logger.Info("starting monofs-client",
		"router", *routerAddr,
		"mount", *mountpoint,
		"cache", *cacheDir,
		"overlay", *overlayDir,
		"writable", *writable,
	)

	// Connect to router and create sharded client
	ctx := context.Background()
	c, err := client.NewShardedClient(ctx, client.ShardedClientConfig{
		RouterAddr:           *routerAddr,
		RefreshInterval:      30 * time.Second,
		RPCTimeout:           *rpcTimeout,
		UseExternalAddresses: false, // Use internal Docker network addresses
		Logger:               logger,
		MountPoint:           *mountpoint,
		Writable:             *writable,
	})
	if err != nil {
		logger.Warn("failed to connect to router, will retry in background", "error", err)
		// Create client in disconnected state - it will retry connections
		c = client.NewDisconnectedClient(client.ShardedClientConfig{
			RouterAddr:      *routerAddr,
			RefreshInterval: 30 * time.Second,
			Logger:          logger,
			MountPoint:      *mountpoint,
			Writable:        *writable,
		})
	} else {
		logger.Info("connected to cluster", "healthy_nodes", len(c.GetHealthyNodes()))
	}
	defer c.Close()

	// Setup cache if directory provided
	var cacheLayer *cache.Cache
	if *cacheDir != "" {
		// Clear cache by default unless --keep-cache is specified
		if !*keepCache {
			logger.Info("clearing cache directory", "dir", *cacheDir)
			if err := os.RemoveAll(*cacheDir); err != nil {
				logger.Warn("failed to clear cache directory", "error", err)
			}
		}

		cacheLayer, err = cache.New(*cacheDir, logger)
		if err != nil {
			logger.Warn("failed to initialize cache, continuing without cache", "error", err)
			cacheLayer = nil
		} else {
			defer cacheLayer.Close()
		}
	}

	// Setup write support if enabled
	var sessionMgr *monofuse.SessionManager
	var commitMgr *monofuse.CommitManager
	var socketHandler *monofuse.SessionSocketHandler

	if *writable && *overlayDir != "" {
		logger.Info("enabling write support", "overlay", *overlayDir)

		sessionMgr, err = monofuse.NewSessionManager(*overlayDir, logger)
		if err != nil {
			logger.Error("failed to create session manager", "error", err)
			os.Exit(1)
		}

		commitMgr = monofuse.NewCommitManager(sessionMgr, c, logger)

		socketHandler, err = monofuse.NewSessionSocketHandler(*overlayDir, sessionMgr, commitMgr, logger)
		if err != nil {
			logger.Error("failed to create session socket", "error", err)
			os.Exit(1)
		}
		socketHandler.Start()
		defer socketHandler.Stop()

		logger.Info("write support enabled",
			"session_socket", filepath.Join(*overlayDir, "session.sock"))
	}

	// Create root node
	var root *monofuse.MonoNode
	if sessionMgr != nil {
		root = monofuse.NewRootWithSession(c, cacheLayer, sessionMgr, logger)
	} else {
		root = monofuse.NewRoot(c, cacheLayer, logger)
	}

	// Mount options
	attrTimeout := cache.DefaultAttrTTL
	entryTimeout := cache.DefaultAttrTTL
	opts := &fs.Options{
		MountOptions: fuse.MountOptions{
			Debug:      *debug,
			FsName:     "monofs",
			Name:       "monofs",
			AllowOther: true, // Allow all users to access the filesystem
		},
		// Cache settings for better performance
		AttrTimeout:  &attrTimeout,
		EntryTimeout: &entryTimeout,
	}

	// Mount filesystem
	server, err := fs.Mount(*mountpoint, root, opts)
	if err != nil {
		logger.Error("failed to mount", "error", err)
		os.Exit(1)
	}

	logger.Info("filesystem mounted", "mountpoint", *mountpoint, "writable", *writable)

	// Handle unmount on signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		logger.Info("received signal, unmounting", "signal", sig)
		if err := server.Unmount(); err != nil {
			logger.Error("unmount error", "error", err)
		}
	}()

	// Wait for unmount
	server.Wait()
	logger.Info("filesystem unmounted")
}

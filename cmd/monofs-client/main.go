// MonoFS Client - FUSE filesystem client for MonoFS
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/radryc/monofs/internal/cache"
	"github.com/radryc/monofs/internal/client"
	monofuse "github.com/radryc/monofs/internal/fuse"
)

var (
	// Version information (injected at build time)
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	routerAddr := flag.String("router", "localhost:9090", "MonoFS router address")
	mountpoint := flag.String("mount", "", "Mount point (required)")
	cacheDir := flag.String("cache", "", "Cache directory (optional, disables cache if empty)")
	overlayDir := flag.String("overlay", "", "Override default overlay storage location (~/.monofs/overlay)")
	useExternalAddrs := flag.Bool("use-external-addrs", false, "Request router-advertised external node addresses (for host/WSL clients)")
	writable := flag.Bool("writable", false, "Enable write support (changes stored client-side)")
	virtualMonorepo := flag.Bool("virtual-monorepo", false, "Project a source-first workspace root that keeps dependency visible but hides doctor, guardian, guardian-system, and nested .git paths")
	debug := flag.Bool("debug", false, "Enable MonoFS layer DEBUG logs (written to --log-file if set, else stdout)")
	fuseDebug := flag.Bool("fuse-debug", false, "Enable go-fuse C layer debug output (very verbose, written to <log-file>.fuse or stderr)")
	logFile := flag.String("log-file", "", "Path for structured JSON log file (DEBUG+). Stdout always gets INFO+ text.")
	keepCache := flag.Bool("keep-cache", false, "Keep existing cache on mount (default: clear cache)")
	rpcTimeout := flag.Duration("rpc-timeout", 10*time.Second, "Timeout for RPC calls to nodes")
	clientID := flag.String("client-id", "", "Persistent client identifier (default: auto-generated and stored in ~/.monofs/client-id)")
	showVersion := flag.Bool("version", false, "Print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("monofs-client version=%s commit=%s build_time=%s\n", Version, Commit, BuildTime)
		return
	}

	if *mountpoint == "" {
		flag.Usage()
		os.Exit(1)
	}

	// ------------------------------------------------------------------
	// Logging setup
	//
	// Our layer  → structured slog
	//   stdout   : text, INFO+  (always visible to operators / SSH sessions)
	//   log-file : JSON, DEBUG+ (machine-parseable, for post-mortem grep/jq)
	//   When --debug without --log-file: stdout promoted to DEBUG.
	//
	// go-fuse C layer uses stdlib log.Default() internally.  We redirect it
	// to a dedicated writer so it never interleaves with our slog output:
	//   --fuse-debug unset : io.Discard  (silence all C-layer chatter)
	//   --fuse-debug set   : <log-file>.fuse or stderr
	// ------------------------------------------------------------------
	logger := buildLogger(*debug, *logFile)
	slog.SetDefault(logger)

	log.SetOutput(buildFuseLogWriter(*logFile, *fuseDebug))
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// Determine overlay directory - default to /home/monofs/.monofs/overlay if writable enabled
	if *writable && *overlayDir == "" {
		*overlayDir = "/home/monofs/.monofs/overlay"
	}
	// If overlay specified without --writable, enable writable mode
	if *overlayDir != "" {
		*writable = true
	}
	workspaceGitStateDir := workspaceGitStateDir(*overlayDir)
	if err := validateClientPaths(*mountpoint, *overlayDir, *cacheDir, workspaceGitStateDir); err != nil {
		logger.Error("invalid client path configuration", "error", err)
		os.Exit(1)
	}
	visibleUID := uint32(os.Getuid())
	visibleGID := uint32(os.Getgid())
	if strings.TrimSpace(*overlayDir) != "" {
		uid, gid, ownerErr := monofuse.ResolvePathOwner(*overlayDir)
		if ownerErr != nil {
			logger.Warn("failed to derive visible owner from overlay path; falling back to process owner", "overlay", *overlayDir, "error", ownerErr)
		} else {
			visibleUID = uid
			visibleGID = gid
		}
	}

	logger.Info("starting monofs-client",
		"version", Version,
		"commit", Commit,
		"build_time", BuildTime,
		"router", *routerAddr,
		"mount", *mountpoint,
		"cache", *cacheDir,
		"overlay", *overlayDir,
		"use_external_addrs", *useExternalAddrs,
		"writable", *writable,
		"virtual_monorepo", *virtualMonorepo,
		"debug", *debug,
		"fuse_debug", *fuseDebug,
		"log_file", *logFile,
	)

	// Resolve persistent client identity
	resolvedClientID := *clientID
	if resolvedClientID == "" {
		var idErr error
		resolvedClientID, idErr = client.LoadOrCreateClientID()
		if idErr != nil {
			logger.Warn("failed to load persistent client ID, using ephemeral", "error", idErr)
		}
	}
	if resolvedClientID != "" {
		logger.Info("client identity", "client_id", resolvedClientID)
	}

	// Connect to router and create sharded client
	ctx := context.Background()
	clientCfg := newShardedClientConfig(
		*routerAddr,
		resolvedClientID,
		*mountpoint,
		*writable,
		*rpcTimeout,
		logger.With("component", "sharded-client"),
		*useExternalAddrs,
	)
	c, err := client.NewShardedClient(ctx, clientCfg)
	if err != nil {
		logger.Warn("failed to connect to router, will retry in background", "error", err)
		// Create client in disconnected state - it will retry connections
		c = client.NewDisconnectedClient(clientCfg)
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

		cacheLayer, err = cache.New(*cacheDir, logger.With("component", "cache"))
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

		sessionMgr, err = monofuse.NewSessionManager(*overlayDir, logger.With("component", "session"))
		if err != nil {
			logger.Error("failed to create session manager", "error", err)
			os.Exit(1)
		}

		commitMgr = monofuse.NewCommitManager(sessionMgr, c, logger.With("component", "commit"))
		if resolvedClientID != "" {
			commitMgr.SetPrincipalID(resolvedClientID)
		}

		socketHandler, err = monofuse.NewSessionSocketHandler(*overlayDir, sessionMgr, commitMgr, logger.With("component", "session-socket"))
		if err != nil {
			logger.Error("failed to create session socket", "error", err)
			os.Exit(1)
		}
		if resolvedClientID != "" {
			socketHandler.SetPrincipalID(resolvedClientID)
		}

		// Wire cluster ingester so push-deps pushes files to backend nodes.
		socketHandler.SetIngester(monofuse.BlobIngesterFunc(
			func(ctx context.Context, files []monofuse.BlobIngestFile) (*monofuse.BlobIngestResult, error) {
				clientFiles := make([]client.BlobFile, len(files))
				for i, f := range files {
					clientFiles[i] = client.BlobFile{
						Path:     f.Path,
						Content:  f.Content,
						Mode:     f.Mode,
						FileType: f.FileType,
					}
				}
				res, err := c.IngestBlobs(ctx, clientFiles)
				if err != nil {
					return nil, err
				}
				out := &monofuse.BlobIngestResult{
					FilesIngested: res.FilesIngested,
					FilesFailed:   res.FilesFailed,
				}
				for _, f := range res.FailedFiles {
					out.FailedFiles = append(out.FailedFiles, monofuse.BlobFailedFile{
						Path:   f.Path,
						Reason: f.Reason,
					})
				}
				return out, nil
			},
		))
		logger.Info("blob cluster ingestion enabled")

		// Wire cluster deleter so push-deps propagates file deletions to backend nodes.
		socketHandler.SetDeleter(monofuse.BlobDeleterFunc(
			func(ctx context.Context, paths []string) (*monofuse.BlobDeleteResult, error) {
				res, err := c.DeleteBlobs(ctx, paths)
				if err != nil {
					return nil, err
				}
				return &monofuse.BlobDeleteResult{
					FilesDeleted: res.FilesDeleted,
					FilesFailed:  res.FilesFailed,
				}, nil
			},
		))
		logger.Info("blob cluster deletion enabled")

		// Wire diff reader so `monofs-session diff` can compare overlay vs original.
		socketHandler.SetDiffReader(monofuse.DiffReaderFunc(
			func(ctx context.Context, path string) ([]byte, error) {
				return c.Read(ctx, path, 0, 0)
			},
		))

		// Wire workspace refresher so `monofs-session pull` re-ingests the
		// included virtual-monorepo repositories through the router.
		socketHandler.SetWorkspaceRefresher(monofuse.WorkspaceRefresherFunc(
			func(ctx context.Context, repos []client.WorkspaceRepository) (*client.WorkspaceRefreshResult, error) {
				return c.RefreshWorkspaceRepositories(ctx, repos)
			},
		))

		// Wire attr cache so push can invalidate stale dependency entries.
		if cacheLayer != nil {
			socketHandler.SetAttrCache(cacheLayer)
		}

		socketHandler.Start()
		defer socketHandler.Stop()

		logger.Info("write support enabled",
			"session_socket", filepath.Join(*overlayDir, "session.sock"))
	}

	// Create root node
	var root *monofuse.MonoNode
	if sessionMgr != nil {
		root = monofuse.NewRootWithSession(c, cacheLayer, sessionMgr, logger.With("component", "fuse"))
	} else {
		root = monofuse.NewRoot(c, cacheLayer, logger.With("component", "fuse"))
	}
	root.SetVisibleOwner(visibleUID, visibleGID)
	if *virtualMonorepo {
		if err := root.EnableVirtualMonorepo(); err != nil {
			logger.Error("failed to enable virtual monorepo mode", "error", err)
			os.Exit(1)
		}
		if err := root.EnableWorkspaceGitProjection(*mountpoint, workspaceGitStateDir); err != nil {
			logger.Error("failed to enable workspace git projection", "error", err)
			os.Exit(1)
		}
		logger.Info("virtual monorepo mode enabled")
		if commitMgr != nil {
			commitMgr.SetWorkspaceManifest(root.WorkspaceManifest())
		}
		c.SetTopologyChangeHook(func() {
			syncCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := root.SyncWorkspaceGitProjection(syncCtx); err != nil {
				logger.Warn("workspace git projection sync failed after topology change", "error", err)
			}
		})
	}
	if socketHandler != nil {
		socketHandler.SetRootNode(root)
	}

	// Mount options
	attrTimeout := cache.DefaultAttrTTL
	entryTimeout := cache.DefaultAttrTTL
	negativeTimeout := cache.DefaultAttrTTL
	// Writable filesystems need very short negative dentry caching.
	// Without this, a file that didn't exist 30s ago stays invisible
	// even after it's been created (e.g. go mod download race).
	if *writable {
		negativeTimeout = 0
	}
	opts := &fs.Options{
		MountOptions: fuse.MountOptions{
			// Debug here controls the go-fuse C layer verbosity.
			// Deliberately NOT wired to --debug; use --fuse-debug to enable.
			Debug:      *fuseDebug,
			FsName:     "monofs",
			Name:       "monofs",
			AllowOther: true, // Allow all users to access the filesystem
		},
		// Cache settings for better performance
		AttrTimeout:     &attrTimeout,
		EntryTimeout:    &entryTimeout,
		NegativeTimeout: &negativeTimeout,
	}

	// Mount filesystem
	server, err := fs.Mount(*mountpoint, root, opts)
	if err != nil {
		logger.Error("failed to mount", "error", err)
		os.Exit(1)
	}

	logger.Info("filesystem mounted", "mountpoint", *mountpoint, "writable", *writable)
	if *virtualMonorepo {
		syncCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		if err := root.SyncWorkspaceGitProjection(syncCtx); err != nil {
			logger.Warn("workspace git projection sync failed", "error", err)
		}
		cancel()
	}

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

func newShardedClientConfig(
	routerAddr, clientID, mountpoint string,
	writable bool,
	rpcTimeout time.Duration,
	logger *slog.Logger,
	useExternalAddresses bool,
) client.ShardedClientConfig {
	return client.ShardedClientConfig{
		RouterAddr:           routerAddr,
		ClientID:             clientID,
		RefreshInterval:      30 * time.Second,
		RPCTimeout:           rpcTimeout,
		UseExternalAddresses: useExternalAddresses,
		Logger:               logger,
		MountPoint:           mountpoint,
		Writable:             writable,
	}
}

func workspaceGitStateDir(overlayDir string) string {
	if strings.TrimSpace(overlayDir) != "" {
		return filepath.Join(overlayDir, "workspace-git")
	}
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".monofs", "workspace-git")
}

func validateClientPaths(mountpoint, overlayDir, cacheDir, workspaceGitStateDir string) error {
	mountAbs, err := absoluteCleanPath(mountpoint)
	if err != nil {
		return fmt.Errorf("resolve mount path: %w", err)
	}

	checks := []struct {
		label string
		path  string
	}{
		{label: "overlay", path: overlayDir},
		{label: "cache", path: cacheDir},
		{label: "workspace git state", path: workspaceGitStateDir},
	}

	for _, check := range checks {
		if strings.TrimSpace(check.path) == "" {
			continue
		}
		stateAbs, err := absoluteCleanPath(check.path)
		if err != nil {
			return fmt.Errorf("resolve %s path: %w", check.label, err)
		}
		if isSameOrNestedPath(stateAbs, mountAbs) {
			return fmt.Errorf("%s path %q must be outside mountpoint %q", check.label, stateAbs, mountAbs)
		}
	}

	return nil
}

func absoluteCleanPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is empty")
	}
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return filepath.Abs(cleaned)
	}
	return cleaned, nil
}

func isSameOrNestedPath(path, base string) bool {
	if path == base {
		return true
	}
	return strings.HasPrefix(path, base+string(os.PathSeparator))
}

// buildLogger constructs the MonoFS structured logger.
//
//   - stdout : text handler, INFO+  (always; operator / SSH friendly)
//   - logFile: JSON handler, DEBUG+ (append; machine-parseable for jq/grep)
//
// When --debug is set but --log-file is not, stdout is promoted to DEBUG so
// the operator still sees the verbose output.
func buildLogger(debug bool, logFile string) *slog.Logger {
	stdoutLevel := slog.LevelInfo
	if debug && logFile == "" {
		stdoutLevel = slog.LevelDebug
	}

	stdoutHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level:     stdoutLevel,
		AddSource: false,
	})

	if logFile == "" {
		return slog.New(stdoutHandler)
	}

	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot open log file %s: %v — logging to stdout only\n", logFile, err)
		return slog.New(stdoutHandler)
	}

	fileLevel := slog.LevelInfo
	if debug {
		fileLevel = slog.LevelDebug
	}

	fileHandler := slog.NewJSONHandler(f, &slog.HandlerOptions{
		Level:     fileLevel,
		AddSource: true, // file:line in JSON helps grep-based debugging
	})

	return slog.New(&multiHandler{handlers: []slog.Handler{stdoutHandler, fileHandler}})
}

// buildFuseLogWriter returns the writer used for the stdlib log package, which
// go-fuse uses internally for its C-layer debug output.
//
//   - fuseDebug=false            → io.Discard (silence all FUSE C-layer chatter)
//   - fuseDebug=true, logFile="" → stderr (visible, but separate from slog)
//   - fuseDebug=true, logFile!="" → logFile+".fuse" (clearly separate file)
func buildFuseLogWriter(logFile string, fuseDebug bool) io.Writer {
	if !fuseDebug {
		return io.Discard
	}
	if logFile != "" {
		fusePath := logFile + ".fuse"
		f, err := os.OpenFile(fusePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err == nil {
			fmt.Fprintf(os.Stderr, "go-fuse C layer logs → %s\n", fusePath)
			return f
		}
		fmt.Fprintf(os.Stderr, "warning: cannot open fuse log file %s: %v — using stderr\n", fusePath, err)
	}
	return os.Stderr
}

// multiHandler fans slog records out to multiple handlers.
// Each handler applies its own level filter set in buildLogger.
type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: next}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: next}
}

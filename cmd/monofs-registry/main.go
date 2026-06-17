package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/radryc/monofs/internal/registry"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	var (
		listen         = flag.String("addr", ":5000", "HTTP listen address")
		routerAddr     = flag.String("router-addr", os.Getenv("MONOFS_ROUTER_ADDR"), "MonoFS router gRPC address")
		token          = flag.String("token", os.Getenv("MONOFS_TOKEN"), "MonoFS guardian token")
		dataNS         = flag.String("data-ns", "docker-registry", "MonoFS data namespace prefix")
		logLevel       = flag.String("log-level", "info", "Log level: debug, info, warn, error")
		debug          = flag.Bool("debug", false, "Enable debug logging")

		defaultUpstream   = flag.String("upstream-default", os.Getenv("MONOFS_REGISTRY_DEFAULT_UPSTREAM"), "Default upstream registry URL for pull-through cache")
		upstreamMappings  = flag.String("upstreams", os.Getenv("MONOFS_REGISTRY_UPSTREAMS"), "Per-repo upstream mappings: prefix=url,prefix=url,...")
		upstreamUsername   = flag.String("upstream-username", os.Getenv("MONOFS_REGISTRY_UPSTREAM_USERNAME"), "Username for upstream registry auth")
		upstreamPassword   = flag.String("upstream-password", os.Getenv("MONOFS_REGISTRY_UPSTREAM_PASSWORD"), "Password for upstream registry auth")
		upstreamCooldown   = flag.Duration("upstream-cooldown", 10*time.Minute, "Time before re-checking upstream for updates")
	)
	flag.Parse()

	level := slog.LevelInfo
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	logger.Info("starting monofs-registry",
		"version", Version,
		"commit", Commit,
		"build_time", BuildTime,
		"addr", *listen,
		"router_addr", *routerAddr,
		"data_ns", *dataNS,
	)

	if *routerAddr == "" {
		logger.Error("monofs router address is required (set --router-addr or MONOFS_ROUTER_ADDR)")
		os.Exit(1)
	}

	ctx := context.Background()
	client, err := registry.NewClient(ctx, registry.ClientConfig{
		RouterAddr: *routerAddr,
		Token:      *token,
		DataNS:     *dataNS,
		Logger:     logger,
	})
	if err != nil {
		logger.Error("failed to connect to monofs router", "error", err)
		os.Exit(1)
	}
	defer client.Close()

	ensureDataDirs(ctx, client, logger)

	upstreamConfig := registry.UpstreamConfig{
		Default:  *defaultUpstream,
		Mappings: parseUpstreamMappings(*upstreamMappings),
		Username: *upstreamUsername,
		Password: *upstreamPassword,
		Cooldown: *upstreamCooldown,
	}

	proxy := registry.NewProxy(upstreamConfig, registry.NewBlobStore(client), registry.NewTagStore(client, registry.NewBlobStore(client)), &registry.Stats{}, logger)
	server := registry.NewServer(client, proxy, logger, *dataNS)

	httpServer := &http.Server{
		Addr:    *listen,
		Handler: server.Handler(),
	}

	go func() {
		logger.Info("monofs-registry listening", "addr", *listen)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server error", "error", err)
			os.Exit(1)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logger.Info("shutting down monofs-registry...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "error", err)
	}
}

func parseUpstreamMappings(raw string) map[string]string {
	mappings := make(map[string]string)
	if raw == "" {
		return mappings
	}
	for _, pair := range strings.Split(raw, ",") {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			mappings[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return mappings
}

func ensureDataDirs(ctx context.Context, client *registry.Client, logger *slog.Logger) {
	dirs := []string{"_blobs", "_uploads"}
	for _, dir := range dirs {
		if err := client.CreateDir(ctx, dir); err != nil {
			logger.Warn("failed to ensure data dir", "dir", dir, "error", err)
		}
	}
}

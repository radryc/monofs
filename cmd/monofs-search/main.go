// Package main implements the MonoFS search service.
package main

import (
	"context"
	"crypto/subtle"
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
	"github.com/radryc/monofs/internal/diagnostics"
	"github.com/radryc/monofs/internal/search"
	"github.com/radryc/monofs/internal/telemetry"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
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
	indexGuardian := flag.Bool("index-guardian", strings.EqualFold(os.Getenv("MONOFS_SEARCH_INDEX_GUARDIAN"), "true"), "Index Guardian-managed partitions (default false: no clonable upstream)")
	diagnosticsAddr := flag.String("diagnostics-addr", ":9101", "Listen address for Prometheus /metrics and pprof endpoints (empty disables)")
	debug := flag.Bool("debug", false, "Enable debug logging")
	showVersion := flag.Bool("version", false, "Show version and exit")

	// Access-control flags. Filtering is off/observe by default so rollout
	// does not change behavior until --authz-enforce-read is set.
	token := flag.String("token", os.Getenv("MONOFS_TOKEN"), "Internal cluster token: requests presenting it via the x-monofs-internal metadata bypass read filtering (defaults to MONOFS_TOKEN)")
	oidcIssuer := flag.String("oidc-issuer", "", "OIDC issuer for bearer token verification (enables OIDC when set with --oidc-audience)")
	oidcAudience := flag.String("oidc-audience", "", "OIDC audience for bearer token verification")
	oidcJWKSURL := flag.String("oidc-jwks-url", "", "Explicit JWKS URL (defaults to OIDC discovery on the issuer)")
	breakGlassToken := flag.String("break-glass-token", "", "Break-glass admin bearer token (resolves to client id break-glass-admin)")
	accessPolicyURL := flag.String("access-policy-url", "", "URL of the compiled access policy bundle (e.g. the router /api/access/policy endpoint)")
	accessPolicyFile := flag.String("access-policy-file", "", "Path to a compiled access policy bundle (dev)")
	accessPolicyToken := flag.String("access-policy-token", "", "Bearer token for --access-policy-url")
	accessPolicyInterval := flag.Duration("access-policy-interval", policy.DefaultPollInterval, "Access policy refresh interval")
	authzEnforceRead := flag.Bool("authz-enforce-read", false, "Enforce access-policy filtering on search results (default false: log decisions only)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("monofs-search %s (commit: %s, built: %s)\n", Version, Commit, BuildTime)
		os.Exit(0)
	}
	telemetryCfg, err := telemetry.LoadConfig("monofs-search")
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

	// Setup logging
	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}
	var handler slog.Handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	})
	if telemetryHandle.Enabled() {
		handler = telemetry.WrapSlogHandler(handler, "monofs/search")
	}
	logger := slog.New(handler)
	if telemetryHandle.Enabled() {
		telemetry.EmitInfo(context.Background(), "monofs/search", "monofs search telemetry enabled")
	}

	logger.Info("starting monofs-search",
		"version", Version,
		"commit", Commit,
		"port", *port,
		"diagnostics_addr", *diagnosticsAddr,
		"index_dir", *indexDir,
		"cache_dir", *cacheDir,
		"workers", *workers)

	diagServer := diagnostics.StartServer(logger, "monofs-search", strings.TrimSpace(*diagnosticsAddr))
	defer diagnostics.ShutdownServer(logger, "monofs-search", diagServer)

	// Access-control wiring: verify bearer tokens, load the compiled policy and
	// hand both to the search service. Filtering stays off/observe unless
	// --authz-enforce-read is set.
	var verifiers []authz.TokenVerifier
	if strings.TrimSpace(*oidcIssuer) != "" && strings.TrimSpace(*oidcAudience) != "" {
		oidcVerifier, oidcErr := authz.NewOIDCVerifier(authz.OIDCConfig{
			Issuer:   strings.TrimSpace(*oidcIssuer),
			Audience: strings.TrimSpace(*oidcAudience),
			JWKSURL:  strings.TrimSpace(*oidcJWKSURL),
		})
		if oidcErr != nil {
			logger.Error("failed to configure OIDC verifier", "error", oidcErr)
			os.Exit(1)
		}
		verifiers = append(verifiers, oidcVerifier)
	}
	if bg := strings.TrimSpace(*breakGlassToken); bg != "" {
		verifiers = append(verifiers, authz.NewBreakGlassVerifier(bg, "break-glass-admin"))
	}
	var verifier authz.TokenVerifier = authz.NoopVerifier{}
	switch len(verifiers) {
	case 1:
		verifier = verifiers[0]
	case 2:
		verifier = authz.NewChainVerifier(verifiers...)
	}
	authenticator := authz.NewAuthenticator(verifier, logger, false)

	var policyLoader *policy.Loader
	if u := strings.TrimSpace(*accessPolicyURL); u != "" {
		policyLoader = policy.NewLoader(policy.HTTPSource{URL: u, Token: strings.TrimSpace(*accessPolicyToken)}, *accessPolicyInterval, func(format string, args ...any) {
			logger.Info(fmt.Sprintf(format, args...))
		})
	} else if f := strings.TrimSpace(*accessPolicyFile); f != "" {
		policyLoader = policy.NewLoader(policy.FileSource{Path: f}, *accessPolicyInterval, func(format string, args ...any) {
			logger.Info(fmt.Sprintf(format, args...))
		})
	}
	if policyLoader != nil {
		go policyLoader.Run(context.Background())
		logger.Info("access policy enabled", "source", policyLoader.Source(), "enforce_read", *authzEnforceRead)
	}

	// Create search service
	cfg := search.Config{
		IndexDir:      *indexDir,
		CacheDir:      *cacheDir,
		Workers:       *workers,
		QueueSize:     *queueSize,
		RouterAddr:    *routerAddr,
		IndexGuardian: *indexGuardian,
		Logger:        logger,
		AccessPolicy:  policyLoader,
		EnforceRead:   *authzEnforceRead,
	}

	svc, err := search.NewService(cfg)
	if err != nil {
		logger.Error("failed to create search service", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	// Create gRPC server
	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.MaxRecvMsgSize(100*1024*1024), // 100MB max message size
		grpc.MaxSendMsgSize(100*1024*1024),
		grpc.ChainUnaryInterceptor(internalAwareInterceptor(authenticator, strings.TrimSpace(*token))),
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

// internalAwareInterceptor wraps the shared auth interceptor: calls that
// present the cluster-internal token are marked as internal (they bypass
// access-policy filtering), every other call goes through normal bearer-token
// verification and identity attachment.
func internalAwareInterceptor(authenticator *authz.Authenticator, internalToken string) grpc.UnaryServerInterceptor {
	base := authenticator.UnaryServerInterceptor()
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if internalToken != "" {
			if md, ok := metadata.FromIncomingContext(ctx); ok {
				if vals := md.Get("x-monofs-internal"); len(vals) > 0 &&
					subtle.ConstantTimeCompare([]byte(vals[0]), []byte(internalToken)) == 1 {
					ctx = search.ContextWithInternalCall(ctx)
					ctx = authz.ContextWithIdentity(ctx, authz.Identity{ClientID: search.InternalClientID})
					return handler(ctx, req)
				}
			}
		}
		return base(ctx, req, info, handler)
	}
}

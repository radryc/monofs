// MonoFS Router - Cluster topology coordinator
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/router"
	"github.com/radryc/monofs/internal/storage"
	filestorage "github.com/radryc/monofs/internal/storage/file"
	gitstorage "github.com/radryc/monofs/internal/storage/git"
	"github.com/radryc/monofs/internal/telemetry"
	"github.com/radryc/monofs/pkg/authz"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

var (
	// Version information (injected at build time)
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func init() {
	// Register Git backends
	storage.DefaultRegistry.RegisterIngestionBackend(
		storage.IngestionTypeGit,
		gitstorage.NewGitIngestionBackend,
	)
	storage.DefaultRegistry.RegisterFetchBackend(
		storage.FetchTypeGit,
		gitstorage.NewGitFetchBackend,
	)

	// Register file backend
	storage.DefaultRegistry.RegisterIngestionBackend(
		storage.IngestionTypeFile,
		filestorage.NewFileIngestionBackend,
	)

	// Future backends will be registered here:
	// storage.DefaultRegistry.RegisterFetchBackend(storage.FetchTypeS3, s3storage.NewS3FetchBackend)
}

func main() {
	var (
		port              = flag.Int("port", 9090, "Router service port")
		httpPort          = flag.Int("http-port", 8080, "HTTP UI port")
		nativeAddr        = flag.String("native-addr", "", "Native protocol listen address (disabled when empty)")
		clusterID         = flag.String("cluster-id", "monofs-cluster", "Cluster identifier")
		routerName        = flag.String("router-name", "local", "Router instance name for UI identification")
		nodes             = flag.String("nodes", "", "Initial nodes: node1=host1:port1,node2=host2:port2,...")
		weights           = flag.String("weights", "", "Node weights: node1=100,node2=100,...")
		externalAddrs     = flag.String("external-addrs", "", "External addresses for host clients: node1=localhost:9001,node2=localhost:9002,...")
		peerRouters       = flag.String("peer-routers", "", "Peer routers for UI aggregation: name=http://host:port or host:port,...")
		searchAddr        = flag.String("search-addr", "", "Search service address (e.g., search:9100)")
		searchDiagAddr    = flag.String("search-diagnostics-addr", "", "Search diagnostics address for pprof collection (e.g., search:9101)")
		fetcherAddrs      = flag.String("fetcher-addrs", "", "Fetcher service addresses for cluster monitoring (e.g., fetcher1:9200,fetcher2:9200)")
		fetcherDiagAddrs  = flag.String("fetcher-diagnostics-addrs", "", "Fetcher diagnostics addresses for pprof collection (e.g., fetcher1:9201,fetcher2:9201)")
		registryAddr      = flag.String("registry-addr", "", "Monofs-registry address for UI proxy (e.g., monofs-registry:5000)")
		registryDiagAddr  = flag.String("registry-diagnostics-addr", "", "Registry diagnostics address for pprof collection (e.g., registry:5001)")
		serverDiagAddrs   = flag.String("server-diagnostics-addrs", "", "Server diagnostics addresses for pprof collection (e.g., node-a=node-a:9100,node-b=node-b:9100)")
		healthInt         = flag.Duration("health-interval", 2*time.Second, "Health check interval")
		unhealthyThr      = flag.Duration("unhealthy-threshold", 6*time.Second, "Time before marking node unhealthy")
		debug             = flag.Bool("debug", false, "Enable debug logging (shorthand for --log-level=debug)")
		logLevel          = flag.String("log-level", "info", "Log level: debug, info, warn, error")
		guardianStateDir  = flag.String("state-dir", ".monofs-router-state", "Directory for persistent router Guardian state")
		workspaceStateDir = flag.String("workspace-state-dir", "", "Directory for persistent workspace job state (Phase 1), separate from Guardian state")
		sourcePushMode    = flag.String("source-push-mode", "squash", "Source push mode: squash or preserve")
		policyGateEnabled = flag.Bool("policy-gate", false, "Enable policy-gated push/publish/refresh (Phase 3)")
		policyConfigPath  = flag.String("policy-config", "", "Path to policy YAML config file (Phase 3)")
		autoPushEnabled   = flag.Bool("auto-push", false, "Enable automatic push of pending commits (Phase 3)")
		autoPushInterval  = flag.Duration("auto-push-interval", 60*time.Second, "Interval between auto-push scans (Phase 3)")
		// Replication and failover configuration
		replicationFactor     = flag.Int("replication-factor", 2, "Number of data copies (1=no replication, 2=primary+1 backup, etc.)")
		rebalanceDelay        = flag.Duration("rebalance-delay", 10*time.Minute, "Wait time before permanent rebalancing after node failure")
		gracefulFailoverDelay = flag.Duration("graceful-failover-delay", 60*time.Second, "Wait time for graceful failover (planned restarts)")
		guardianIngestTimeout = flag.Duration("guardian-ingest-timeout", 5*time.Minute, "Timeout for guardian batch file ingestion to nodes")

		// Packager encryption
		encryptionKeyHex = flag.String("encryption-key", "", "32-byte hex-encoded encryption key for packager archives")

		// Partition authorization + SSO (authz epics A/C)
		authzEnforceIngest = flag.Bool("authz-enforce-ingest", false, "Enforce partition-scoped ingest authorization")
		authzGrantsPath    = flag.String("authz-grants-path", "", "Path to authz grant store JSON (defaults to <state-dir>/authz_grants.json)")
		oidcIssuer         = flag.String("oidc-issuer", "", "OIDC issuer URL for SSO token verification")
		oidcAudience       = flag.String("oidc-audience", "", "Expected OIDC audience for SSO tokens")
		oidcJWKSURL        = flag.String("oidc-jwks-url", "", "OIDC JWKS URL (discovered from issuer when empty)")
		devDisableAuth     = flag.Bool("insecure-dev-disable-auth", strings.EqualFold(strings.TrimSpace(os.Getenv("MONOFS_INSECURE_DEV_DISABLE_AUTH")), "true"), "Disable all UI/API authentication (development only; insecure)")
	)
	flag.Parse()
	telemetryCfg, err := telemetry.LoadConfig("monofs-router")
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

	// Setup logger
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
	var handler slog.Handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	if telemetryHandle.Enabled() {
		handler = telemetry.WrapSlogHandler(handler, "monofs/router")
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	if telemetryHandle.Enabled() {
		telemetry.EmitInfo(context.Background(), "monofs/router", "monofs router telemetry enabled")
	}

	logger.Info("starting monofs-router",
		"version", Version,
		"commit", Commit,
		"build_time", BuildTime,
		"router_name", *routerName,
		"port", *port,
		"http_port", *httpPort,
		"cluster_id", *clusterID,
		"replication_factor", *replicationFactor,
		"rebalance_delay", *rebalanceDelay,
		"graceful_failover_delay", *gracefulFailoverDelay)

	// Parse encryption key (flag > env var)
	var encryptionKey []byte
	encKeyStr := *encryptionKeyHex
	if encKeyStr == "" {
		encKeyStr = os.Getenv("MONOFS_ENCRYPTION_KEY")
	}
	if encKeyStr != "" {
		var err error
		encryptionKey, err = hex.DecodeString(encKeyStr)
		if err != nil || len(encryptionKey) != 32 {
			logger.Error("encryption key must be 32 bytes (64 hex chars)", "len", len(encryptionKey), "error", err)
			os.Exit(1)
		}
	}

	// Create router
	cfg := router.RouterConfig{
		ClusterID:             *clusterID,
		RouterName:            *routerName,
		HealthCheckInterval:   *healthInt,
		UnhealthyThreshold:    *unhealthyThr,
		PeerRouters:           parsePeerRouters(*peerRouters),
		SearchDiagnostics:     strings.TrimSpace(*searchDiagAddr),
		FetcherDiagnostics:    parseCSVAddrs(*fetcherDiagAddrs),
		ServerDiagnostics:     parseServerDiagnostics(*serverDiagAddrs),
		RegistryDiagnostics:   strings.TrimSpace(*registryDiagAddr),
		GuardianStateDir:      *guardianStateDir,
		WorkspaceStateDir:     *workspaceStateDir,
		SourcePushMode:        *sourcePushMode,
		PolicyGateEnabled:     *policyGateEnabled,
		PolicyConfigPath:      *policyConfigPath,
		AutoPushEnabled:       *autoPushEnabled,
		AutoPushInterval:      *autoPushInterval,
		EncryptionKey:         encryptionKey,
		ReplicationFactor:     *replicationFactor,
		RebalanceDelay:        *rebalanceDelay,
		GracefulFailoverDelay: *gracefulFailoverDelay,
		GuardianIngestTimeout: *guardianIngestTimeout,
		AuthzEnforceIngest:    *authzEnforceIngest,
		AuthzGrantsPath:       strings.TrimSpace(*authzGrantsPath),
		AuthzGrantsJSON:       strings.TrimSpace(os.Getenv("MONOFS_AUTHZ_GRANTS_JSON")),
	}
	r := router.NewRouter(cfg, logger)
	r.SetVersion(Version, Commit, BuildTime)

	// Configure search service if provided
	if *searchAddr != "" {
		if err := r.SetSearchClient(*searchAddr); err != nil {
			logger.Error("failed to configure search service", "error", err)
		}
	}

	// Configure fetcher cluster for monitoring if provided
	if *fetcherAddrs != "" {
		addrs := strings.Split(*fetcherAddrs, ",")
		for i, addr := range addrs {
			addrs[i] = strings.TrimSpace(addr)
		}
		if err := r.SetFetcherClient(addrs); err != nil {
			logger.Warn("failed to configure fetcher cluster", "error", err)
		}
	}

	if *registryAddr != "" {
		r.SetRegistryAddr(strings.TrimSpace(*registryAddr))
	}

	// Parse initial nodes
	if *nodes != "" {
		weightMap := parseWeights(*weights)
		externalAddrMap := parseExternalAddrs(*externalAddrs)

		for _, nodeSpec := range strings.Split(*nodes, ",") {
			parts := strings.SplitN(nodeSpec, "=", 2)
			if len(parts) != 2 {
				log.Fatalf("Invalid node spec: %s (expected node_id=host:port)", nodeSpec)
			}
			nodeID := strings.TrimSpace(parts[0])
			address := strings.TrimSpace(parts[1])
			weight := uint32(100)
			if w, ok := weightMap[nodeID]; ok {
				weight = w
			}

			// Get external address if configured
			externalAddr := externalAddrMap[nodeID]
			if externalAddr != "" {
				r.RegisterNodeWithExternalAddr(nodeID, address, externalAddr, weight)
				logger.Info("registered node with dual addressing",
					"node_id", nodeID,
					"internal_address", address,
					"external_address", externalAddr,
					"weight", weight)
			} else {
				r.RegisterNodeStatic(nodeID, address, weight)
				logger.Info("registered node", "node_id", nodeID, "address", address, "weight", weight)
			}
		}
	}

	// Start health checking
	r.StartHealthCheck()

	// Build the SSO verifier. It accepts OIDC tokens (when configured) and,
	// as a break-glass path, a static admin token supplied via MONOFS_TOKEN.
	// Authentication is enforced by default for HTTP (UI + CLI API); use
	// --insecure-dev-disable-auth only in local development to allow anonymous
	// access. gRPC service-to-service traffic remains in observe mode because
	// those connections already use their own transport credentials.
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
		logger.Info("OIDC SSO verification enabled", "issuer", *oidcIssuer, "audience", *oidcAudience)
	}
	const breakGlassClientID = "break-glass-admin"
	if bg := strings.TrimSpace(os.Getenv("MONOFS_TOKEN")); bg != "" {
		verifiers = append(verifiers, authz.NewBreakGlassVerifier(bg, breakGlassClientID))
		r.AddBreakGlassAdmin(breakGlassClientID)
		logger.Warn("break-glass admin token enabled via MONOFS_TOKEN; all usage is audited")
	}

	requireToken := !*devDisableAuth
	if requireToken && len(verifiers) == 0 {
		logger.Error("no authentication configured: set --oidc-issuer and --oidc-audience, set MONOFS_TOKEN for break-glass access, or use --insecure-dev-disable-auth for local development")
		os.Exit(1)
	}

	var verifier authz.TokenVerifier = authz.NoopVerifier{}
	switch len(verifiers) {
	case 0:
		// Development mode: anonymous identities allowed.
	case 1:
		verifier = verifiers[0]
	default:
		verifier = authz.NewChainVerifier(verifiers...)
	}
	// HTTP routes (UI + CLI API) enforce authentication when not in dev mode.
	// gRPC routes are used for internal service-to-service traffic which already
	// has its own transport security, so they remain in observe mode.
	httpAuthenticator := authz.NewAuthenticator(verifier, logger, requireToken)
	grpcAuthenticator := authz.NewAuthenticator(verifier, logger, false)

	// Emit Prometheus metrics for authentication outcomes on both protocols.
	// outcome is one of: authenticated, anonymous, rejected.
	observe := func(protocol string) func(string) {
		return func(outcome string) {
			router.RecordAuthOutcome(outcome, protocol)
		}
	}
	httpAuthenticator.Observe = observe("http")
	grpcAuthenticator.Observe = observe("grpc")

	// Create gRPC server with keepalive enforcement policy
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(1024*1024*1024),
		grpc.MaxSendMsgSize(1024*1024*1024),
		grpc.StatsHandler(telemetry.NewGRPCServerStatsHandler()),
		grpc.ChainUnaryInterceptor(grpcAuthenticator.UnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(grpcAuthenticator.StreamServerInterceptor()),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second, // Allow pings every 5s (prevents too_many_pings)
			PermitWithoutStream: true,            // Allow pings even when no streams active
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    2 * time.Minute,  // Send keepalive pings if no activity
			Timeout: 20 * time.Second, // Wait 20s for ping ack before closing
		}),
	)
	pb.RegisterMonoFSRouterServer(grpcServer, r)

	// Start gRPC server in background
	go func() {
		logger.Info("monofs router grpc listening", "port", *port, "cluster_id", *clusterID)
		if err := grpcServer.Serve(lis); err != nil {
			logger.Error("failed to serve grpc", "error", err)
			os.Exit(1)
		}
	}()

	// Start HTTP UI server
	base := r.ServeHTTP()

	// Public paths must remain reachable without authentication so probes,
	// scrapers, and static login-page assets keep working.
	publicPaths := []string{
		"/health", "/healthz", "/livez", "/readyz", "/-/health",
		"/metrics",
		"/favicon.ico", "/favicon.svg", "/icons.svg",
		"/assets/", "/static/",
	}
	// The WebAuthenticator only exempts /healthz variants by default, not /health.
	webAuthExemptPaths := []string{"/health"}
	publicExempt := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, p := range publicPaths {
				if r.URL.Path == p || strings.HasPrefix(r.URL.Path, p) {
					base.ServeHTTP(w, r)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}

	// API endpoints must not be anonymous when auth is enforced. The
	// WebAuthenticator attaches the identity (session cookie or bearer token) to
	// the request context before this check runs.
	apiIdentityCheck := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !*devDisableAuth && strings.HasPrefix(r.URL.Path, "/api/") {
				if id, ok := authz.IdentityFromContext(r.Context()); !ok || id.IsAnonymous() {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}

	// Base handler chain: public paths bypass auth, everything else needs a token.
	authChecked := publicExempt(httpAuthenticator.HTTPMiddleware(base))
	var uiHandler http.Handler = authChecked

	// Browser login (Authorization Code + PKCE) for the router UI. When OIDC is
	// configured, anonymous browsers are redirected to the IdP by default.
	routerRedirect := strings.TrimSpace(os.Getenv("MONOFS_ROUTER_OIDC_REDIRECT_URL"))
	routerSecret := strings.TrimSpace(os.Getenv("MONOFS_OIDC_CLIENT_SECRET"))
	routerAuthURL := strings.TrimSpace(os.Getenv("MONOFS_ROUTER_OIDC_AUTH_URL"))
	if routerAuthURL == "" {
		routerAuthURL = strings.TrimSpace(os.Getenv("MONOFS_OIDC_AUTH_URL"))
	}
	if strings.TrimSpace(*oidcIssuer) != "" && strings.TrimSpace(*oidcAudience) != "" && routerSecret != "" && routerRedirect != "" {
		clientID := strings.TrimSpace(os.Getenv("MONOFS_OIDC_CLIENT_ID"))
		if clientID == "" {
			clientID = "monofs"
		}
		waCfg := authz.WebAuthConfig{
			Issuer:       strings.TrimSpace(*oidcIssuer),
			ClientID:     clientID,
			ClientSecret: routerSecret,
			RedirectURL:  routerRedirect,
			Verifier:     verifier,
			RequireLogin: !*devDisableAuth,
			ExemptPaths:  webAuthExemptPaths,
		}
		if routerAuthURL != "" {
			waCfg.Endpoints.AuthURL = routerAuthURL
		}
		// Default session persistence under the state dir so browser logins
		// survive router restarts. Explicit MONOFS_SESSION_DIR overrides this.
		if os.Getenv("MONOFS_SESSION_DIR") == "" && *guardianStateDir != "" {
			if ps, err := authz.NewPersistentSessionStore(filepath.Join(*guardianStateDir, "sessions"), 12*time.Hour); err == nil {
				waCfg.Sessions = ps
			}
		}
		if wa, werr := authz.NewWebAuthenticator(context.Background(), waCfg); werr != nil {
			logger.Error("router ui browser login disabled", "error", werr)
		} else {
			// Browser auth handles the UI and attaches identity from session
			// cookie or bearer token. API calls must carry a non-anonymous
			// identity. Public paths bypass auth entirely.
			uiHandler = wa.Handler(publicExempt(apiIdentityCheck(base)))
			if routerAuthURL != "" {
				logger.Info("router ui browser login enabled", "redirect", routerRedirect, "auth_url", routerAuthURL)
			} else {
				logger.Info("router ui browser login enabled", "redirect", routerRedirect)
			}
		}
	}
	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", *httpPort),
		Handler: uiHandler,
	}
	if telemetryHandle.Enabled() {
		httpServer.Handler = otelhttp.NewHandler(httpServer.Handler, "monofs-router-http")
	}

	var nativeListener net.Listener
	var nativeServer *router.NativeGateway
	if *nativeAddr != "" {
		nativeListener, err = net.Listen("tcp", *nativeAddr)
		if err != nil {
			log.Fatalf("Failed to listen for native protocol: %v", err)
		}
		nativeServer = router.NewNativeGateway(r, logger)
		go func() {
			logger.Info("monofs router native listener", "addr", *nativeAddr)
			if err := nativeServer.Serve(nativeListener); err != nil {
				logger.Error("failed to serve native protocol", "error", err)
				os.Exit(1)
			}
		}()
	}

	go func() {
		logger.Info("monofs router http ui listening", "port", *httpPort)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("failed to serve http", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logger.Info("shutting down router...")
	httpServer.Close()
	if nativeListener != nil {
		nativeListener.Close()
	}
	grpcServer.GracefulStop()
	r.Close()
}

func parseWeights(weightsStr string) map[string]uint32 {
	result := make(map[string]uint32)
	if weightsStr == "" {
		return result
	}

	for _, spec := range strings.Split(weightsStr, ",") {
		parts := strings.SplitN(spec, "=", 2)
		if len(parts) != 2 {
			continue
		}
		nodeID := strings.TrimSpace(parts[0])
		var weight uint32
		if _, err := fmt.Sscanf(parts[1], "%d", &weight); err != nil {
			continue
		}
		if weight > 0 {
			result[nodeID] = weight
		}
	}
	return result
}

func parseExternalAddrs(addrsStr string) map[string]string {
	result := make(map[string]string)
	if addrsStr == "" {
		return result
	}

	for _, spec := range strings.Split(addrsStr, ",") {
		parts := strings.SplitN(spec, "=", 2)
		if len(parts) != 2 {
			continue
		}
		nodeID := strings.TrimSpace(parts[0])
		address := strings.TrimSpace(parts[1])
		if address != "" {
			result[nodeID] = address
		}
	}
	return result
}

func parsePeerRouters(peersStr string) []router.RouterPeer {
	if peersStr == "" {
		return nil
	}

	items := strings.Split(peersStr, ",")
	peers := make([]router.RouterPeer, 0, len(items))
	for _, raw := range items {
		spec := strings.TrimSpace(raw)
		if spec == "" {
			continue
		}
		if strings.Contains(spec, "=") {
			parts := strings.SplitN(spec, "=", 2)
			name := strings.TrimSpace(parts[0])
			url := strings.TrimSpace(parts[1])
			if name == "" {
				name = url
			}
			peers = append(peers, router.RouterPeer{Name: name, URL: url})
			continue
		}
		peers = append(peers, router.RouterPeer{Name: spec, URL: spec})
	}
	return peers
}

func parseCSVAddrs(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		addr := strings.TrimSpace(part)
		if addr != "" {
			result = append(result, addr)
		}
	}
	return result
}

func parseServerDiagnostics(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	result := make(map[string]string)
	for _, spec := range strings.Split(raw, ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		parts := strings.SplitN(spec, "=", 2)
		nodeID := strings.TrimSpace(parts[0])
		addr := ""
		if len(parts) == 2 {
			addr = strings.TrimSpace(parts[1])
		}
		if nodeID != "" && addr != "" {
			result[nodeID] = addr
		}
	}
	return result
}

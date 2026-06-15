// Package router provides HTTP UI handlers for MonoFS.
package router

import (
	"archive/zip"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/sharding"
	"github.com/radryc/monofs/internal/storage"
	"google.golang.org/grpc/metadata"
)

//go:embed ui/dist
var spaFS embed.FS

//go:embed static/*
var staticFiles embed.FS

// mockIngestStream implements MonoFSRouter_IngestRepositoryServer for HTTP ingestion.
// Progress messages are discarded — callers use fire-and-forget goroutines.
type mockIngestStream struct {
	ctx context.Context
}

func (s *mockIngestStream) Send(_ *pb.IngestProgress) error {
	return nil
}

func (s *mockIngestStream) Context() context.Context {
	return s.ctx
}

func (s *mockIngestStream) SendMsg(m interface{}) error {
	return nil
}

func (s *mockIngestStream) RecvMsg(m interface{}) error {
	return nil
}

func (s *mockIngestStream) SetHeader(metadata.MD) error {
	return nil
}

func (s *mockIngestStream) SendHeader(metadata.MD) error {
	return nil
}

func (s *mockIngestStream) SetTrailer(metadata.MD) {
}

func (r *Router) injectGuardianPartitionFromSource(ctx context.Context, source, ref, partitionName, token string) error {
	if source == "" {
		return fmt.Errorf("source is required")
	}
	if partitionName == "" {
		return fmt.Errorf("partition_name is required")
	}
	if ref == "" {
		ref = "main"
	}

	backend, err := storage.DefaultRegistry.CreateIngestionBackend(storage.IngestionTypeGit)
	if err != nil {
		return fmt.Errorf("create guardian source backend: %w", err)
	}
	defer backend.Cleanup()

	config := map[string]string{
		"branch":       ref,
		"display_path": "guardian/" + partitionName,
	}
	if err := backend.Validate(ctx, source, config); err != nil {
		return fmt.Errorf("validate guardian source: %w", err)
	}
	if err := backend.Initialize(ctx, source, config); err != nil {
		return fmt.Errorf("initialize guardian source: %w", err)
	}

	files := make([]*pb.InjectGuardianFile, 0, 64)
	if err := backend.WalkFiles(ctx, func(meta storage.FileMetadata) error {
		relPath := cleanGuardianRelativePath(meta.Path)
		if relPath == "" {
			return fmt.Errorf("guardian source file path %q is invalid", meta.Path)
		}
		files = append(files, &pb.InjectGuardianFile{
			Path:    relPath,
			Content: append([]byte(nil), meta.Content...),
		})
		return nil
	}); err != nil {
		return fmt.Errorf("walk guardian source: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("guardian source %q produced no files", source)
	}

	if _, err := r.InjectGuardianPartition(ctx, &pb.InjectGuardianPartitionRequest{
		GuardianToken: token,
		PartitionName: partitionName,
		Files:         files,
	}); err != nil {
		return fmt.Errorf("inject guardian partition: %w", err)
	}

	return nil
}

// ServeHTTP returns an HTTP handler for the web UI.
func (r *Router) ServeHTTP() http.Handler {
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("/api/ingest", r.handleIngest)
	mux.HandleFunc("/api/workspace-sync/jobs", r.handleWorkspaceSyncJobsAPI)
	mux.HandleFunc("/api/workspace-sync/jobs/", r.handleWorkspaceSyncJobsAPI)
	mux.HandleFunc("/api/status", r.handleStatus)
	mux.HandleFunc("/api/repositories", r.handleRepositoriesList)
	mux.HandleFunc("/api/routers", r.handleRouters)
	mux.HandleFunc("/api/rebalance", r.handleRebalance)
	mux.HandleFunc("/api/clients", r.handleClientsAPI)
	mux.HandleFunc("/api/local-clients", r.handleLocalClientsAPI)
	mux.HandleFunc("/api/fetchers", r.handleFetchersAPI)
	mux.HandleFunc("/api/logengine", r.handleLogEngineAPI)
	mux.HandleFunc("/api/dependencies", r.handleDependenciesAPI)

	// Whitelist API routes
	mux.HandleFunc("/api/whitelist", r.handleWhitelistAPI)
	mux.HandleFunc("/api/whitelist/toggle", r.handleWhitelistToggleAPI)

	// Predictor API route
	mux.HandleFunc("/api/predictor", r.handlePredictorAPI)
	mux.HandleFunc("/api/pprof/collect", r.handlePprofCollectAPI)

	// Search API routes
	mux.HandleFunc("/api/search", r.handleSearchAPI)
	mux.HandleFunc("/api/search/indexes", r.handleSearchIndexes)
	mux.HandleFunc("/api/search/rebuild", r.handleSearchRebuild)
	mux.HandleFunc("/api/search/stats", r.handleSearchStats)

	// File content API (for code viewer)
	mux.HandleFunc("/api/file/content", r.handleFileContent)

	// Guardian API routes
	mux.HandleFunc("/api/guardian/clients", r.handleGuardianClientsAPI)
	mux.HandleFunc("/api/guardian/local-clients", r.handleGuardianLocalClientsAPI)
	mux.HandleFunc("/api/guardian/inject", r.handleGuardianInject)
	mux.HandleFunc("/api/guardian/partitions", r.handleGuardianPartitions)
	mux.HandleFunc("/api/guardian/partitions/", r.handleGuardianPartition)

	// Health check endpoint for HAProxy
	mux.HandleFunc("/health", r.handleHealth)

	// Prometheus metrics
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))

	// Static files (logo, etc.)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles))))

	// SPA — serve Vue dist; fall back to index.html for all non-API paths
	dist, err := fs.Sub(spaFS, "ui/dist")
	if err != nil {
		panic("spaFS sub: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(dist))
	mux.HandleFunc("/assets/", func(w http.ResponseWriter, req *http.Request) {
		fileServer.ServeHTTP(w, req)
	})
	mux.HandleFunc("/favicon.svg", func(w http.ResponseWriter, req *http.Request) {
		fileServer.ServeHTTP(w, req)
	})
	mux.HandleFunc("/icons.svg", func(w http.ResponseWriter, req *http.Request) {
		fileServer.ServeHTTP(w, req)
	})
	mux.HandleFunc("/", r.handleSPA(dist))

	return mux
}

// handleSPA serves the Vue SPA index.html for all non-asset routes.
func (r *Router) handleSPA(dist fs.FS) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		// Try to serve the requested file first
		path := strings.TrimPrefix(req.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		f, err := dist.Open(path)
		if err == nil {
			f.Close()
			http.FileServer(http.FS(dist)).ServeHTTP(w, req)
			return
		}
		// SPA fallback — serve index.html
		index, err := dist.Open("index.html")
		if err != nil {
			http.Error(w, "UI not built", http.StatusServiceUnavailable)
			return
		}
		defer index.Close()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, req, "index.html", time.Time{}, index.(io.ReadSeeker))
	}
}

func (r *Router) handleIngest(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Enforce ingestion whitelist
	if r.whitelist.Enabled() {
		clientID := req.FormValue("client_id")
		if clientID == "" {
			clientID = req.Header.Get("X-Client-ID")
		}
		if !r.whitelist.IsAllowed(clientID) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("client %q is not whitelisted for ingestion", clientID),
			})
			return
		}
	}

	source := req.FormValue("source")
	ref := req.FormValue("ref")
	sourceID := req.FormValue("source_id") // Optional: auto-generated if empty
	ingestionType := req.FormValue("ingestion_type")
	fetchType := req.FormValue("fetch_type")
	replicateData := req.FormValue("replicate_data") == "true"

	if source == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "source is required",
		})
		return
	}

	// Default backend types
	if ingestionType == "" {
		ingestionType = "git"
	}
	if fetchType == "" {
		fetchType = "git"
	}

	// Guardian ingestion is not allowed through the UI — use the guardian API
	if ingestionType == "guardian" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "guardian ingestion must use the guardian API endpoint",
		})
		return
	}

	// Parse backend config from form (format: ingestion_config[key]=value)
	ingestionConfig := make(map[string]string)
	fetchConfig := make(map[string]string)
	// Form parsing for nested configs would go here if needed

	// Start ingestion asynchronously to avoid blocking HTTP request
	go func() {
		stream := &mockIngestStream{ctx: context.Background()}

		err := r.IngestRepository(&pb.IngestRequest{
			Source:          source,
			Ref:             ref,
			SourceId:        sourceID,
			IngestionType:   parseIngestionTypeString(ingestionType),
			FetchType:       parseFetchTypeString(fetchType),
			ReplicateData:   replicateData,
			IngestionConfig: ingestionConfig,
			FetchConfig:     fetchConfig,
		}, stream)

		if err != nil {
			r.logger.Error("async ingestion failed",
				"source", source,
				"error", err)
		}
	}()

	// Return immediately - client will poll /api/repositories for progress
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Ingestion started",
		"status":  "in_progress",
	})
}

// parseIngestionTypeString converts string to IngestionType enum
func parseIngestionTypeString(s string) pb.IngestionType {
	switch s {
	case "git":
		return pb.IngestionType_INGESTION_GIT
	case "s3":
		return pb.IngestionType_INGESTION_S3
	case "file":
		return pb.IngestionType_INGESTION_FILE
	case "guardian":
		return pb.IngestionType_INGESTION_GUARDIAN
	default:
		return pb.IngestionType_INGESTION_GIT
	}
}

// parseFetchTypeString converts string to SourceType enum
func parseFetchTypeString(s string) pb.SourceType {
	switch s {
	case "git":
		return pb.SourceType_SOURCE_TYPE_GIT
	case "blob":
		return pb.SourceType_SOURCE_TYPE_BLOB
	default:
		return pb.SourceType_SOURCE_TYPE_BLOB
	}
}

func (r *Router) handleStatus(w http.ResponseWriter, req *http.Request) {
	// Serve cached data when fresh to keep UI responsive
	const cacheTTL = 2 * time.Second
	r.statusCacheMu.RLock()
	if r.statusCache != nil && time.Since(r.statusCacheAt) < cacheTTL {
		data := r.statusCache
		r.statusCacheMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
		return
	}
	r.statusCacheMu.RUnlock()

	// Request data via channel (non-blocking, handled by separate goroutine)
	data, err := r.sendUIRequest(UIRequestStatus, 5*time.Second)
	if err != nil {
		r.logger.Error("failed to get status", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "Service temporarily unavailable",
		})
		return
	}

	if status, ok := data.(*StatusData); ok {
		r.statusCacheMu.Lock()
		r.statusCache = status
		r.statusCacheAt = time.Now()
		r.statusCacheMu.Unlock()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func (r *Router) handleRepositoriesList(w http.ResponseWriter, req *http.Request) {
	// Serve cached data when fresh to keep UI responsive under load
	const cacheTTL = 3 * time.Second
	r.repoCacheMu.RLock()
	if r.repoCache != nil && time.Since(r.repoCacheAt) < cacheTTL {
		data := r.repoCache
		r.repoCacheMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
		return
	}
	r.repoCacheMu.RUnlock()

	// Request data via channel (non-blocking, handled by separate goroutine)
	data, err := r.sendUIRequest(UIRequestRepositories, 5*time.Second)
	if err != nil {
		r.logger.Error("failed to get repositories", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "Service temporarily unavailable",
		})
		return
	}

	if repos, ok := data.(*RepositoriesData); ok {
		r.repoCacheMu.Lock()
		r.repoCache = repos
		r.repoCacheAt = time.Now()
		r.repoCacheMu.Unlock()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func (r *Router) handleRouters(w http.ResponseWriter, req *http.Request) {
	// Serve cached data when fresh - longer TTL since this fetches from peer routers
	const cacheTTL = 3 * time.Second
	r.routersCacheMu.RLock()
	if r.routersCache != nil && time.Since(r.routersCacheAt) < cacheTTL {
		data := r.routersCache
		r.routersCacheMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
		return
	}
	r.routersCacheMu.RUnlock()

	data, err := r.sendUIRequest(UIRequestRouters, 8*time.Second)
	if err != nil {
		r.logger.Error("failed to get routers", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "Service temporarily unavailable",
		})
		return
	}

	if routers, ok := data.(*RoutersData); ok {
		r.routersCacheMu.Lock()
		r.routersCache = routers
		r.routersCacheAt = time.Now()
		r.routersCacheMu.Unlock()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func (r *Router) handleHealth(w http.ResponseWriter, req *http.Request) {
	// Simple health check - return 200 OK if router is running
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "healthy",
		"service": "monofs-router",
	})
}

func (r *Router) handleRebalance(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	storageID := req.FormValue("storage_id")
	if storageID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "storage_id is required",
		})
		return
	}

	// Check if repository exists
	r.mu.RLock()
	repo, exists := r.ingestedRepos[storageID]
	r.mu.RUnlock()

	if !exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "repository not found",
		})
		return
	}

	// Check if already rebalancing
	repo.mu.RLock()
	currentState := repo.rebalanceState
	repo.mu.RUnlock()

	if currentState != RebalanceStateStable {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "rebalancing already in progress",
			"state":   currentState.String(),
		})
		return
	}

	// Trigger rebalancing asynchronously
	r.logger.Info("manual rebalance triggered via API", "storage_id", storageID)
	go r.rebalanceRepository(storageID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "rebalancing started",
	})
}

// handleSearchAPI handles search requests
func (r *Router) handleSearchAPI(w http.ResponseWriter, req *http.Request) {
	if r.searchClient == nil {
		http.Error(w, "Search service not configured", http.StatusServiceUnavailable)
		return
	}

	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var searchReq struct {
		Query         string   `json:"query"`
		StorageID     string   `json:"storage_id"`
		CaseSensitive bool     `json:"case_sensitive"`
		Regex         bool     `json:"regex"`
		MaxResults    int      `json:"max_results"`
		FilePatterns  []string `json:"file_patterns"`
	}
	if err := json.NewDecoder(req.Body).Decode(&searchReq); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if searchReq.MaxResults <= 0 {
		searchReq.MaxResults = 100
	}

	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()

	resp, err := r.searchClient.Search(ctx, &pb.SearchRequest{
		Query:         searchReq.Query,
		StorageId:     searchReq.StorageID,
		CaseSensitive: searchReq.CaseSensitive,
		Regex:         searchReq.Regex,
		MaxResults:    int32(searchReq.MaxResults),
		FilePatterns:  searchReq.FilePatterns,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Search failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleSearchIndexes returns all search indexes
func (r *Router) handleSearchIndexes(w http.ResponseWriter, req *http.Request) {
	if r.searchClient == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   "Search service not configured or unavailable",
			"indexes": []interface{}{},
		})
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()

	resp, err := r.searchClient.ListIndexes(ctx, &pb.ListIndexesRequest{})
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": fmt.Sprintf("Failed to list indexes: %v", err),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleSearchRebuild triggers index rebuild
func (r *Router) handleSearchRebuild(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.searchClient == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   "Search service not configured or unavailable",
			"success": false,
		})
		return
	}

	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "Method not allowed",
		})
		return
	}

	var rebuildReq struct {
		StorageID string `json:"storage_id"`
		All       bool   `json:"all"`
		Force     bool   `json:"force"`
	}
	json.NewDecoder(req.Body).Decode(&rebuildReq)

	// Use longer timeout for rebuild operations (especially rebuild all)
	timeout := 60 * time.Second
	if rebuildReq.All {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	defer cancel()

	if rebuildReq.All {
		resp, err := r.searchClient.RebuildAllIndexes(ctx, &pb.RebuildAllIndexesRequest{
			Force: rebuildReq.Force,
		})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":   fmt.Sprintf("Failed to rebuild: %v", err),
				"success": false,
			})
			return
		}
		json.NewEncoder(w).Encode(resp)
	} else {
		resp, err := r.searchClient.RebuildIndex(ctx, &pb.RebuildIndexRequest{
			StorageId: rebuildReq.StorageID,
			Force:     rebuildReq.Force,
		})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":   fmt.Sprintf("Failed to rebuild: %v", err),
				"success": false,
			})
			return
		}
		json.NewEncoder(w).Encode(resp)
	}
}

// handleSearchStats returns search service statistics
func (r *Router) handleSearchStats(w http.ResponseWriter, req *http.Request) {
	if r.searchClient == nil {
		http.Error(w, "Search service not configured", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()

	resp, err := r.searchClient.GetStats(ctx, &pb.StatsRequest{})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get stats: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleGuardianClientsAPI returns the list of connected guardian clients (local + peers)
func (r *Router) handleGuardianClientsAPI(w http.ResponseWriter, req *http.Request) {
	r.guardianClientsMu.RLock()

	now := time.Now()
	clients := make([]guardianClientJSON, 0, len(r.guardianClients))
	routerName := r.config.RouterName
	if routerName == "" {
		routerName = "local"
	}
	for _, gc := range r.guardianClients {
		state := "connected"
		if now.Sub(gc.lastHeartbeat) > 60*time.Second {
			state = "stale"
		}
		clients = append(clients, guardianClientJSON{
			ClientID:      gc.clientID,
			BaseURL:       gc.baseURL,
			LastHeartbeat: gc.lastHeartbeat.Unix(),
			ConnectedSec:  int64(now.Sub(gc.lastHeartbeat).Seconds()),
			State:         state,
			Router:        routerName,
		})
	}
	r.guardianClientsMu.RUnlock()

	// Fetch guardian clients from peer routers
	for _, peer := range r.config.PeerRouters {
		peerURL, err := normalizeRouterURL(peer.URL)
		if err != nil {
			continue
		}
		peerClients := fetchPeerGuardianClients(peerURL, peer.Name)
		clients = append(clients, peerClients...)
	}

	clients = dedupeGuardianClients(clients)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"guardian_clients": clients,
		"count":            len(clients),
	})
}

// handleClientsAPI returns the list of connected clients (local + peers)
func (r *Router) handleClientsAPI(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()

	resp, err := r.ListClients(ctx, &pb.ListClientsRequest{})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to list clients: %v", err), http.StatusInternalServerError)
		return
	}

	// Fetch clients from peer routers and merge
	for _, peer := range r.config.PeerRouters {
		peerURL, err := normalizeRouterURL(peer.URL)
		if err != nil {
			continue
		}
		peerClients := fetchPeerClients(peerURL)
		if peerClients != nil {
			resp.Clients = append(resp.Clients, peerClients...)
		}
	}

	// Deduplicate by client_id (same client may appear on both routers after reconnect)
	seen := make(map[string]bool, len(resp.Clients))
	deduped := make([]*pb.ClientInfo, 0, len(resp.Clients))
	for _, c := range resp.Clients {
		if !seen[c.ClientId] {
			seen[c.ClientId] = true
			deduped = append(deduped, c)
		}
	}
	resp.Clients = deduped

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleLocalClientsAPI returns only this router's clients (called by peer routers).
func (r *Router) handleLocalClientsAPI(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()

	resp, err := r.ListClients(ctx, &pb.ListClientsRequest{})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to list clients: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleGuardianLocalClientsAPI returns only this router's guardian clients (called by peer routers).
func (r *Router) handleGuardianLocalClientsAPI(w http.ResponseWriter, req *http.Request) {
	r.guardianClientsMu.RLock()
	defer r.guardianClientsMu.RUnlock()

	now := time.Now()
	clients := make([]guardianClientJSON, 0, len(r.guardianClients))
	for _, gc := range r.guardianClients {
		state := "connected"
		if now.Sub(gc.lastHeartbeat) > 60*time.Second {
			state = "stale"
		}
		clients = append(clients, guardianClientJSON{
			ClientID:      gc.clientID,
			BaseURL:       gc.baseURL,
			LastHeartbeat: gc.lastHeartbeat.Unix(),
			ConnectedSec:  int64(now.Sub(gc.lastHeartbeat).Seconds()),
			State:         state,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"guardian_clients": clients,
		"count":            len(clients),
	})
}

// handleFileContent reads file content for the code viewer
func (r *Router) handleFileContent(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var request struct {
		StorageID string `json:"storage_id"`
		FilePath  string `json:"file_path"`
	}
	if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if request.StorageID == "" || request.FilePath == "" {
		http.Error(w, "storage_id and file_path are required", http.StatusBadRequest)
		return
	}

	// Increase timeout for streaming large files or slow connections
	ctx, cancel := context.WithTimeout(req.Context(), 120*time.Second)
	defer cancel()

	// Get repository info to retrieve display_path (repoID)
	r.mu.RLock()
	repo, exists := r.ingestedRepos[request.StorageID]
	r.mu.RUnlock()

	if !exists {
		http.Error(w, "Repository not found", http.StatusNotFound)
		return
	}

	displayPath := repo.repoID

	// Get the node that has this file
	nodeResp, err := r.GetNodeForFile(ctx, &pb.GetNodeForFileRequest{
		StorageId: request.StorageID,
		FilePath:  request.FilePath,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to locate file: %v", err), http.StatusNotFound)
		return
	}

	// Get the node client
	r.mu.RLock()
	nodeState := r.nodes[nodeResp.NodeId]
	r.mu.RUnlock()

	if nodeState == nil || nodeState.client == nil {
		http.Error(w, "Node not available", http.StatusServiceUnavailable)
		return
	}

	// Read the file content using gRPC streaming
	// Backend expects: display_path/file_path
	fullPath := displayPath + "/" + request.FilePath
	stream, err := nodeState.client.Read(ctx, &pb.ReadRequest{
		Path:   fullPath,
		Offset: 0,
		Size:   10 * 1024 * 1024, // Max 10MB for code viewer
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read file: %v", err), http.StatusInternalServerError)
		return
	}

	// Collect all chunks
	var content []byte
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to read file data: %v", err), http.StatusInternalServerError)
			return
		}
		content = append(content, chunk.Data...)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"content": string(content),
	})
}

// handleFetchersAPI returns fetcher cluster statistics
func (r *Router) handleFetchersAPI(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()

	// Always request source stats so cluster-level blob_stats are populated.
	stats, err := r.GetFetcherClusterStats(ctx, true)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":     err.Error(),
			"available": false,
		})
		return
	}

	// Strip per-fetcher source stats unless ?detailed=true to keep the response small.
	if req.URL.Query().Get("detailed") != "true" {
		for i := range stats.Fetchers {
			stats.Fetchers[i].SourceStats = nil
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// handleLogEngineAPI returns per-node doctor telemetry logengine stats.
func (r *Router) handleLogEngineAPI(w http.ResponseWriter, _ *http.Request) {
	r.mu.RLock()
	nodesSnapshot := make(map[string]*nodeState, len(r.nodes))
	for k, v := range r.nodes {
		nodesSnapshot[k] = v
	}
	r.mu.RUnlock()

	type nodeLogStats struct {
		NodeID       string `json:"node_id"`
		Address      string `json:"address"`
		Enabled      bool   `json:"enabled"`
		LogChunks    int64  `json:"log_chunks"`
		MetricChunks int64  `json:"metric_chunks"`
		TraceChunks  int64  `json:"trace_chunks"`
	}

	var (
		nodes        []nodeLogStats
		totalLogs    int64
		totalMetrics int64
		totalTraces  int64
		anyEnabled   bool
	)

	for nodeID, state := range nodesSnapshot {
		ns := nodeLogStats{
			NodeID:  nodeID,
			Address: state.externalAddress,
		}
		if le := state.logEngineStats; le != nil {
			ns.Enabled = le.Enabled
			ns.LogChunks = le.LogChunks
			ns.MetricChunks = le.MetricChunks
			ns.TraceChunks = le.TraceChunks
			if le.Enabled {
				anyEnabled = true
				totalLogs += le.LogChunks
				totalMetrics += le.MetricChunks
				totalTraces += le.TraceChunks
			}
		}
		nodes = append(nodes, ns)
	}

	// Sort for stable output.
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"enabled":       anyEnabled,
		"log_chunks":    totalLogs,
		"metric_chunks": totalMetrics,
		"trace_chunks":  totalTraces,
		"nodes":         nodes,
	})
}
func (r *Router) handleDependenciesAPI(w http.ResponseWriter, req *http.Request) {
	data, err := r.sendUIRequest(UIRequestDependencies, 10*time.Second)
	if err != nil {
		r.logger.Error("failed to get dependencies", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "Service temporarily unavailable",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// handlePredictorAPI returns predictor statistics from all storage nodes.
func (r *Router) handlePredictorAPI(w http.ResponseWriter, req *http.Request) {
	r.mu.RLock()
	nodesSnapshot := make(map[string]*nodeState, len(r.nodes))
	for k, v := range r.nodes {
		nodesSnapshot[k] = v
	}
	staleThreshold := r.config.UnhealthyThreshold
	r.mu.RUnlock()

	type nodePredictorStats struct {
		NodeID         string  `json:"node_id"`
		Address        string  `json:"address"`
		Enabled        bool    `json:"enabled"`
		MarkovChains   int32   `json:"markov_chains"`
		DirectoryMaps  int32   `json:"directory_maps"`
		Predictions    int64   `json:"predictions"`
		Prefetches     int64   `json:"prefetches"`
		PrefetchHits   int64   `json:"prefetch_hits"`
		PrefetchMisses int64   `json:"prefetch_misses"`
		HitRate        float64 `json:"hit_rate"`
		Error          string  `json:"error,omitempty"`
	}

	var (
		results []nodePredictorStats
		mu      sync.Mutex
		wg      sync.WaitGroup
	)

	for _, state := range nodesSnapshot {
		state := state
		if state.client == nil || !state.info.Healthy {
			continue
		}
		if staleThreshold > 0 && time.Since(state.lastSeen) > staleThreshold {
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			resp, err := state.client.GetPredictorStats(ctx, &pb.PredictorStatsRequest{})
			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				results = append(results, nodePredictorStats{
					NodeID:  state.info.NodeId,
					Address: state.info.Address,
					Error:   err.Error(),
				})
				return
			}

			results = append(results, nodePredictorStats{
				NodeID:         resp.NodeId,
				Address:        state.info.Address,
				Enabled:        resp.Enabled,
				MarkovChains:   resp.MarkovChains,
				DirectoryMaps:  resp.DirectoryMaps,
				Predictions:    resp.Predictions,
				Prefetches:     resp.Prefetches,
				PrefetchHits:   resp.PrefetchHits,
				PrefetchMisses: resp.PrefetchMisses,
				HitRate:        resp.HitRate,
			})
		}()
	}

	wg.Wait()

	// Compute cluster totals
	var totalPredictions, totalPrefetches, totalHits, totalMisses int64
	var totalChains, totalDirs int32
	enabledNodes := 0
	for _, r := range results {
		if r.Enabled {
			enabledNodes++
			totalChains += r.MarkovChains
			totalDirs += r.DirectoryMaps
			totalPredictions += r.Predictions
			totalPrefetches += r.Prefetches
			totalHits += r.PrefetchHits
			totalMisses += r.PrefetchMisses
		}
	}

	var clusterHitRate float64
	if total := float64(totalHits + totalMisses); total > 0 {
		clusterHitRate = float64(totalHits) / total
	}

	response := map[string]interface{}{
		"nodes":             results,
		"total_nodes":       len(results),
		"enabled_nodes":     enabledNodes,
		"total_predictions": totalPredictions,
		"total_prefetches":  totalPrefetches,
		"total_hits":        totalHits,
		"total_misses":      totalMisses,
		"cluster_hit_rate":  clusterHitRate,
		"total_chains":      totalChains,
		"total_dir_maps":    totalDirs,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleGuardianInject handles POST /api/guardian/inject — ingest a guardian partition
func (r *Router) handleGuardianInject(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token := req.Header.Get("X-Guardian-Token")
	if token == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "X-Guardian-Token header is required"})
		return
	}

	if _, ok := r.validateGuardianToken(token); !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid guardian token"})
		return
	}

	source := req.FormValue("source")
	partitionName := req.FormValue("partition_name")
	ref := req.FormValue("ref")

	if source == "" || partitionName == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "source and partition_name are required"})
		return
	}

	if strings.Contains(partitionName, "/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "partition_name must not contain '/'"})
		return
	}

	ingestionConfig := map[string]string{"guardian_token": token}

	go func() {
		err := r.injectGuardianPartitionFromSource(context.Background(), source, ref, partitionName, ingestionConfig["guardian_token"])
		if err != nil {
			r.logger.Error("guardian injection failed", "partition", partitionName, "error", err)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Guardian ingestion started for partition: " + partitionName,
	})
}

// handleGuardianPartitions handles GET /api/guardian/partitions — list guardian partitions
func (r *Router) handleGuardianPartitions(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data := r.buildRepositoriesData()
	var guardianRepos []map[string]interface{}
	for _, repo := range data.Repositories {
		if isGuardian, ok := repo["is_guardian"].(bool); ok && isGuardian {
			guardianRepos = append(guardianRepos, repo)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"partitions": guardianRepos,
	})
}

// handleGuardianPartition handles DELETE /api/guardian/partitions/{name}[/files?path=...][/dirs?path=...]
func (r *Router) handleGuardianPartition(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token := req.Header.Get("X-Guardian-Token")
	if token == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "X-Guardian-Token header is required"})
		return
	}

	if _, ok := r.validateGuardianToken(token); !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid guardian token"})
		return
	}

	// Parse path: /api/guardian/partitions/{name}[/files][/dirs]
	pathParts := strings.Split(strings.TrimPrefix(req.URL.Path, "/api/guardian/partitions/"), "/")
	if len(pathParts) == 0 || pathParts[0] == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "partition name is required"})
		return
	}

	partitionName := pathParts[0]
	displayPath := "guardian/" + partitionName
	storageID := sharding.GenerateStorageID(displayPath)

	var subAction string
	if len(pathParts) > 1 {
		subAction = pathParts[1]
	}

	switch subAction {
	case "files":
		// DELETE /api/guardian/partitions/{name}/files?path=some/file.txt
		filePath := req.URL.Query().Get("path")
		if filePath == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "path query parameter is required"})
			return
		}
		resp, err := r.deleteGuardianFileFromAllNodes(storageID, filePath)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)

	case "dirs":
		// DELETE /api/guardian/partitions/{name}/dirs?path=some/subdir
		dirPath := req.URL.Query().Get("path")
		if dirPath == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "path query parameter is required"})
			return
		}
		resp, err := r.deleteGuardianDirFromAllNodes(storageID, dirPath)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)

	case "":
		// DELETE /api/guardian/partitions/{name} — delete entire partition
		resp, err := r.deleteRepositoryInternal(req.Context(), storageID)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"success": resp.Success, "message": resp.Message})

	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "unknown sub-action: " + subAction})
	}
}

type pprofCollectRequest struct {
	Profiles           []string `json:"profiles"`
	CpuDurationSeconds int      `json:"cpu_duration_seconds"`
}

type pprofTarget struct {
	ServiceType string `json:"service_type"`
	Name        string `json:"name"`
	Address     string `json:"address"`
	BaseURL     string `json:"base_url"`
}

type pprofProfileResult struct {
	Profile string `json:"profile"`
	OK      bool   `json:"ok"`
	Bytes   int    `json:"bytes,omitempty"`
	Error   string `json:"error,omitempty"`
}

type pprofTargetResult struct {
	ServiceType string               `json:"service_type"`
	Name        string               `json:"name"`
	Address     string               `json:"address"`
	BaseURL     string               `json:"base_url"`
	Profiles    []pprofProfileResult `json:"profiles"`
}

type pprofCollectManifest struct {
	GeneratedAt          string              `json:"generated_at"`
	Profiles             []string            `json:"profiles"`
	CpuDurationSeconds   int                 `json:"cpu_duration_seconds"`
	RequestedTargetCount int                 `json:"requested_target_count"`
	Results              []pprofTargetResult `json:"results"`
}

func (r *Router) handlePprofCollectAPI(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyReq := pprofCollectRequest{}
	if req.Body != nil {
		defer req.Body.Close()
		if err := json.NewDecoder(req.Body).Decode(&bodyReq); err != nil && err != io.EOF {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
	}

	profiles := normalizePprofProfiles(bodyReq.Profiles)
	if len(profiles) == 0 {
		profiles = []string{"cpu", "heap", "goroutine"}
	}
	cpuSeconds := bodyReq.CpuDurationSeconds
	if cpuSeconds <= 0 {
		cpuSeconds = 30
	}
	if cpuSeconds > 120 {
		cpuSeconds = 120
	}

	targets := r.collectPprofTargets(req)
	if len(targets) == 0 {
		http.Error(w, "No pprof targets discovered", http.StatusServiceUnavailable)
		return
	}

	r.logger.Info("pprof collection starting",
		"targets", len(targets),
		"profiles", profiles,
		"cpu_seconds", cpuSeconds)

	collectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	manifest, files := collectPprofArtifacts(collectCtx, targets, profiles, cpuSeconds)

	var failedTargets, failedProfiles int
	for _, result := range manifest.Results {
		for _, p := range result.Profiles {
			if !p.OK {
				failedProfiles++
				r.logger.Warn("pprof profile fetch failed",
					"service_type", result.ServiceType,
					"name", result.Name,
					"address", result.Address,
					"profile", p.Profile,
					"error", p.Error)
			}
		}
		hasTargetFailed := false
		for _, p := range result.Profiles {
			if !p.OK {
				hasTargetFailed = true
				break
			}
		}
		if hasTargetFailed {
			failedTargets++
		}
	}
	r.logger.Info("pprof collection completed",
		"targets", len(targets),
		"failed_targets", failedTargets,
		"failed_profiles", failedProfiles,
		"files_collected", len(files))

	buf := bytes.NewBuffer(nil)
	zw := zip.NewWriter(buf)
	for name, data := range files {
		fileWriter, err := zw.Create(name)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to create zip entry: %v", err), http.StatusInternalServerError)
			return
		}
		if _, err := fileWriter.Write(data); err != nil {
			http.Error(w, fmt.Sprintf("Failed to write zip entry: %v", err), http.StatusInternalServerError)
			return
		}
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to encode manifest: %v", err), http.StatusInternalServerError)
		return
	}
	manifestWriter, err := zw.Create("manifest.json")
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create manifest entry: %v", err), http.StatusInternalServerError)
		return
	}
	if _, err := manifestWriter.Write(manifestBytes); err != nil {
		http.Error(w, fmt.Sprintf("Failed to write manifest entry: %v", err), http.StatusInternalServerError)
		return
	}
	if err := zw.Close(); err != nil {
		http.Error(w, fmt.Sprintf("Failed to finalize zip: %v", err), http.StatusInternalServerError)
		return
	}

	succeeded := len(targets) - failedTargets
	filename := fmt.Sprintf("monofs-pprof-%d.zip", time.Now().Unix())
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("X-Pprof-Target-Count", strconv.Itoa(len(targets)))
	w.Header().Set("X-Pprof-Success-Targets", strconv.Itoa(succeeded))
	w.Header().Set("X-Pprof-Failed-Targets", strconv.Itoa(failedTargets))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func (r *Router) collectPprofTargets(req *http.Request) []pprofTarget {
	targets := make([]pprofTarget, 0, 16)
	seen := make(map[string]bool)

	addTarget := func(t pprofTarget) {
		if t.BaseURL == "" {
			return
		}
		key := t.ServiceType + "|" + t.Name + "|" + t.BaseURL
		if seen[key] {
			return
		}
		seen[key] = true
		targets = append(targets, t)
	}

	// Routers: local router + configured peers.
	if baseURL := routerBaseURLFromRequest(req); baseURL != "" {
		addTarget(pprofTarget{
			ServiceType: "router",
			Name:        r.config.RouterName,
			Address:     baseURL,
			BaseURL:     baseURL,
		})
	}
	for _, peer := range r.config.PeerRouters {
		peerURL, err := normalizeRouterURL(peer.URL)
		if err != nil {
			continue
		}
		addTarget(pprofTarget{
			ServiceType: "router",
			Name:        peer.Name,
			Address:     peerURL,
			BaseURL:     peerURL,
		})
	}

	// Storage servers: explicit diagnostics addresses first, then gRPC+100 convention.
	r.mu.RLock()
	for _, state := range r.nodes {
		if state == nil || state.info == nil {
			continue
		}
		var diagAddr string
		var baseURL string
		if configured, ok := r.config.ServerDiagnostics[state.info.NodeId]; ok {
			baseURL, diagAddr = diagnosticsEndpoint(configured)
			if baseURL == "" {
				r.logger.Warn("skipping server pprof target: invalid explicit diagnostics config",
					"node_id", state.info.NodeId,
					"configured", configured)
				continue
			}
		} else {
			var err error
			diagAddr, err = addressWithOffset(state.info.Address, 100)
			if err != nil {
				r.logger.Warn("skipping server pprof target: cannot compute diagnostics address",
					"node_id", state.info.NodeId,
					"address", state.info.Address,
					"error", err)
				continue
			}
			baseURL = "http://" + diagAddr
		}
		addTarget(pprofTarget{
			ServiceType: "server",
			Name:        state.info.NodeId,
			Address:     diagAddr,
			BaseURL:     baseURL,
		})
	}
	r.mu.RUnlock()

	// Search service diagnostics: explicit address first, then gRPC+1 convention.
	if diagURL, diagAddress := diagnosticsEndpoint(r.config.SearchDiagnostics); diagURL != "" {
		addTarget(pprofTarget{
			ServiceType: "search",
			Name:        "search",
			Address:     diagAddress,
			BaseURL:     diagURL,
		})
	} else if searchAddr := r.getSearchAddress(); strings.TrimSpace(searchAddr) != "" {
		if diagAddr, err := addressWithOffset(searchAddr, 1); err == nil {
			addTarget(pprofTarget{
				ServiceType: "search",
				Name:        "search",
				Address:     diagAddr,
				BaseURL:     "http://" + diagAddr,
			})
		} else {
			r.logger.Warn("skipping search pprof target: cannot compute diagnostics address",
				"search_addr", searchAddr,
				"error", err)
		}
	}

	// Fetcher diagnostics: explicit addresses first, then gRPC+1 convention.
	if len(r.config.FetcherDiagnostics) > 0 {
		for _, configured := range r.config.FetcherDiagnostics {
			diagURL, diagAddress := diagnosticsEndpoint(configured)
			if diagURL == "" {
				continue
			}
			addTarget(pprofTarget{
				ServiceType: "fetcher",
				Name:        diagAddress,
				Address:     diagAddress,
				BaseURL:     diagURL,
			})
		}
	} else if fetcherClient := r.getFetcherClient(); fetcherClient != nil {
		for _, fetcherAddr := range fetcherClient.AllFetchers() {
			diagAddr, err := addressWithOffset(fetcherAddr, 1)
			if err != nil {
				r.logger.Warn("skipping fetcher pprof target: cannot compute diagnostics address",
					"fetcher_addr", fetcherAddr,
					"error", err)
				continue
			}
			addTarget(pprofTarget{
				ServiceType: "fetcher",
				Name:        fetcherAddr,
				Address:     diagAddr,
				BaseURL:     "http://" + diagAddr,
			})
		}
	}

	r.logger.Info("pprof targets discovered",
		"count", len(targets),
		"service_types", pprofTargetServiceTypes(targets))

	return targets
}

func collectPprofArtifacts(parent context.Context, targets []pprofTarget, profiles []string, cpuSeconds int) (pprofCollectManifest, map[string][]byte) {
	manifest := pprofCollectManifest{
		GeneratedAt:          time.Now().UTC().Format(time.RFC3339),
		Profiles:             append([]string(nil), profiles...),
		CpuDurationSeconds:   cpuSeconds,
		RequestedTargetCount: len(targets),
		Results:              make([]pprofTargetResult, 0, len(targets)),
	}

	files := make(map[string][]byte)
	results := make([]pprofTargetResult, 0, len(targets))

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	for _, target := range targets {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := pprofTargetResult{
				ServiceType: target.ServiceType,
				Name:        target.Name,
				Address:     target.Address,
				BaseURL:     target.BaseURL,
				Profiles:    make([]pprofProfileResult, 0, len(profiles)),
			}

			for _, profile := range profiles {
				data, err := fetchPprofProfile(parent, target.BaseURL, profile, cpuSeconds)
				entry := pprofProfileResult{Profile: profile}
				if err != nil {
					entry.OK = false
					entry.Error = err.Error()
					result.Profiles = append(result.Profiles, entry)
					continue
				}

				entry.OK = true
				entry.Bytes = len(data)
				result.Profiles = append(result.Profiles, entry)

				fileName := fmt.Sprintf("%s/%s/%s.pprof", sanitizePathSegment(target.ServiceType), sanitizePathSegment(target.Name), sanitizePathSegment(profile))
				mu.Lock()
				files[fileName] = data
				mu.Unlock()
			}

			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}()
	}

	wg.Wait()
	sort.Slice(results, func(i, j int) bool {
		if results[i].ServiceType == results[j].ServiceType {
			return results[i].Name < results[j].Name
		}
		return results[i].ServiceType < results[j].ServiceType
	})
	manifest.Results = results
	return manifest, files
}

func fetchPprofProfile(parent context.Context, baseURL, profile string, cpuSeconds int) ([]byte, error) {
	path := "/debug/pprof/" + profile
	if profile == "cpu" {
		path = "/debug/pprof/profile?seconds=" + strconv.Itoa(cpuSeconds)
	}

	ctx, cancel := context.WithTimeout(parent, profileTimeout(profile, cpuSeconds))
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: profileTimeout(profile, cpuSeconds)}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", response.StatusCode)
	}

	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty profile")
	}
	return data, nil
}

func profileTimeout(profile string, cpuSeconds int) time.Duration {
	if profile == "cpu" {
		return time.Duration(cpuSeconds+15) * time.Second
	}
	if profile == "trace" {
		return 45 * time.Second
	}
	return 20 * time.Second
}

func normalizePprofProfiles(input []string) []string {
	allowed := map[string]bool{
		"cpu":          true,
		"heap":         true,
		"goroutine":    true,
		"allocs":       true,
		"mutex":        true,
		"block":        true,
		"threadcreate": true,
		"trace":        true,
	}
	seen := make(map[string]bool)
	profiles := make([]string, 0, len(input))
	for _, item := range input {
		profile := strings.ToLower(strings.TrimSpace(item))
		if !allowed[profile] || seen[profile] {
			continue
		}
		seen[profile] = true
		profiles = append(profiles, profile)
	}
	return profiles
}

func sanitizePathSegment(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "unknown"
	}
	trimmed = strings.ReplaceAll(trimmed, ":", "_")
	trimmed = strings.ReplaceAll(trimmed, "/", "_")
	trimmed = strings.ReplaceAll(trimmed, "\\", "_")
	trimmed = strings.ReplaceAll(trimmed, "..", "_")
	cleaned := filepath.Clean(trimmed)
	if cleaned == "." || cleaned == "" {
		return "unknown"
	}
	return cleaned
}

func pprofTargetServiceTypes(targets []pprofTarget) []string {
	seen := make(map[string]bool)
	types := make([]string, 0, 4)
	for _, t := range targets {
		if !seen[t.ServiceType] {
			seen[t.ServiceType] = true
			types = append(types, t.ServiceType)
		}
	}
	sort.Strings(types)
	return types
}

func routerBaseURLFromRequest(req *http.Request) string {
	if req == nil {
		return ""
	}
	host := strings.TrimSpace(req.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(req.Host)
	}
	if host == "" {
		return ""
	}
	if strings.Contains(host, ",") {
		host = strings.TrimSpace(strings.Split(host, ",")[0])
	}
	proto := strings.TrimSpace(req.Header.Get("X-Forwarded-Proto"))
	if proto == "" {
		if req.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	return fmt.Sprintf("%s://%s", proto, host)
}

func addressWithOffset(address string, portOffset int) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return "", err
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(portNum+portOffset)), nil
}

func diagnosticsEndpoint(raw string) (baseURL string, address string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ""
	}
	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err != nil || parsed.Host == "" {
			return "", ""
		}
		return strings.TrimRight(parsed.String(), "/"), parsed.Host
	}
	return "http://" + trimmed, trimmed
}

// fetchPeerClients fetches FUSE clients from a peer router's /api/local-clients endpoint.
// Returns nil on any error (best-effort aggregation).
func fetchPeerClients(peerURL string) []*pb.ClientInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, peerURL+"/api/local-clients", nil)
	if err != nil {
		return nil
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(request)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var data pb.ListClientsResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil
	}
	return data.Clients
}

// guardianClientJSON is the JSON shape for guardian client info in API responses.
type guardianClientJSON struct {
	ClientID      string `json:"client_id"`
	BaseURL       string `json:"base_url"`
	LastHeartbeat int64  `json:"last_heartbeat"`
	ConnectedSec  int64  `json:"connected_sec"`
	State         string `json:"state"`
	Router        string `json:"router"`
}

func dedupeGuardianClients(clients []guardianClientJSON) []guardianClientJSON {
	if len(clients) <= 1 {
		return clients
	}

	bestByID := make(map[string]guardianClientJSON, len(clients))
	order := make([]string, 0, len(clients))
	for _, client := range clients {
		existing, ok := bestByID[client.ClientID]
		if !ok {
			bestByID[client.ClientID] = client
			order = append(order, client.ClientID)
			continue
		}

		if client.LastHeartbeat > existing.LastHeartbeat ||
			(client.LastHeartbeat == existing.LastHeartbeat && client.State == "connected" && existing.State != "connected") {
			bestByID[client.ClientID] = client
		}
	}

	deduped := make([]guardianClientJSON, 0, len(bestByID))
	for _, clientID := range order {
		deduped = append(deduped, bestByID[clientID])
	}
	return deduped
}

// fetchPeerGuardianClients fetches guardian clients from a peer router.
func fetchPeerGuardianClients(peerURL, peerName string) []guardianClientJSON {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, peerURL+"/api/guardian/local-clients", nil)
	if err != nil {
		return nil
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(request)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var data struct {
		GuardianClients []guardianClientJSON `json:"guardian_clients"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil
	}

	for i := range data.GuardianClients {
		data.GuardianClients[i].Router = peerName
	}
	return data.GuardianClients
}

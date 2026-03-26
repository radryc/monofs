// Package router provides HTTP UI handlers for MonoFS.
package router

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/sharding"
	"google.golang.org/grpc/metadata"
)

//go:embed templates/*
var templates embed.FS

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

// ServeHTTP returns an HTTP handler for the web UI.
func (r *Router) ServeHTTP() http.Handler {
	mux := http.NewServeMux()

	// Page routes
	mux.HandleFunc("/", r.handleDashboard)
	mux.HandleFunc("/cluster", r.handleClusterPage)
	mux.HandleFunc("/clients", r.handleClientsPage)
	mux.HandleFunc("/performance", r.handlePerformancePage)
	mux.HandleFunc("/replication", r.handleReplicationPage)
	mux.HandleFunc("/repositories", r.handleRepositoriesPage)
	mux.HandleFunc("/ingest", r.handleIngestPage)
	mux.HandleFunc("/search", r.handleSearchPage)
	mux.HandleFunc("/indexer", r.handleIndexerPage)
	mux.HandleFunc("/fetchers", r.handleFetchersPage)
	mux.HandleFunc("/dependencies", r.handleDependenciesPage)

	// API routes
	mux.HandleFunc("/api/ingest", r.handleIngest)
	mux.HandleFunc("/api/status", r.handleStatus)
	mux.HandleFunc("/api/repositories", r.handleRepositoriesList)
	mux.HandleFunc("/api/routers", r.handleRouters)
	mux.HandleFunc("/api/rebalance", r.handleRebalance)
	mux.HandleFunc("/api/clients", r.handleClientsAPI)
	mux.HandleFunc("/api/local-clients", r.handleLocalClientsAPI)
	mux.HandleFunc("/api/fetchers", r.handleFetchersAPI)
	mux.HandleFunc("/api/dependencies", r.handleDependenciesAPI)

	// Whitelist API routes
	mux.HandleFunc("/api/whitelist", r.handleWhitelistAPI)
	mux.HandleFunc("/api/whitelist/toggle", r.handleWhitelistToggleAPI)

	// Predictor API route
	mux.HandleFunc("/api/predictor", r.handlePredictorAPI)

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

	// Static files (logo, etc.)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles))))

	return mux
}

func (r *Router) handleDashboard(w http.ResponseWriter, req *http.Request) {
	// Only match exact root path
	if req.URL.Path != "/" {
		http.NotFound(w, req)
		return
	}
	r.renderPage(w, "dashboard", "Dashboard")
}

func (r *Router) handleClusterPage(w http.ResponseWriter, req *http.Request) {
	r.renderPage(w, "cluster", "Cluster")
}

func (r *Router) handleClientsPage(w http.ResponseWriter, req *http.Request) {
	r.renderPage(w, "clients", "Clients")
}

func (r *Router) handlePerformancePage(w http.ResponseWriter, req *http.Request) {
	r.renderPage(w, "performance", "Performance")
}

func (r *Router) handleReplicationPage(w http.ResponseWriter, req *http.Request) {
	r.renderPage(w, "replication", "Replication")
}

func (r *Router) handleRepositoriesPage(w http.ResponseWriter, req *http.Request) {
	r.renderPage(w, "repositories", "Repositories")
}

func (r *Router) handleIngestPage(w http.ResponseWriter, req *http.Request) {
	r.renderPage(w, "ingest", "Ingest")
}

func (r *Router) handleSearchPage(w http.ResponseWriter, req *http.Request) {
	r.renderPage(w, "search", "Search")
}

func (r *Router) handleIndexerPage(w http.ResponseWriter, req *http.Request) {
	r.renderPage(w, "indexer", "Indexer")
}

func (r *Router) handleFetchersPage(w http.ResponseWriter, req *http.Request) {
	r.renderPage(w, "fetchers", "Fetchers")
}

func (r *Router) handleDependenciesPage(w http.ResponseWriter, req *http.Request) {
	r.renderPage(w, "dependencies", "Dependencies")
}

func (r *Router) renderPage(w http.ResponseWriter, page, title string) {
	tmpl, err := template.ParseFS(templates,
		"templates/layout.html",
		"templates/"+page+".html",
	)
	if err != nil {
		http.Error(w, "Failed to load template: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data := map[string]string{
		"Page":  page,
		"Title": title,
	}

	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, "Failed to render template: "+err.Error(), http.StatusInternalServerError)
		return
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
		stream := &mockIngestStream{ctx: context.Background()}
		err := r.IngestRepository(&pb.IngestRequest{
			Source:          source,
			Ref:             ref,
			SourceId:        partitionName,
			IngestionType:   pb.IngestionType_INGESTION_GUARDIAN,
			FetchType:       pb.SourceType_SOURCE_TYPE_BLOB,
			IngestionConfig: ingestionConfig,
		}, stream)
		if err != nil {
			r.logger.Error("guardian ingestion failed", "partition", partitionName, "error", err)
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

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
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc/metadata"
)

//go:embed templates/*
var templates embed.FS

//go:embed static/*
var staticFiles embed.FS

// mockIngestStream implements MonoFSRouter_IngestRepositoryServer for HTTP ingestion
type mockIngestStream struct {
	progress []*pb.IngestProgress
	ctx      context.Context
}

func (s *mockIngestStream) Send(p *pb.IngestProgress) error {
	s.progress = append(s.progress, p)
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

	// API routes
	mux.HandleFunc("/api/ingest", r.handleIngest)
	mux.HandleFunc("/api/status", r.handleStatus)
	mux.HandleFunc("/api/repositories", r.handleRepositoriesList)
	mux.HandleFunc("/api/routers", r.handleRouters)
	mux.HandleFunc("/api/rebalance", r.handleRebalance)
	mux.HandleFunc("/api/clients", r.handleClientsAPI)
	mux.HandleFunc("/api/fetchers", r.handleFetchersAPI)

	// Search API routes
	mux.HandleFunc("/api/search", r.handleSearchAPI)
	mux.HandleFunc("/api/search/indexes", r.handleSearchIndexes)
	mux.HandleFunc("/api/search/rebuild", r.handleSearchRebuild)
	mux.HandleFunc("/api/search/stats", r.handleSearchStats)

	// File content API (for code viewer)
	mux.HandleFunc("/api/file/content", r.handleFileContent)

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

	// Parse backend config from form (format: ingestion_config[key]=value)
	ingestionConfig := make(map[string]string)
	fetchConfig := make(map[string]string)
	// Form parsing for nested configs would go here if needed

	// Start ingestion asynchronously to avoid blocking HTTP request
	go func() {
		stream := &mockIngestStream{
			ctx:      context.Background(), // Use background context for async operation
			progress: make([]*pb.IngestProgress, 0),
		}

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
	case "go":
		return pb.IngestionType_INGESTION_GO
	case "s3":
		return pb.IngestionType_INGESTION_S3
	case "file":
		return pb.IngestionType_INGESTION_FILE
	default:
		return pb.IngestionType_INGESTION_GIT
	}
}

// parseFetchTypeString converts string to FetchType enum
func parseFetchTypeString(s string) pb.FetchType {
	switch s {
	case "git":
		return pb.FetchType_FETCH_GIT
	case "gomod":
		return pb.FetchType_FETCH_GOMOD
	case "s3":
		return pb.FetchType_FETCH_S3
	case "local":
		return pb.FetchType_FETCH_LOCAL
	default:
		return pb.FetchType_FETCH_GIT
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

// handleClientsAPI returns the list of connected clients
func (r *Router) handleClientsAPI(w http.ResponseWriter, req *http.Request) {
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

	// Check for detailed stats query param
	includeSourceStats := req.URL.Query().Get("detailed") == "true"

	stats, err := r.GetFetcherClusterStats(ctx, includeSourceStats)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":     err.Error(),
			"available": false,
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

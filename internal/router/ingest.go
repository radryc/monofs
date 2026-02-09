// Package router provides ingestion logic for MonoFS.
package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/buildlayout"
	"github.com/radryc/monofs/internal/sharding"
	"github.com/radryc/monofs/internal/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// parseCommitTime converts a commit time string to int64, returning 0 on error
func parseCommitTime(timeStr string) int64 {
	if timeStr == "" {
		return 0
	}
	val, _ := strconv.ParseInt(timeStr, 10, 64)
	return val
}

// generateStorageID creates a SHA-256 hash of the display path.
// This provides a consistent, fixed-length internal identifier regardless of
// the display path format (path-based or simple string).
func generateStorageID(displayPath string) string {
	hash := sha256.Sum256([]byte(displayPath))
	return hex.EncodeToString(hash[:])
}

// normalizeGitURL converts a Git repository URL to a normalized path format.
// Examples:
//   - https://github.com/owner/repo -> github.com/owner/repo
//   - git@github.com:owner/repo.git -> github.com/owner/repo
//
// normalizeGitURL converts a Git repository URL to a normalized path format.
// Examples:
//   - https://github.com/owner/repo -> github.com/owner/repo
//   - git@github.com:owner/repo.git -> github.com/owner/repo
func normalizeGitURL(repoURL string) string {
	u, err := url.Parse(repoURL)
	if err != nil {
		// If parsing fails, return as-is (might be a Go module path)
		return repoURL
	}

	// If it has a scheme (http://, https://, git@), process as URL
	if u.Scheme != "" {
		// Get host (keep dots as-is)
		host := u.Host

		// Get path without leading slash
		path := strings.TrimPrefix(u.Path, "/")

		// Remove .git suffix if present
		path = strings.TrimSuffix(path, ".git")

		// Combine host and path
		if path != "" {
			return host + "/" + path
		}
		return host
	}

	// No scheme - likely a Go module path like "github.com/owner/repo"
	// Return as-is, removing only .git suffix if present
	return strings.TrimSuffix(repoURL, ".git")
}

// ingestionContext holds all state for an ingestion operation
type ingestionContext struct {
	req              *pb.IngestRequest
	stream           pb.MonoFSRouter_IngestRepositoryServer
	displayPath      string
	storageID        string
	initialStorageID string // storageID at registration time (before canonical path update)
	sourceURL        string
	ref              string
	ingestionType    storage.IngestionType
	fetchType        pb.SourceType
	backend          storage.IngestionBackend
	nodes            []sharding.Node
	nodeClients      map[string]nodeClient
	sharder          *sharding.HRW
	filesIngested    *int64
	totalFiles       *int64
	collectedFiles   []buildlayout.FileInfo
	canonicalPath    string
	canonicalPathMux sync.Mutex
}

type nodeClient struct {
	nodeID string
	client pb.MonoFSClient
}

// validateAndSetup validates the request and sets up the ingestion context
func (r *Router) validateAndSetup(ctx *ingestionContext) error {
	// Validate source
	if ctx.req.Source == "" {
		return fmt.Errorf("source must be specified")
	}

	// Get ingestion handler for this type
	registry := NewIngestionRegistry()
	handler, err := registry.Get(ctx.req.IngestionType)
	if err != nil {
		return fmt.Errorf("unsupported ingestion type: %v", ctx.req.IngestionType)
	}

	// Determine display path using handler
	ctx.displayPath = handler.NormalizeDisplayPath(ctx.req.Source, ctx.req.SourceId)
	ctx.ref = ctx.req.Ref
	ctx.sourceURL = ctx.req.Source

	// Special handling for Go modules
	if ctx.req.IngestionType == pb.IngestionType_INGESTION_GO {
		if ctx.ref != "" && !strings.Contains(ctx.req.Source, "@") {
			ctx.sourceURL = ctx.req.Source + "@" + ctx.ref
			ctx.displayPath = handler.NormalizeDisplayPath(ctx.sourceURL, ctx.req.SourceId)
		}
	}

	// Apply default ref if needed
	if ctx.ref == "" && handler.GetDefaultRef() != "" {
		ctx.ref = handler.GetDefaultRef()
	}

	// Validate source format
	if err := handler.ValidateSource(ctx.sourceURL, ctx.ref); err != nil {
		return err
	}

	// Add directory prefix for package managers
	ctx.displayPath = handler.AddDirectoryPrefix(ctx.displayPath, ctx.req.Source)

	// Generate internal storage ID.
	// When displayPath is empty (npm/cargo/maven before canonical path discovery),
	// use sourceURL for a stable unique ID instead of hashing empty string.
	if ctx.displayPath != "" {
		ctx.storageID = generateStorageID(ctx.displayPath)
	} else {
		ctx.storageID = generateStorageID(ctx.sourceURL)
	}

	// Get storage types from handler
	ctx.ingestionType = handler.GetStorageType()
	ctx.fetchType = handler.GetFetchType()

	return nil
}

// initializeBackend creates and initializes the ingestion backend with progress updates
func (r *Router) initializeBackend(ctx *ingestionContext, sendProgress func(pb.IngestProgress_Stage, string, int64, int64, string)) error {
	sendProgress(pb.IngestProgress_CLONING, "Initializing backend...", 0, 0, "")

	// Create ingestion backend
	backend, err := storage.DefaultRegistry.CreateIngestionBackend(ctx.ingestionType)
	if err != nil {
		return fmt.Errorf("unsupported ingestion type: %w", err)
	}
	ctx.backend = backend

	// Prepare backend config
	config := ctx.req.IngestionConfig
	if config == nil {
		config = make(map[string]string)
	}
	config["branch"] = ctx.ref
	config["display_path"] = ctx.displayPath

	// Validate source
	sendProgress(pb.IngestProgress_CLONING, "Validating source...", 0, 0, "")
	if err := backend.Validate(ctx.stream.Context(), ctx.sourceURL, config); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// Initialize backend with progress keepalives
	initDone := make(chan struct{})
	initErr := make(chan error, 1)

	go func() {
		err := backend.Initialize(ctx.stream.Context(), ctx.sourceURL, config)
		if err != nil {
			initErr <- err
		} else {
			close(initDone)
		}
	}()

	// Send keepalive progress updates
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	initStartTime := time.Now()

	for {
		select {
		case <-initDone:
			elapsed := time.Since(initStartTime).Round(time.Second)
			sendProgress(pb.IngestProgress_CLONING, fmt.Sprintf("Backend ready in %v", elapsed), 0, 0, "")
			return nil
		case err := <-initErr:
			return fmt.Errorf("failed to initialize backend: %w", err)
		case <-ticker.C:
			elapsed := time.Since(initStartTime).Round(time.Second)
			sendProgress(pb.IngestProgress_CLONING, fmt.Sprintf("Initializing backend... (%v elapsed)", elapsed), 0, 0, "")
		case <-ctx.stream.Context().Done():
			return fmt.Errorf("initialization cancelled: %w", ctx.stream.Context().Err())
		}
	}
}

// registerOnAllNodes registers repository metadata on all cluster nodes
func (r *Router) registerOnAllNodes(ctx *ingestionContext) error {
	var wg sync.WaitGroup
	for _, node := range ctx.nodes {
		r.mu.Lock()
		state, ok := r.nodes[node.ID]
		if !ok {
			r.mu.Unlock()
			continue
		}

		// Ensure we have a gRPC client connection
		if state.client == nil {
			conn, err := grpc.NewClient(state.info.Address,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
			)
			if err != nil {
				r.mu.Unlock()
				r.logger.Error("failed to connect to node for registration",
					"node_id", node.ID,
					"address", state.info.Address,
					"error", err)
				continue
			}
			state.conn = conn
			state.client = pb.NewMonoFSClient(conn)
		}
		client := state.client
		nodeID := node.ID
		r.mu.Unlock()

		// Register repository on this node (parallel)
		wg.Add(1)
		go func() {
			defer wg.Done()
			regCtx, regCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := client.RegisterRepository(regCtx, &pb.RegisterRepositoryRequest{
				StorageId:       ctx.storageID,
				DisplayPath:     ctx.displayPath,
				Source:          ctx.sourceURL,
				IngestionType:   ctx.req.IngestionType,
				FetchType:       ctx.req.FetchType,
				IngestionConfig: ctx.req.IngestionConfig,
				FetchConfig:     ctx.req.FetchConfig,
				CommitHash:      ctx.req.IngestionConfig["commit_hash"],
				CommitTime:      parseCommitTime(ctx.req.IngestionConfig["commit_time"]),
				CommitMessage:   ctx.req.IngestionConfig["commit_message"],
			})
			regCancel()

			if err != nil {
				r.logger.Error("failed to register repository on node",
					"node_id", nodeID,
					"error", err)
			} else {
				r.logger.Info("registered repository on node",
					"node_id", nodeID,
					"display_path", ctx.displayPath)
			}
		}()
	}
	wg.Wait()
	return nil
}

// setupNodesAndSharding gets healthy nodes and creates sharding calculator
func (r *Router) setupNodesAndSharding(ctx *ingestionContext) error {
	r.mu.RLock()
	ctx.nodes = make([]sharding.Node, 0, len(r.nodes))
	for _, state := range r.nodes {
		if state.info.Healthy {
			ctx.nodes = append(ctx.nodes, sharding.Node{
				ID:      state.info.NodeId,
				Address: state.info.Address,
				Weight:  state.info.Weight,
				Healthy: true,
			})
		}
	}
	r.mu.RUnlock()

	if len(ctx.nodes) == 0 {
		return fmt.Errorf("no healthy nodes available")
	}

	ctx.sharder = sharding.NewHRW(ctx.nodes)

	// Pre-create gRPC connections
	ctx.nodeClients = make(map[string]nodeClient)
	r.mu.Lock()
	for _, node := range ctx.nodes {
		state, ok := r.nodes[node.ID]
		if !ok || !state.info.Healthy || state.status != NodeActive {
			continue
		}

		if state.client == nil {
			conn, err := grpc.NewClient(state.info.Address,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
			)
			if err != nil {
				r.logger.Error("failed to connect to node", "node_id", node.ID, "error", err)
				continue
			}
			state.conn = conn
			state.client = pb.NewMonoFSClient(conn)
		}

		ctx.nodeClients[node.ID] = nodeClient{
			nodeID: node.ID,
			client: state.client,
		}
	}
	r.mu.Unlock()

	if len(ctx.nodeClients) == 0 {
		return fmt.Errorf("no healthy nodes with connections available")
	}

	return nil
}

// distributeFiles walks files and distributes them to nodes using sharding
func (r *Router) distributeFiles(ctx *ingestionContext, sendProgress func(pb.IngestProgress_Stage, string, int64, int64, string)) (map[string][]*pb.FileMetadata, map[string]map[string][]*pb.FileMetadata, error) {
	replicationFactor := r.config.ReplicationFactor
	if replicationFactor < 1 {
		replicationFactor = 1
	}
	if replicationFactor > len(ctx.nodes) {
		replicationFactor = len(ctx.nodes)
	}

	primaryBatches := make(map[string][]*pb.FileMetadata)
	replicaBatches := make(map[string]map[string][]*pb.FileMetadata)

	// Get handler for canonical path extraction
	registry := NewIngestionRegistry()
	handler, err := registry.Get(ctx.req.IngestionType)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get ingestion handler: %w", err)
	}

	err = ctx.backend.WalkFiles(ctx.stream.Context(), func(meta storage.FileMetadata) error {
		atomic.AddInt64(ctx.totalFiles, 1)

		// Extract canonical path from metadata on FIRST file
		ctx.canonicalPathMux.Lock()
		if ctx.canonicalPath == "" && meta.Metadata != nil {
			if canonicalPath := handler.ExtractCanonicalPath(meta.Metadata); canonicalPath != "" {
				ctx.canonicalPath = canonicalPath

				// Update display path and storage ID immediately
				oldDisplayPath := ctx.displayPath
				oldStorageID := ctx.storageID

				ctx.displayPath = canonicalPath
				ctx.storageID = generateStorageID(canonicalPath)

				r.logger.Info("discovered canonical path from metadata",
					"original_path", oldDisplayPath,
					"canonical_path", ctx.canonicalPath,
					"old_storage_id", oldStorageID,
					"new_storage_id", ctx.storageID)

				// Move in-progress tracking from old storageID to new storageID
				r.mu.Lock()
				if progress, ok := r.inProgressIngestions[oldStorageID]; ok {
					progress.mu.Lock()
					progress.storageID = ctx.storageID
					progress.repoID = ctx.canonicalPath
					progress.mu.Unlock()
					r.inProgressIngestions[ctx.storageID] = progress
					delete(r.inProgressIngestions, oldStorageID)
				}
				r.mu.Unlock()

				// Re-register all nodes with new canonical path
				r.updateCanonicalPath(ctx)
			}
		}
		currentDisplayPath := ctx.displayPath
		currentStorageID := ctx.storageID
		ctx.canonicalPathMux.Unlock()

		// Determine target nodes using HRW sharding
		shardKey := currentStorageID + ":" + meta.Path
		targetNodes := ctx.sharder.GetNodes(shardKey, replicationFactor)
		if len(targetNodes) == 0 {
			r.logger.Warn("no node available for path", "path", meta.Path)
			return nil
		}

		primaryNode := targetNodes[0]

		// Check connection to primary node
		if _, ok := ctx.nodeClients[primaryNode.ID]; !ok {
			r.logger.Warn("skipping file - no connection to primary node", "node_id", primaryNode.ID, "path", meta.Path)
			return nil
		}

		// Merge backend metadata
		backendMeta := meta.Metadata
		if backendMeta == nil {
			backendMeta = make(map[string]string)
		}

		fileMeta := &pb.FileMetadata{
			Path:            meta.Path,
			Ref:             ctx.ref,
			Size:            meta.Size,
			Mtime:           meta.ModTime,
			Mode:            meta.Mode,
			BlobHash:        meta.ContentHash,
			Source:          ctx.sourceURL,
			StorageId:       currentStorageID,
			DisplayPath:     currentDisplayPath,
			SourceType:      ctx.req.IngestionType,
			FetchType:       ctx.req.FetchType,
			BackendMetadata: backendMeta,
		}

		// Add to primary batch
		primaryBatches[primaryNode.ID] = append(primaryBatches[primaryNode.ID], fileMeta)

		// Add to replica batches
		for i := 1; i < len(targetNodes); i++ {
			replicaNode := targetNodes[i]
			if _, ok := ctx.nodeClients[replicaNode.ID]; !ok {
				continue
			}

			if replicaBatches[replicaNode.ID] == nil {
				replicaBatches[replicaNode.ID] = make(map[string][]*pb.FileMetadata)
			}
			replicaBatches[replicaNode.ID][primaryNode.ID] = append(
				replicaBatches[replicaNode.ID][primaryNode.ID], fileMeta)
		}

		// Send progress periodically
		if atomic.LoadInt64(ctx.totalFiles)%1000 == 0 {
			count := atomic.LoadInt64(ctx.totalFiles)
			sendProgress(pb.IngestProgress_INGESTING,
				fmt.Sprintf("Scanning files... %d found", count),
				0, count, meta.Path)
		}

		// Collect file info for layout generation
		ctx.collectedFiles = append(ctx.collectedFiles, buildlayout.FileInfo{
			Path:            meta.Path,
			BlobHash:        meta.ContentHash,
			Size:            meta.Size,
			Mode:            meta.Mode,
			Mtime:           meta.ModTime,
			Source:          ctx.sourceURL,
			BackendMetadata: meta.Metadata,
		})

		return nil
	})

	if err != nil {
		return nil, nil, fmt.Errorf("failed to walk files: %w", err)
	}

	// No need to call updateCanonicalPath here - already done during first file
	return primaryBatches, replicaBatches, nil
}

// updateCanonicalPath updates repository registration with canonical path
func (r *Router) updateCanonicalPath(ctx *ingestionContext) {
	r.logger.Info("updating repository registrations with canonical path",
		"original_path", ctx.displayPath,
		"canonical_path", ctx.canonicalPath)

	type nodeUpdate struct {
		nodeID string
		client pb.MonoFSClient
	}
	var updates []nodeUpdate

	for _, node := range ctx.nodes {
		r.mu.Lock()
		state, ok := r.nodes[node.ID]
		if !ok || state.client == nil {
			r.mu.Unlock()
			continue
		}
		updates = append(updates, nodeUpdate{nodeID: node.ID, client: state.client})
		r.mu.Unlock()
	}

	var wg sync.WaitGroup
	for _, u := range updates {
		u := u
		wg.Add(1)
		go func() {
			defer wg.Done()
			regCtx, regCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := u.client.RegisterRepository(regCtx, &pb.RegisterRepositoryRequest{
				StorageId:       ctx.storageID,
				DisplayPath:     ctx.canonicalPath,
				Source:          ctx.sourceURL,
				IngestionType:   ctx.req.IngestionType,
				FetchType:       ctx.req.FetchType,
				IngestionConfig: ctx.req.IngestionConfig,
				FetchConfig:     ctx.req.FetchConfig,
			})
			regCancel()

			if err != nil {
				r.logger.Warn("failed to update repository registration with canonical path",
					"node_id", u.nodeID,
					"error", err)
			}
		}()
	}
	wg.Wait()
}

// ingestPrimaryBatches sends primary file batches to nodes
func (r *Router) ingestPrimaryBatches(ctx *ingestionContext, primaryBatches map[string][]*pb.FileMetadata, sendProgress func(pb.IngestProgress_Stage, string, int64, int64, string)) error {
	const batchSize = 1000
	const maxConcurrentBatches = 10

	type batchJob struct {
		nodeID string
		batch  []*pb.FileMetadata
	}

	// Calculate total batches
	totalPrimaryBatches := 0
	for _, files := range primaryBatches {
		totalPrimaryBatches += (len(files) + batchSize - 1) / batchSize
	}

	batchChan := make(chan batchJob, 100)
	resultChan := make(chan error, totalPrimaryBatches)
	doneChan := make(chan struct{})

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < maxConcurrentBatches; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range batchChan {
				client, ok := ctx.nodeClients[job.nodeID]
				if !ok {
					resultChan <- fmt.Errorf("node %s not found", job.nodeID)
					continue
				}

				batchCtx, batchCancel := context.WithTimeout(context.Background(), 60*time.Second)
				resp, err := client.client.IngestFileBatch(batchCtx, &pb.IngestFileBatchRequest{
					Files:       job.batch,
					StorageId:   ctx.storageID,
					DisplayPath: ctx.displayPath,
					Source:      ctx.sourceURL,
					Ref:         ctx.ref,
				})
				batchCancel()

				if err != nil {
					r.logger.Error("primary batch ingestion failed",
						"node_id", job.nodeID,
						"batch_size", len(job.batch),
						"error", err)
					resultChan <- err
					continue
				}

				if !resp.Success {
					r.logger.Error("primary batch ingestion returned failure",
						"node_id", job.nodeID,
						"files_ingested", resp.FilesIngested,
						"files_failed", resp.FilesFailed,
						"error", resp.ErrorMessage)
				}

				atomic.AddInt64(ctx.filesIngested, resp.FilesIngested)
				resultChan <- nil
			}
		}()
	}

	// Progress reporter
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-doneChan:
				return
			case <-ticker.C:
				count := atomic.LoadInt64(ctx.filesIngested)
				total := atomic.LoadInt64(ctx.totalFiles)
				sendProgress(pb.IngestProgress_INGESTING,
					fmt.Sprintf("Ingesting files... %d/%d", count, total),
					count, total, "")
			}
		}
	}()

	// Queue batches
	batchesQueued := 0
	for nodeID, files := range primaryBatches {
		for i := 0; i < len(files); i += batchSize {
			end := i + batchSize
			if end > len(files) {
				end = len(files)
			}
			batchChan <- batchJob{nodeID: nodeID, batch: files[i:end]}
			batchesQueued++
		}
	}
	close(batchChan)

	// Wait for completion
	wg.Wait()
	close(doneChan)

	// Collect results
	var firstError error
	errorCount := 0
	close(resultChan)
	for err := range resultChan {
		if err != nil {
			errorCount++
			if firstError == nil {
				firstError = err
			}
		}
	}

	if firstError != nil && errorCount == batchesQueued {
		return fmt.Errorf("all batches failed: %w", firstError)
	}

	if errorCount > 0 {
		r.logger.Warn("some primary batches failed",
			"failed_batches", errorCount,
			"total_batches", batchesQueued)
	}

	return nil
}

// ingestReplicaBatches sends replica file batches to nodes (async, best-effort)
func (r *Router) ingestReplicaBatches(ctx *ingestionContext, replicaBatches map[string]map[string][]*pb.FileMetadata) {
	replicationFactor := r.config.ReplicationFactor
	if replicationFactor <= 1 || len(replicaBatches) == 0 {
		return
	}

	const batchSize = 1000
	var replicasIngested, replicaErrors int64
	var replicaWg sync.WaitGroup

	for replicaNodeID, primaryMap := range replicaBatches {
		client, ok := ctx.nodeClients[replicaNodeID]
		if !ok {
			continue
		}

		for primaryNodeID, files := range primaryMap {
			for i := 0; i < len(files); i += batchSize {
				end := i + batchSize
				if end > len(files) {
					end = len(files)
				}
				batch := files[i:end]

				replicaWg.Add(1)
				go func(nodeID, primaryID string, fileBatch []*pb.FileMetadata, c nodeClient) {
					defer replicaWg.Done()

					replCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer cancel()

					resp, err := c.client.IngestReplicaBatch(replCtx, &pb.IngestReplicaBatchRequest{
						Files:         fileBatch,
						StorageId:     ctx.storageID,
						DisplayPath:   ctx.displayPath,
						PrimaryNodeId: primaryID,
						Source:        ctx.sourceURL,
						Ref:           ctx.ref,
					})

					if err != nil {
						r.logger.Warn("replica batch failed",
							"replica_node", nodeID,
							"primary_node", primaryID,
							"error", err)
						atomic.AddInt64(&replicaErrors, 1)
						return
					}

					atomic.AddInt64(&replicasIngested, resp.FilesReplicated)
				}(replicaNodeID, primaryNodeID, batch, client)
			}
		}
	}

	replicaWg.Wait()
	r.logger.Info("replica ingestion complete",
		"files_replicated", replicasIngested,
		"errors", replicaErrors)
}

// performPostIngestionTasks handles indexing, layouts, and marking repo as onboarded
func (r *Router) performPostIngestionTasks(ctx *ingestionContext, sendProgress func(pb.IngestProgress_Stage, string, int64, int64, string)) {
	// Build directory indexes
	sendProgress(pb.IngestProgress_INGESTING, "Building directory indexes...",
		atomic.LoadInt64(ctx.filesIngested), atomic.LoadInt64(ctx.filesIngested), "")

	r.mu.RLock()
	indexingNodes := make([]*nodeState, 0, len(r.nodes))
	for _, state := range r.nodes {
		if state.info.Healthy && state.status == NodeActive && state.client != nil {
			indexingNodes = append(indexingNodes, state)
		}
	}
	r.mu.RUnlock()

	// Build directory indexes in parallel across all nodes
	var indexWg sync.WaitGroup
	for _, state := range indexingNodes {
		state := state
		indexWg.Add(1)
		go func() {
			defer indexWg.Done()
			indexCtx, indexCancel := context.WithTimeout(context.Background(), 60*time.Second)
			resp, err := state.client.BuildDirectoryIndexes(indexCtx, &pb.BuildDirectoryIndexesRequest{
				StorageId: ctx.storageID,
			})
			indexCancel()

			if err != nil {
				r.logger.Error("failed to build directory indexes on node",
					"node_id", state.info.NodeId, "error", err)
			} else {
				r.logger.Info("directory indexes built on node",
					"node_id", state.info.NodeId,
					"directories_indexed", resp.DirectoriesIndexed)
			}
		}()
	}
	indexWg.Wait()

	// Mark repository as onboarded
	r.mu.RLock()
	activeNodes := make([]*nodeState, 0, len(r.nodes))
	for _, state := range r.nodes {
		if state.info.Healthy && state.status == NodeActive && state.client != nil {
			activeNodes = append(activeNodes, state)
		}
	}
	r.mu.RUnlock()

	// Mark onboarded in parallel across all nodes
	var markWg sync.WaitGroup
	for _, state := range activeNodes {
		state := state
		markWg.Add(1)
		go func() {
			defer markWg.Done()
			markCtx, markCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := state.client.MarkRepositoryOnboarded(markCtx, &pb.MarkRepositoryOnboardedRequest{
				StorageId: ctx.storageID,
			})
			markCancel()

			if err != nil {
				r.logger.Warn("failed to mark repository onboarded on node",
					"node_id", state.info.NodeId, "error", err)
			}
		}()
	}
	markWg.Wait()

	// Track ingested repository
	r.mu.Lock()
	_, isReingestion := r.ingestedRepos[ctx.storageID]
	r.ingestedRepos[ctx.storageID] = &ingestedRepo{
		repoID:            ctx.displayPath,
		repoURL:           ctx.sourceURL,
		branch:            ctx.ref,
		filesCount:        atomic.LoadInt64(ctx.filesIngested),
		ingestedAt:        time.Now(),
		topologyVersion:   r.version.Load(),
		targetTopology:    0,
		rebalanceState:    RebalanceStateStable,
		rebalanceProgress: 1.0,
	}
	r.mu.Unlock()

	// Trigger rebalancing if re-ingestion
	if isReingestion {
		r.logger.Info("detected repository re-ingestion, triggering cluster rebalancing",
			"storage_id", ctx.storageID)
		go func() {
			r.rebalanceSem <- struct{}{}
			defer func() { <-r.rebalanceSem }()
			r.rebalanceRepository(ctx.storageID)
		}()
	}

	// Trigger search indexing
	if r.searchClient != nil {
		go func() {
			searchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			resp, err := r.searchClient.IndexRepository(searchCtx, &pb.IndexRequest{
				StorageId:   ctx.storageID,
				DisplayPath: ctx.displayPath,
				Source:      ctx.sourceURL,
				Ref:         ctx.ref,
			})
			if err != nil {
				r.logger.Warn("failed to trigger search indexing", "error", err)
			} else if resp.Queued {
				r.logger.Info("search indexing queued", "job_id", resp.JobId)
			}
		}()
	}

	// Generate build layouts
	if r.layoutRegistry != nil && r.layoutRegistry.HasMappers() {
		layoutInfo := buildlayout.RepoInfo{
			DisplayPath:   ctx.displayPath,
			StorageID:     ctx.storageID,
			Source:        ctx.sourceURL,
			Ref:           ctx.ref,
			IngestionType: string(ctx.ingestionType),
			FetchType:     ctx.fetchType.String(),
			Config:        ctx.req.IngestionConfig,
		}

		layoutProgress := func(stage pb.IngestProgress_Stage, msg string) {
			if ctx.stream != nil {
				ctx.stream.Send(&pb.IngestProgress{
					Stage:   stage,
					Message: msg,
				})
			}
		}

		layoutCtx := context.Background()
		if ctx.stream != nil {
			layoutCtx = ctx.stream.Context()
		}
		r.generateLayouts(layoutCtx, layoutInfo, ctx.collectedFiles, layoutProgress)
	}
}

// IngestRepository implements the IngestRepository RPC with streaming progress.
func (r *Router) IngestRepository(req *pb.IngestRequest, stream pb.MonoFSRouter_IngestRepositoryServer) error {
	// Initialize ingestion context
	var filesIngested, totalFiles int64
	ctx := &ingestionContext{
		req:           req,
		stream:        stream,
		filesIngested: &filesIngested,
		totalFiles:    &totalFiles,
	}

	// Step 1: Validate and setup
	if err := r.validateAndSetup(ctx); err != nil {
		return err
	}

	r.logger.Info("ingesting source",
		"source", req.Source,
		"ref", ctx.ref,
		"source_url", ctx.sourceURL,
		"display_path", ctx.displayPath,
		"ingestion_type", ctx.ingestionType,
		"fetch_type", ctx.fetchType)

	// Check for already in-progress or already completed ingestion.
	// We check by storageID first, then also scan by source URL because
	// for npm/cargo/maven packages the initial storageID (hash of source
	// string like "prettier@3.1.1") differs from the canonical storageID
	// (hash of discovered path like "github.com/prettier/prettier").
	r.mu.Lock()
	if existing, ok := r.inProgressIngestions[ctx.storageID]; ok {
		r.mu.Unlock()
		return fmt.Errorf("ingestion already in progress for %s (started %s)",
			existing.repoID, existing.startedAt.Format(time.RFC3339))
	}
	for _, existing := range r.inProgressIngestions {
		if existing.repoURL == ctx.sourceURL {
			r.mu.Unlock()
			return fmt.Errorf("ingestion already in progress for %s (started %s)",
				existing.repoID, existing.startedAt.Format(time.RFC3339))
		}
	}
	if existing, ok := r.ingestedRepos[ctx.storageID]; ok {
		r.mu.Unlock()
		return fmt.Errorf("source already ingested as %s (ingested %s)",
			existing.repoID, existing.ingestedAt.Format(time.RFC3339))
	}
	for _, existing := range r.ingestedRepos {
		if existing.repoURL == ctx.sourceURL {
			r.mu.Unlock()
			return fmt.Errorf("source already ingested as %s (ingested %s)",
				existing.repoID, existing.ingestedAt.Format(time.RFC3339))
		}
	}

	// Register in-progress ingestion
	ctx.initialStorageID = ctx.storageID
	r.inProgressIngestions[ctx.storageID] = &inProgressIngestion{
		storageID: ctx.storageID,
		repoID:    ctx.displayPath,
		repoURL:   ctx.sourceURL,
		branch:    ctx.ref,
		startedAt: time.Now(),
		stage:     pb.IngestProgress_CLONING,
		message:   "Starting ingestion...",
	}
	r.mu.Unlock()

	// Ensure cleanup on exit
	defer func() {
		r.mu.Lock()
		// Always clean up the initial storageID entry (before canonical path changed it)
		if ctx.initialStorageID != ctx.storageID {
			delete(r.inProgressIngestions, ctx.initialStorageID)
		}
		progress, exists := r.inProgressIngestions[ctx.storageID]
		if exists && progress.stage == pb.IngestProgress_FAILED {
			r.mu.Unlock()
			time.AfterFunc(30*time.Second, func() {
				r.mu.Lock()
				delete(r.inProgressIngestions, ctx.storageID)
				r.mu.Unlock()
			})
		} else {
			delete(r.inProgressIngestions, ctx.storageID)
			r.mu.Unlock()
		}
		if ctx.backend != nil {
			ctx.backend.Cleanup()
		}
	}()

	// Helper to send progress updates
	sendProgress := func(stage pb.IngestProgress_Stage, message string, filesProcessed, totalFiles int64, currentFile string) {
		if r.mu.TryLock() {
			if progress, ok := r.inProgressIngestions[ctx.storageID]; ok {
				progress.mu.Lock()
				progress.stage = stage
				progress.message = message
				progress.filesProcessed = filesProcessed
				progress.totalFiles = totalFiles
				progress.mu.Unlock()
			}
			r.mu.Unlock()
		}
		stream.Send(&pb.IngestProgress{
			Stage:          stage,
			Message:        message,
			FilesProcessed: filesProcessed,
			TotalFiles:     totalFiles,
			CurrentFile:    currentFile,
			Success:        stage == pb.IngestProgress_COMPLETED,
		})
	}

	// Step 2: Initialize backend
	if err := r.initializeBackend(ctx, sendProgress); err != nil {
		sendProgress(pb.IngestProgress_FAILED, err.Error(), 0, 0, "")
		return err
	}

	// Step 3: Setup nodes and sharding
	sendProgress(pb.IngestProgress_REGISTERING, "Preparing cluster nodes...", 0, 0, "")
	if err := r.setupNodesAndSharding(ctx); err != nil {
		sendProgress(pb.IngestProgress_FAILED, err.Error(), 0, 0, "")
		return err
	}

	// Step 4: Register on all nodes (skip if display path not yet known —
	// npm/cargo/maven discover canonical path during file scanning and
	// register via updateCanonicalPath at that point)
	if ctx.displayPath != "" {
		sendProgress(pb.IngestProgress_REGISTERING, "Registering repository on nodes...", 0, 0, "")
		r.logger.Info("registering repository on all nodes",
			"display_path", ctx.displayPath,
			"storage_id", ctx.storageID,
			"node_count", len(ctx.nodes))

		if err := r.registerOnAllNodes(ctx); err != nil {
			sendProgress(pb.IngestProgress_FAILED, err.Error(), 0, 0, "")
			return err
		}
	} else {
		sendProgress(pb.IngestProgress_REGISTERING, "Preparing ingestion (display path pending)...", 0, 0, "")
		r.logger.Info("skipping initial registration — display path will be discovered from package metadata",
			"source", ctx.sourceURL)
	}

	// Step 5: Distribute files to nodes
	sendProgress(pb.IngestProgress_INGESTING, "Scanning files...", 0, 0, "")
	primaryBatches, replicaBatches, err := r.distributeFiles(ctx, sendProgress)
	if err != nil {
		sendProgress(pb.IngestProgress_FAILED, err.Error(), 0, 0, "")
		return err
	}

	// Step 6: Ingest primary batches
	sendProgress(pb.IngestProgress_INGESTING, "Ingesting files to primary nodes...", 0, atomic.LoadInt64(ctx.totalFiles), "")
	if err := r.ingestPrimaryBatches(ctx, primaryBatches, sendProgress); err != nil {
		sendProgress(pb.IngestProgress_FAILED, err.Error(), atomic.LoadInt64(ctx.filesIngested), atomic.LoadInt64(ctx.totalFiles), "")
		return err
	}

	// Step 7: Ingest replica batches (best-effort)
	if r.config.ReplicationFactor > 1 {
		sendProgress(pb.IngestProgress_INGESTING, "Replicating to backup nodes...",
			atomic.LoadInt64(ctx.filesIngested), atomic.LoadInt64(ctx.totalFiles), "")
		r.ingestReplicaBatches(ctx, replicaBatches)
	}

	// Step 8: Post-ingestion tasks
	r.performPostIngestionTasks(ctx, sendProgress)

	filesIngestedFinal := atomic.LoadInt64(ctx.filesIngested)
	sendProgress(pb.IngestProgress_COMPLETED,
		fmt.Sprintf("Repository ingested successfully: %d files", filesIngestedFinal),
		filesIngestedFinal, filesIngestedFinal, "")
	return nil
}

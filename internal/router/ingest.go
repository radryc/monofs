// Package router provides ingestion logic for MonoFS.
package router

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/radryc/packager"
	"github.com/radryc/packager/pipeline"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/sharding"
	"github.com/radryc/monofs/internal/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// normalizeRepoID converts a repository URL to a normalized path format
// Examples:
//   - https://github.com/owner/repo -> github.com/owner/repo
//   - github.com/google/uuid@v1.3.0 -> github.com/google/uuid@v1.3.0
func normalizeRepoID(repoURL string) string {
	// For Go modules with @version, keep as-is
	if strings.Contains(repoURL, "@") {
		return repoURL
	}

	// Try to parse as URL
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

// IngestRepository implements the IngestRepository RPC with streaming progress.
func (r *Router) IngestRepository(req *pb.IngestRequest, stream pb.MonoFSRouter_IngestRepositoryServer) error {
	// Enforce ingestion whitelist
	if r.whitelist.Enabled() {
		clientID := extractClientID(stream.Context())
		if !r.whitelist.IsAllowed(clientID) {
			r.logger.Warn("ingestion denied by whitelist",
				"client_id", clientID,
				"source", req.Source)
			return fmt.Errorf("client %q is not whitelisted for ingestion", clientID)
		}
	}

	// Validate source
	if req.Source == "" {
		return fmt.Errorf("source must be specified")
	}

	// Guardian partition validation
	var guardianURL string
	if req.IngestionType == pb.IngestionType_INGESTION_GUARDIAN {
		if req.SourceId == "" {
			return fmt.Errorf("source_id (partition name) is required for guardian ingestion")
		}
		if strings.Contains(req.SourceId, "/") {
			return fmt.Errorf("guardian partition name must not contain '/'")
		}
		token := req.IngestionConfig["guardian_token"]
		if token == "" {
			return fmt.Errorf("guardian_token is required in ingestion_config for guardian ingestion")
		}
		clientID, ok := r.validateGuardianToken(token)
		if !ok {
			return fmt.Errorf("invalid guardian token: no matching connected guardian client")
		}
		baseURL := r.getGuardianBaseURL(clientID)
		if strings.TrimSpace(baseURL) != "" {
			guardianURL = strings.TrimRight(baseURL, "/")
		}
	}

	// Step 1: Determine display path (what users see in filesystem)
	// If req.SourceId is set: use custom path (e.g., "my/custom/path" or "myrepo")
	// If req.SourceId is empty: auto-generate from source (e.g., "github_com/owner/repo")
	displayPath := req.SourceId
	if displayPath == "" {
		displayPath = normalizeRepoID(req.Source)
	}
	// Guardian partitions always live under guardian/ prefix
	if req.IngestionType == pb.IngestionType_INGESTION_GUARDIAN {
		displayPath = "guardian/" + req.SourceId
	}

	// Step 2: Generate internal storage ID (SHA-256 hash)
	storageID := sharding.GenerateStorageID(displayPath)

	// Determine backend types
	ingestionType := storage.IngestionType(strings.ToLower(req.IngestionType.String()))
	if ingestionType == "" || ingestionType == "ingestion_git" {
		ingestionType = storage.IngestionTypeGit
	} else if ingestionType == "ingestion_s3" {
		ingestionType = storage.IngestionTypeS3
	} else if ingestionType == "ingestion_guardian" {
		// Guardian uses standard git backend for ingestion
		ingestionType = storage.IngestionTypeGit
	}

	// All ingestion now produces blob archives by default
	fetchType := storage.FetchTypeBlob

	// Handle ref with type-specific defaults and validation
	ref := req.Ref
	sourceURL := req.Source

	switch req.IngestionType {
	case pb.IngestionType_INGESTION_GIT:
		// Git: ref is optional, defaults to "main"
		if ref == "" {
			ref = "main"
		}
	case pb.IngestionType_INGESTION_S3:
		// S3: ref is optional and used as prefix
	}

	r.logger.Info("ingesting source",
		"source", req.Source,
		"ref", ref,
		"source_url", sourceURL,
		"display_path", displayPath,
		"ingestion_type", ingestionType,
		"fetch_type", fetchType)

	// Register in-progress ingestion
	r.mu.Lock()
	r.inProgressIngestions[storageID] = &inProgressIngestion{
		storageID: storageID,
		repoID:    displayPath,
		repoURL:   sourceURL,
		branch:    ref,
		startedAt: time.Now(),
		stage:     pb.IngestProgress_CLONING,
		message:   "Starting ingestion...",
	}
	r.mu.Unlock()

	// Ensure cleanup on exit (with delay for failed ingestions so UI can see the error)
	defer func() {
		// Check if this was a failure by looking at the final stage
		r.mu.Lock()
		progress, exists := r.inProgressIngestions[storageID]
		if exists && progress.stage == pb.IngestProgress_FAILED {
			// Keep failed ingestions visible for 30 seconds before cleanup
			r.mu.Unlock()
			time.AfterFunc(30*time.Second, func() {
				r.mu.Lock()
				delete(r.inProgressIngestions, storageID)
				r.mu.Unlock()
				r.logger.Info("cleaned up failed ingestion", "storage_id", storageID)
			})
		} else {
			// Successful or cancelled - remove immediately
			delete(r.inProgressIngestions, storageID)
			r.mu.Unlock()
		}
	}()

	// Helper to send progress updates and update in-memory state
	sendProgress := func(stage pb.IngestProgress_Stage, message string, filesProcessed, totalFiles int64, currentFile string) {
		// Update in-memory state (non-blocking - use TryLock to avoid blocking ingestion)
		if r.mu.TryLock() {
			if progress, ok := r.inProgressIngestions[storageID]; ok {
				progress.mu.Lock()
				progress.stage = stage
				progress.message = message
				progress.filesProcessed = filesProcessed
				progress.totalFiles = totalFiles
				progress.mu.Unlock()
			}
			r.mu.Unlock()
		}
		// If lock not acquired, skip UI update - ingestion performance is more important

		// Send to stream
		stream.Send(&pb.IngestProgress{
			Stage:          stage,
			Message:        message,
			FilesProcessed: filesProcessed,
			TotalFiles:     totalFiles,
			CurrentFile:    currentFile,
			Success:        stage == pb.IngestProgress_COMPLETED,
		})
	}

	sendProgress(pb.IngestProgress_CLONING, "Initializing backend...", 0, 0, "")

	r.logger.Info("ingesting repository",
		"url", sourceURL,
		"branch", ref,
		"display_path", displayPath,
		"storage_id", storageID,
		"ingestion_type", ingestionType,
		"fetch_type", fetchType)

	// Create ingestion backend
	backend, err := storage.DefaultRegistry.CreateIngestionBackend(ingestionType)
	if err != nil {
		sendProgress(pb.IngestProgress_FAILED, fmt.Sprintf("unsupported ingestion type: %v", err), 0, 0, "")
		return fmt.Errorf("unsupported ingestion type: %w", err)
	}
	defer backend.Cleanup()

	// Prepare backend config
	config := req.IngestionConfig
	if config == nil {
		config = make(map[string]string)
	}
	config["branch"] = ref
	config["repo_id"] = displayPath

	// Validate source
	sendProgress(pb.IngestProgress_CLONING, "Validating source...", 0, 0, "")
	if err := backend.Validate(stream.Context(), sourceURL, config); err != nil {
		sendProgress(pb.IngestProgress_FAILED, fmt.Sprintf("validation failed: %v", err), 0, 0, "")
		return fmt.Errorf("validation failed: %w", err)
	}

	// Initialize backend with progress keepalives (prevents stream timeout during long operations)
	initDone := make(chan struct{})
	initErr := make(chan error, 1)

	go func() {
		err := backend.Initialize(stream.Context(), sourceURL, config)
		if err != nil {
			initErr <- err
		} else {
			close(initDone)
		}
	}()

	// Send keepalive progress updates every 10 seconds during initialization
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	initStartTime := time.Now()

	for {
		select {
		case <-initDone:
			// Initialization completed successfully
			elapsed := time.Since(initStartTime).Round(time.Second)
			sendProgress(pb.IngestProgress_CLONING, fmt.Sprintf("Backend ready in %v", elapsed), 0, 0, "")
			goto initComplete
		case err := <-initErr:
			sendProgress(pb.IngestProgress_FAILED, fmt.Sprintf("failed to initialize backend: %v", err), 0, 0, "")
			return fmt.Errorf("failed to initialize backend: %w", err)
		case <-ticker.C:
			elapsed := time.Since(initStartTime).Round(time.Second)
			sendProgress(pb.IngestProgress_CLONING, fmt.Sprintf("Initializing backend... (%v elapsed)", elapsed), 0, 0, "")
		case <-stream.Context().Done():
			return fmt.Errorf("initialization cancelled: %w", stream.Context().Err())
		}
	}

initComplete:
	// Extract commit info from backend config (Git backend populates this during Initialize)
	commitHash := config["commit_hash"]
	commitMessage := config["commit_message"]
	var commitTime int64
	if ct, ok := config["commit_time"]; ok {
		if t, err := time.Parse(time.RFC3339, ct); err == nil {
			commitTime = t.Unix()
		}
	}

	sendProgress(pb.IngestProgress_REGISTERING, "Registering repository on nodes...", 0, 0, "")

	// Get cluster nodes for sharding
	r.mu.RLock()
	nodes := make([]sharding.Node, 0, len(r.nodes))
	for _, state := range r.nodes {
		if state.info.Healthy {
			nodes = append(nodes, sharding.Node{
				ID:      state.info.NodeId,
				Address: state.info.Address,
				Weight:  state.info.Weight,
				Healthy: true, // Mark as healthy since we filtered above
			})
		}
	}
	r.mu.RUnlock()

	if len(nodes) == 0 {
		sendProgress(pb.IngestProgress_FAILED, "no healthy nodes available", 0, 0, "")
		return fmt.Errorf("no healthy nodes available")
	}

	// Create sharding calculator
	sharder := sharding.NewHRW(nodes)

	// STEP 1: Register repository metadata on ALL nodes
	// This ensures every node knows about the repo and can resolve paths
	r.logger.Info("registering repository on all nodes",
		"display_path", displayPath,
		"storage_id", storageID,
		"node_count", len(nodes))

	for _, node := range nodes {
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
		r.mu.Unlock()

		// Register repository on this node
		regCtx, regCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := client.RegisterRepository(regCtx, &pb.RegisterRepositoryRequest{
			StorageId:       storageID,
			DisplayPath:     displayPath,
			Source:          sourceURL,
			IngestionType:   req.IngestionType,
			FetchType:       req.FetchType,
			IngestionConfig: req.IngestionConfig,
			FetchConfig:     req.FetchConfig,
			CommitHash:      commitHash,
			CommitTime:      commitTime,
			CommitMessage:   commitMessage,
			GuardianUrl:     guardianURL,
		})
		regCancel()

		if err != nil {
			r.logger.Error("failed to register repository on node",
				"node_id", node.ID,
				"error", err)
			// Don't fail ingestion if registration fails on one node
			// Files can still be ingested to other nodes
		} else {
			r.logger.Info("registered repository on node",
				"node_id", node.ID,
				"display_path", displayPath)
		}
	}

	// STEP 2: Distribute files to nodes using HRW sharding with replication
	sendProgress(pb.IngestProgress_INGESTING, "Ingesting files...", 0, 0, "")

	var filesIngested int64
	const batchSize = 1000          // Files per batch (optimal for transaction size)
	const maxConcurrentBatches = 10 // Parallel batch operations

	// Get replication factor from router config
	replicationFactor := r.config.ReplicationFactor
	if replicationFactor < 1 {
		replicationFactor = 1 // At least primary
	}
	if replicationFactor > len(nodes) {
		replicationFactor = len(nodes) // Can't have more replicas than nodes
	}

	r.logger.Info("ingestion replication config",
		"replication_factor", replicationFactor,
		"healthy_nodes", len(nodes))

	// Pre-create gRPC connections for all healthy nodes (connection pooling)
	type nodeClient struct {
		nodeID string
		client pb.MonoFSClient
	}
	nodeClients := make(map[string]nodeClient)

	r.mu.Lock()
	for _, node := range nodes {
		state, ok := r.nodes[node.ID]
		if !ok || !state.info.Healthy || state.status != NodeActive {
			continue
		}

		// Ensure connection exists
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

		nodeClients[node.ID] = nodeClient{
			nodeID: node.ID,
			client: state.client,
		}
	}
	r.mu.Unlock()

	if len(nodeClients) == 0 {
		sendProgress(pb.IngestProgress_FAILED, "no healthy nodes with connections available", 0, 0, "")
		return fmt.Errorf("no healthy nodes available")
	}
	// Batch files by target node (primary) and replicas
	primaryBatches := make(map[string][]*pb.FileMetadata)            // primaryNodeID -> files
	replicaBatches := make(map[string]map[string][]*pb.FileMetadata) // replicaNodeID -> primaryNodeID -> files

	// Collect file content for archive building
	type archiveFile struct {
		path    string
		hash    string
		content []byte
	}
	var collectedFiles []archiveFile
	var collectedSize int64

	// Walk files using backend and group by target nodes
	var totalFiles int64
	err = backend.WalkFiles(stream.Context(), func(meta storage.FileMetadata) error {
		atomic.AddInt64(&totalFiles, 1)

		// Collect content for archive building
		if len(meta.Content) > 0 {
			collectedFiles = append(collectedFiles, archiveFile{
				path:    meta.Path,
				hash:    meta.ContentHash,
				content: meta.Content,
			})
			collectedSize += int64(len(meta.Content))
		}

		// Determine target nodes using HRW sharding with replication
		// GetNodes returns top N nodes sorted by HRW score
		shardKey := storageID + ":" + meta.Path
		targetNodes := sharder.GetNodes(shardKey, replicationFactor)
		if len(targetNodes) == 0 {
			r.logger.Warn("no node available for path", "path", meta.Path)
			return nil
		}

		// First node is primary, rest are replicas
		primaryNode := targetNodes[0]

		// Debug logging for first 5 files
		if atomic.LoadInt64(&totalFiles) <= 5 {
			replicaIDs := make([]string, 0, len(targetNodes)-1)
			for i := 1; i < len(targetNodes); i++ {
				replicaIDs = append(replicaIDs, targetNodes[i].ID)
			}
			r.logger.Info("file sharding with replication",
				"file_num", totalFiles,
				"path", meta.Path,
				"shard_key", shardKey,
				"primary_node", primaryNode.ID,
				"replica_nodes", replicaIDs)
		}

		// Check if we have a connection to primary node
		if _, ok := nodeClients[primaryNode.ID]; !ok {
			r.logger.Warn("skipping file - no connection to primary node", "node_id", primaryNode.ID, "path", meta.Path)
			return nil
		}

		// Merge backend metadata with standard metadata
		backendMeta := meta.Metadata
		if backendMeta == nil {
			backendMeta = make(map[string]string)
		}

		fileMeta := &pb.FileMetadata{
			Path:            meta.Path,
			Ref:             ref,
			Size:            meta.Size,
			Mtime:           meta.ModTime,
			Mode:            meta.Mode,
			BlobHash:        meta.ContentHash,
			Source:          sourceURL,
			StorageId:       storageID,
			DisplayPath:     displayPath,
			SourceType:      req.IngestionType,
			FetchType:       req.FetchType,
			BackendMetadata: backendMeta,
		}

		// Add to primary batch
		primaryBatches[primaryNode.ID] = append(primaryBatches[primaryNode.ID], fileMeta)

		// Add to replica batches (for nodes 1..N-1)
		for i := 1; i < len(targetNodes); i++ {
			replicaNode := targetNodes[i]
			if _, ok := nodeClients[replicaNode.ID]; !ok {
				continue // Skip if no connection to replica
			}

			if replicaBatches[replicaNode.ID] == nil {
				replicaBatches[replicaNode.ID] = make(map[string][]*pb.FileMetadata)
			}
			// Create a clone for replica to avoid race conditions during gRPC serialization
			replicaMeta := &pb.FileMetadata{
				Path:            fileMeta.Path,
				Ref:             fileMeta.Ref,
				Size:            fileMeta.Size,
				Mtime:           fileMeta.Mtime,
				Mode:            fileMeta.Mode,
				BlobHash:        fileMeta.BlobHash,
				Source:          fileMeta.Source,
				StorageId:       fileMeta.StorageId,
				DisplayPath:     fileMeta.DisplayPath,
				SourceType:      fileMeta.SourceType,
				FetchType:       fileMeta.FetchType,
				BackendMetadata: fileMeta.BackendMetadata,
			}
			replicaBatches[replicaNode.ID][primaryNode.ID] = append(
				replicaBatches[replicaNode.ID][primaryNode.ID], replicaMeta)
		}

		// Send progress every 1000 files
		if atomic.LoadInt64(&totalFiles)%1000 == 0 {
			count := atomic.LoadInt64(&totalFiles)
			sendProgress(pb.IngestProgress_INGESTING,
				fmt.Sprintf("Scanning files... %d found", count),
				0, count, meta.Path)
		}

		return nil
	})

	if err != nil {
		sendProgress(pb.IngestProgress_FAILED, fmt.Sprintf("failed to walk files: %v", err), 0, 0, "")
		return fmt.Errorf("failed to walk files: %w", err)
	}

	// ===== ARCHIVE BUILDING PHASE =====
	// Build packager archives from collected file content and stream to fetcher nodes.
	// Archives are split at 2GB boundaries.
	// Files are stored using content hash as key (content-addressed) so that
	// fetcher blob lookups by hash work correctly.
	if r.fetcherClient != nil && len(collectedFiles) > 0 {
		// Deduplicate by content hash — same blob should only appear once in archives.
		seenHashes := make(map[string]bool, len(collectedFiles))
		dedupedFiles := make([]archiveFile, 0, len(collectedFiles))
		for _, f := range collectedFiles {
			if f.hash == "" {
				continue
			}
			if seenHashes[f.hash] {
				continue
			}
			seenHashes[f.hash] = true
			dedupedFiles = append(dedupedFiles, f)
		}

		r.logger.Info("archive deduplication",
			"storage_id", storageID,
			"original_files", len(collectedFiles),
			"unique_blobs", len(dedupedFiles))

		sendProgress(pb.IngestProgress_INGESTING,
			fmt.Sprintf("Building archives from %d files (%d unique blobs, %d MB)...", len(collectedFiles), len(dedupedFiles), collectedSize/(1024*1024)),
			0, totalFiles, "")

		const maxArchiveSize int64 = 2 * 1024 * 1024 * 1024 // 2GB split threshold
		chunkIndex := 0
		var currentSize int64
		var currentFiles []archiveFile

		// flushArchive builds a packager archive from currentFiles and streams it to fetcher
		flushArchive := func() error {
			if len(currentFiles) == 0 {
				return nil
			}

			// Get encryption key from router config
			encKey := r.config.EncryptionKey
			if len(encKey) != 32 {
				return fmt.Errorf("encryption key must be 32 bytes, got %d", len(encKey))
			}

			pipe, err := pipeline.NewPipeline(encKey)
			if err != nil {
				return fmt.Errorf("create pipeline: %w", err)
			}

			var buf bytes.Buffer
			w := packager.NewArchiveWriter(&buf, pipe)

			for _, f := range currentFiles {
				// Use content hash as the archive entry key (content-addressed storage).
				// This matches how FetchBlob looks up blobs: by ContentID (= hash).
				if err := w.AddFile(f.hash, f.content, packager.DefaultAddFileOptions()); err != nil {
					return fmt.Errorf("add blob %s to archive: %w", f.hash, err)
				}
			}

			if err := w.Close(); err != nil {
				return fmt.Errorf("close archive writer: %w", err)
			}

			archiveData := buf.Bytes()

			r.logger.Info("streaming archive to fetcher",
				"storage_id", storageID,
				"chunk_index", chunkIndex,
				"files_in_archive", len(currentFiles),
				"archive_size_mb", len(archiveData)/(1024*1024))

			if err := r.fetcherClient.StoreArchive(stream.Context(), storageID, chunkIndex, archiveData); err != nil {
				return fmt.Errorf("store archive chunk %d: %w", chunkIndex, err)
			}

			chunkIndex++
			return nil
		}

		for _, f := range dedupedFiles {
			// If adding this file would exceed the split threshold, flush current archive
			if currentSize > 0 && currentSize+int64(len(f.content)) > maxArchiveSize {
				if err := flushArchive(); err != nil {
					sendProgress(pb.IngestProgress_FAILED, fmt.Sprintf("failed to build archive: %v", err), 0, 0, "")
					return fmt.Errorf("archive building failed: %w", err)
				}
				currentFiles = nil
				currentSize = 0
			}

			currentFiles = append(currentFiles, f)
			currentSize += int64(len(f.content))
		}

		// Flush remaining files
		if err := flushArchive(); err != nil {
			sendProgress(pb.IngestProgress_FAILED, fmt.Sprintf("failed to build archive: %v", err), 0, 0, "")
			return fmt.Errorf("archive building failed: %w", err)
		}

		// Free collected content memory
		collectedFiles = nil

		r.logger.Info("archive building complete",
			"storage_id", storageID,
			"total_archives", chunkIndex,
			"total_size_mb", collectedSize/(1024*1024))

		sendProgress(pb.IngestProgress_INGESTING,
			fmt.Sprintf("Archives built: %d chunks", chunkIndex),
			0, totalFiles, "")
	} else if r.fetcherClient == nil {
		r.logger.Warn("no fetcher client configured, skipping archive building")
	}

	// Log distribution for debugging
	totalReplicaFiles := 0
	for nodeID, files := range primaryBatches {
		r.logger.Info("primary batch prepared",
			"node_id", nodeID,
			"file_count", len(files),
			"sample_paths", func() []string {
				if len(files) > 3 {
					return []string{files[0].Path, files[1].Path, files[2].Path}
				}
				return []string{}
			}())
	}
	for replicaNodeID, primaryMap := range replicaBatches {
		for primaryNodeID, files := range primaryMap {
			totalReplicaFiles += len(files)
			r.logger.Debug("replica batch prepared",
				"replica_node", replicaNodeID,
				"primary_node", primaryNodeID,
				"file_count", len(files))
		}
	}

	r.logger.Info("files grouped for ingestion",
		"total_files", totalFiles,
		"primary_nodes", len(primaryBatches),
		"replica_nodes", len(replicaBatches),
		"total_replica_files", totalReplicaFiles,
		"replication_factor", replicationFactor)

	// ===== PHASE 1: Send PRIMARY batches =====
	type batchJob struct {
		nodeID string
		batch  []*pb.FileMetadata
	}

	// Pre-calculate total batches needed for proper channel sizing
	totalPrimaryBatches := 0
	for _, files := range primaryBatches {
		totalPrimaryBatches += (len(files) + batchSize - 1) / batchSize
	}

	batchChan := make(chan batchJob, 100)
	resultChan := make(chan error, totalPrimaryBatches)
	doneChan := make(chan struct{})

	// Start batch worker goroutines
	var wg sync.WaitGroup
	for i := 0; i < maxConcurrentBatches; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for job := range batchChan {
				client, ok := nodeClients[job.nodeID]
				if !ok {
					r.logger.Error("node client not found", "node_id", job.nodeID)
					resultChan <- fmt.Errorf("node %s not found", job.nodeID)
					continue
				}

				// Send batch with timeout
				batchCtx, batchCancel := context.WithTimeout(context.Background(), 60*time.Second)

				r.logger.Debug("sending primary batch to node",
					"node_id", job.nodeID,
					"batch_size", len(job.batch))

				resp, err := client.client.IngestFileBatch(batchCtx, &pb.IngestFileBatchRequest{
					Files:       job.batch,
					StorageId:   storageID,
					DisplayPath: displayPath,
					Source:      sourceURL,
					Ref:         ref,
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

				// Update counter
				atomic.AddInt64(&filesIngested, resp.FilesIngested)

				// Debug logging for specific files
				resultChan <- nil
			}
		}(i)
	}

	// Progress reporter goroutine
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-doneChan:
				return
			case <-ticker.C:
				count := atomic.LoadInt64(&filesIngested)
				sendProgress(pb.IngestProgress_INGESTING,
					fmt.Sprintf("Ingesting files... %d/%d", count, totalFiles),
					count, totalFiles, "")
			}
		}
	}()

	// Feed primary batches to workers
	batchesQueued := 0
	for nodeID, files := range primaryBatches {
		// Split into smaller batches for this node
		for i := 0; i < len(files); i += batchSize {
			end := i + batchSize
			if end > len(files) {
				end = len(files)
			}

			batchChan <- batchJob{
				nodeID: nodeID,
				batch:  files[i:end],
			}
			batchesQueued++
		}
	}
	close(batchChan)

	r.logger.Info("primary batches queued", "count", batchesQueued)

	// Wait for all primary batches to complete
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
		sendProgress(pb.IngestProgress_FAILED, fmt.Sprintf("all batches failed: %v", firstError), filesIngested, totalFiles, "")
		return fmt.Errorf("ingestion failed: %w", firstError)
	}

	if errorCount > 0 {
		r.logger.Warn("some primary batches failed",
			"failed_batches", errorCount,
			"total_batches", batchesQueued,
			"files_ingested", filesIngested)
	}

	r.logger.Info("primary batch ingestion complete",
		"files", filesIngested,
		"batches", batchesQueued,
		"failed_batches", errorCount)

	// ===== PHASE 2: Send REPLICA batches (async, best-effort) =====
	// Replica ingestion is done asynchronously - it's okay if some fail
	// because failover will still work (just slower initial sync)
	if replicationFactor > 1 && len(replicaBatches) > 0 {
		sendProgress(pb.IngestProgress_INGESTING, "Replicating to backup nodes...", filesIngested, totalFiles, "")

		var replicasIngested int64
		var replicaErrors int64
		var replicaWg sync.WaitGroup

		for replicaNodeID, primaryMap := range replicaBatches {
			client, ok := nodeClients[replicaNodeID]
			if !ok {
				continue
			}

			for primaryNodeID, files := range primaryMap {
				// Split into batches
				for i := 0; i < len(files); i += batchSize {
					end := i + batchSize
					if end > len(files) {
						end = len(files)
					}
					batch := files[i:end]

					replicaWg.Add(1)
					go func(nodeID, primaryID string, fileBatch []*pb.FileMetadata, c nodeClient) {
						defer replicaWg.Done()

						ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
						defer cancel()

						resp, err := c.client.IngestReplicaBatch(ctx, &pb.IngestReplicaBatchRequest{
							Files:         fileBatch,
							StorageId:     storageID,
							DisplayPath:   displayPath,
							PrimaryNodeId: primaryID,
							Source:        sourceURL,
							Ref:           ref,
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

	// Build directory indexes on ALL nodes in batch (deferred for performance)
	sendProgress(pb.IngestProgress_INGESTING, "Building directory indexes...", filesIngested, filesIngested, "")

	r.mu.RLock()
	indexingNodes := make([]*nodeState, 0, len(r.nodes))
	for _, state := range r.nodes {
		if state.info.Healthy && state.status == NodeActive && state.client != nil {
			indexingNodes = append(indexingNodes, state)
		}
	}
	r.mu.RUnlock()

	for _, state := range indexingNodes {
		indexCtx, indexCancel := context.WithTimeout(context.Background(), 60*time.Second)
		resp, err := state.client.BuildDirectoryIndexes(indexCtx, &pb.BuildDirectoryIndexesRequest{
			StorageId: storageID,
		})
		indexCancel()

		if err != nil {
			r.logger.Error("failed to build directory indexes on node",
				"node_id", state.info.NodeId,
				"storage_id", storageID,
				"error", err)
		} else {
			r.logger.Info("directory indexes built on node",
				"node_id", state.info.NodeId,
				"storage_id", storageID,
				"directories_indexed", resp.DirectoriesIndexed)
		}
	}

	// Mark repository as onboarded on ALL nodes
	r.mu.RLock()
	activeNodes := make([]*nodeState, 0, len(r.nodes))
	for _, state := range r.nodes {
		if state.info.Healthy && state.status == NodeActive {
			activeNodes = append(activeNodes, state)
		}
	}
	r.mu.RUnlock()

	for _, state := range activeNodes {
		markCtx, markCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := state.client.MarkRepositoryOnboarded(markCtx, &pb.MarkRepositoryOnboardedRequest{
			StorageId: storageID,
		})
		markCancel()

		if err != nil {
			r.logger.Warn("failed to mark repository onboarded on node",
				"node_id", state.info.NodeId,
				"storage_id", storageID,
				"error", err)
		} else {
			r.logger.Info("marked repository as onboarded on node",
				"node_id", state.info.NodeId,
				"storage_id", storageID)
		}
	}

	// Track ingested repository and detect re-ingestion
	r.mu.Lock()
	existingRepo, isReingestion := r.ingestedRepos[storageID]
	r.ingestedRepos[storageID] = &ingestedRepo{
		repoID:            displayPath, // Store display path for UI
		repoURL:           sourceURL,
		branch:            ref,
		filesCount:        filesIngested,
		ingestedAt:        time.Now(),
		topologyVersion:   r.version.Load(), // Current topology version
		targetTopology:    0,
		rebalanceState:    RebalanceStateStable,
		rebalanceProgress: 1.0,
	}
	r.mu.Unlock()
	r.bumpNativeNamespaceGeneration("repository ingest")

	// If this is a re-ingestion of an existing repository, trigger rebalancing
	if isReingestion {
		r.logger.Info("detected repository re-ingestion, triggering cluster rebalancing",
			"storage_id", storageID,
			"display_path", displayPath,
			"previous_ingestion", existingRepo.ingestedAt,
			"previous_files", existingRepo.filesCount,
			"new_files", filesIngested)

		// Trigger rebalancing on all active nodes asynchronously
		// This will redistribute files according to current cluster topology
		go r.rebalanceRepository(storageID)
	}

	// Trigger search indexing asynchronously (if search service configured)
	if r.searchClient != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			resp, err := r.searchClient.IndexRepository(ctx, &pb.IndexRequest{
				StorageId:   storageID,
				DisplayPath: displayPath,
				Source:      sourceURL,
				Ref:         ref,
			})
			if err != nil {
				r.logger.Warn("failed to trigger search indexing",
					"storage_id", storageID,
					"error", err)
			} else if resp.Queued {
				r.logger.Info("search indexing queued",
					"storage_id", storageID,
					"job_id", resp.JobId)
			} else {
				r.logger.Warn("search indexing not queued",
					"storage_id", storageID,
					"message", resp.Message)
			}
		}()
	}

	sendProgress(pb.IngestProgress_COMPLETED, fmt.Sprintf("Repository ingested successfully: %d files", filesIngested), filesIngested, filesIngested, "")
	return nil
}

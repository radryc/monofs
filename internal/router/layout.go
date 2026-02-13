package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/buildlayout"
	"github.com/radryc/monofs/internal/fetcher"
	"github.com/radryc/monofs/internal/sharding"
)

// generateLayouts is called after successful ingestion.
// It runs all matching layout mappers and ingests virtual entries as new repos.
//
// Returns an error if layout generation fails, which will fail the ingestion.
func (r *Router) generateLayouts(
	ctx context.Context,
	info buildlayout.RepoInfo,
	files []buildlayout.FileInfo,
	sendProgress func(stage pb.IngestProgress_Stage, msg string),
) error {
	if r.layoutRegistry == nil || !r.layoutRegistry.HasMappers() {
		return nil
	}

	start := time.Now()

	entries, err := r.layoutRegistry.MapAll(info, files)
	if err != nil {
		r.logger.Error("layout mapper failed",
			"display_path", info.DisplayPath,
			"error", err)
		return fmt.Errorf("failed to generate layouts: %w", err)
	}

	if len(entries) == 0 {
		return nil
	}

	r.logger.Info("generating build layouts",
		"display_path", info.DisplayPath,
		"virtual_entries", len(entries))

	if sendProgress != nil {
		sendProgress(pb.IngestProgress_INGESTING,
			fmt.Sprintf("Generating %d virtual layout entries", len(entries)))
	}

	// Group entries by VirtualDisplayPath — each unique path becomes a virtual repo.
	grouped := make(map[string][]buildlayout.VirtualEntry)
	for _, e := range entries {
		grouped[e.VirtualDisplayPath] = append(grouped[e.VirtualDisplayPath], e)
	}

	// Build a lookup map from OriginalFilePath → FileInfo for fast access.
	fileIndex := make(map[string]*buildlayout.FileInfo, len(files))
	for i := range files {
		fileIndex[files[i].Path] = &files[i]
	}

	// Ingest each virtual repo.
	var failedRepos []string
	for virtualDisplayPath, virtualFiles := range grouped {
		if err := r.ingestVirtualRepo(ctx, info.StorageID, info, virtualDisplayPath, virtualFiles, fileIndex); err != nil {
			r.logger.Error("virtual repo ingestion failed",
				"virtual_path", virtualDisplayPath,
				"error", err)
			failedRepos = append(failedRepos, virtualDisplayPath)
		}
	}

	if len(failedRepos) > 0 {
		return fmt.Errorf("failed to ingest %d virtual repos: %v", len(failedRepos), failedRepos)
	}

	r.logger.Info("build layout generation complete",
		"display_path", info.DisplayPath,
		"virtual_repos", len(grouped),
		"duration", time.Since(start))
	return nil
}

// ingestVirtualRepo registers a virtual repo on all nodes and distributes file metadata.
//
// This reuses the EXISTING IngestFileBatch and BuildDirectoryIndexes RPCs.
// The virtual files have the same BlobHash as the originals, so the fetcher
// serves identical content without duplication.
//
// Storage ID strategy:
//   - Reference entries (OriginalFilePath set): reuse the ORIGINAL storage_id
//     so the fetcher can resolve blobs via the same storage as the source repo.
//   - Synthetic entries (OriginalFilePath empty): generate a NEW storage_id
//     from the virtual display path so they get an isolated directory tree.
func (r *Router) ingestVirtualRepo(
	ctx context.Context,
	originalStorageID string,
	originalInfo buildlayout.RepoInfo,
	virtualDisplayPath string,
	virtualFiles []buildlayout.VirtualEntry,
	fileIndex map[string]*buildlayout.FileInfo,
) error {
	// Determine if this group is fully synthetic (no OriginalFilePath on any entry).
	allSynthetic := true
	for _, vf := range virtualFiles {
		if vf.OriginalFilePath != "" {
			allSynthetic = false
			break
		}
	}

	var virtualStorageID string
	if allSynthetic {
		// Synthetic entries need their own storage ID so their directory tree
		// is isolated from the source repo (e.g., cache/download/ files).
		hash := sha256.Sum256([]byte(virtualDisplayPath))
		virtualStorageID = hex.EncodeToString(hash[:])
	} else {
		// Reference entries reuse the original storage ID (hard link concept)
		// so fetchers can resolve blobs from the same storage.
		virtualStorageID = originalStorageID
	}

	r.logger.Info("ingesting virtual repo",
		"virtual_path", virtualDisplayPath,
		"virtual_storage_id", virtualStorageID,
		"files", len(virtualFiles))

	// Step 1: Get list of healthy nodes.
	r.mu.RLock()
	var healthyNodes []sharding.Node
	var nodeStates []*nodeState
	for id, ns := range r.nodes {
		if ns.status == NodeActive && ns.client != nil {
			healthyNodes = append(healthyNodes, sharding.Node{
				ID:      id,
				Weight:  ns.info.Weight,
				Healthy: true,
			})
			nodeStates = append(nodeStates, ns)
		}
	}
	r.mu.RUnlock()

	if len(healthyNodes) == 0 {
		return fmt.Errorf("no healthy nodes available")
	}

	// Step 2: Determine ingestion and fetch types from original info
	ingestionType := pb.IngestionType_INGESTION_GIT
	fetchType := pb.SourceType_SOURCE_TYPE_GIT

	switch strings.ToLower(originalInfo.IngestionType) {
	case "git":
		ingestionType = pb.IngestionType_INGESTION_GIT
	case "go", "ingestion_go":
		ingestionType = pb.IngestionType_INGESTION_GO
	}

	switch strings.ToLower(originalInfo.FetchType) {
	case "git", "source_type_git":
		fetchType = pb.SourceType_SOURCE_TYPE_GIT
	case "gomod", "source_type_gomod":
		fetchType = pb.SourceType_SOURCE_TYPE_GOMOD
	case "npm", "source_type_npm":
		fetchType = pb.SourceType_SOURCE_TYPE_NPM
	case "maven", "source_type_maven":
		fetchType = pb.SourceType_SOURCE_TYPE_MAVEN
	case "cargo", "source_type_cargo":
		fetchType = pb.SourceType_SOURCE_TYPE_CARGO
	}

	// Step 3: Register virtual repo on ALL healthy nodes (parallel).
	var regWg sync.WaitGroup
	for _, ns := range nodeStates {
		ns := ns
		regWg.Add(1)
		go func() {
			defer regWg.Done()
			regCtx, regCancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := ns.client.RegisterRepository(regCtx, &pb.RegisterRepositoryRequest{
				StorageId:     virtualStorageID,
				DisplayPath:   virtualDisplayPath,
				Source:        originalInfo.Source,
				IngestionType: ingestionType,
				FetchType:     fetchType,
			})
			regCancel()

			if err != nil {
				r.logger.Warn("failed to register virtual repo on node",
					"node_id", ns.info.NodeId,
					"virtual_path", virtualDisplayPath,
					"error", err)
				// Non-fatal for individual node failures
			}
		}()
	}
	regWg.Wait()

	// Step 4: Build protobuf FileMetadata from VirtualEntry + original FileInfo.
	// Supports two modes:
	//   a) Reference mode: OriginalFilePath is set → look up from fileIndex (existing).
	//   b) Synthetic mode: OriginalFilePath is empty → use VirtualEntry's own fields
	//      (for generated cache artifacts that have no original file).
	var batch []*pb.FileMetadata
	for _, vf := range virtualFiles {
		var fm *pb.FileMetadata

		if vf.OriginalFilePath != "" {
			// Reference mode: look up original file
			orig, ok := fileIndex[vf.OriginalFilePath]
			if !ok {
				r.logger.Debug("skipping virtual file: original not found",
					"original_path", vf.OriginalFilePath,
					"virtual_path", vf.VirtualFilePath)
				continue
			}

			fm = &pb.FileMetadata{
				Path:            vf.VirtualFilePath,
				StorageId:       virtualStorageID,
				DisplayPath:     virtualDisplayPath,
				Ref:             originalInfo.Ref,
				Size:            orig.Size,
				Mtime:           orig.Mtime,
				Mode:            orig.Mode,
				BlobHash:        orig.BlobHash,
				Source:          orig.Source,
				SourceType:      ingestionType,
				FetchType:       fetchType,
				BackendMetadata: orig.BackendMetadata,
			}
		} else {
			// Synthetic mode: use VirtualEntry's own fields (generated cache artifacts)
			source := vf.Source
			if source == "" {
				source = originalInfo.Source
			}
			mode := vf.Mode
			if mode == 0 {
				mode = 0444 // read-only default for generated files
			}

			// Calculate real size for synthetic files with unknown size
			size := vf.Size
			if size == 0 && vf.BlobHash != "" && r.fetcherClient != nil {
				// Fetch blob content once during ingestion to determine real size
				fetchReq := &fetcher.FetchRequest{
					ContentID:    vf.BlobHash,
					SourceKey:    originalInfo.Source,
					SourceConfig: vf.BackendMetadata,
					RequestID:    fmt.Sprintf("size-calc-%s", vf.VirtualFilePath),
					Priority:     5, // Medium priority
				}

				fetchCtx, fetchCancel := context.WithTimeout(ctx, 30*time.Second)
				content, err := r.fetcherClient.FetchBlob(fetchCtx, fetchReq, fetchType)
				fetchCancel()

				if err != nil {
					r.logger.Warn("failed to calculate size for synthetic file",
						"virtual_path", vf.VirtualFilePath,
						"blob_hash", vf.BlobHash,
						"error", err)
					// Continue with size=0, file will still be accessible
				} else {
					size = uint64(len(content))
					r.logger.Debug("calculated size for synthetic file",
						"virtual_path", vf.VirtualFilePath,
						"blob_hash", vf.BlobHash,
						"size", size)
				}
			}

			fm = &pb.FileMetadata{
				Path:            vf.VirtualFilePath,
				StorageId:       virtualStorageID,
				DisplayPath:     virtualDisplayPath,
				Ref:             originalInfo.Ref,
				Size:            size,
				Mtime:           vf.Mtime,
				Mode:            mode,
				BlobHash:        vf.BlobHash,
				Source:          source,
				SourceType:      ingestionType,
				FetchType:       fetchType,
				BackendMetadata: vf.BackendMetadata,
			}
		}
		batch = append(batch, fm)
	}

	if len(batch) == 0 {
		return fmt.Errorf("no files could be mapped (all originals missing)")
	}

	// Step 5: Distribute files using HRW sharding.
	// IMPORTANT: The shard key must match what the FUSE client computes when reading.
	// The client uses: sha256(first 3 path parts) + ":" + remaining path parts
	// where the full path = displayPath + "/" + filePath.
	// We must use the same formula so reads route to the same node.
	sharder := sharding.NewHRW(healthyNodes)
	nodeBatches := make(map[string][]*pb.FileMetadata)
	for _, fm := range batch {
		// Reconstruct the full filesystem path that the FUSE client will see
		fullPath := virtualDisplayPath + "/" + fm.Path
		shardKey := clientShardKey(fullPath)
		targetNodes := sharder.GetNodes(shardKey, 1) // Primary only for virtual files
		if len(targetNodes) > 0 {
			nodeBatches[targetNodes[0].ID] = append(nodeBatches[targetNodes[0].ID], fm)
		}
	}

	// Step 6: Send batches to nodes (batch size 1000, same as normal ingestion).
	const batchSize = 1000
	for nodeID, fileBatch := range nodeBatches {
		ns := r.getNodeByID(nodeID)
		if ns == nil || ns.client == nil {
			continue
		}

		for i := 0; i < len(fileBatch); i += batchSize {
			end := i + batchSize
			if end > len(fileBatch) {
				end = len(fileBatch)
			}

			batchCtx, batchCancel := context.WithTimeout(ctx, 30*time.Second)
			_, err := ns.client.IngestFileBatch(batchCtx, &pb.IngestFileBatchRequest{
				Files:       fileBatch[i:end],
				StorageId:   virtualStorageID,
				DisplayPath: virtualDisplayPath,
				Source:      originalInfo.Source,
				Ref:         originalInfo.Ref,
			})
			batchCancel()

			if err != nil {
				r.logger.Warn("virtual batch ingest failed",
					"node_id", nodeID,
					"batch_start", i,
					"batch_end", end,
					"error", err)
			}
		}
	}

	// Step 7: Build directory indexes on all nodes that received files.
	for nodeID := range nodeBatches {
		ns := r.getNodeByID(nodeID)
		if ns == nil || ns.client == nil {
			continue
		}

		idxCtx, idxCancel := context.WithTimeout(ctx, 5*time.Minute)
		_, err := ns.client.BuildDirectoryIndexes(idxCtx, &pb.BuildDirectoryIndexesRequest{
			StorageId: virtualStorageID,
		})
		idxCancel()

		if err != nil {
			r.logger.Warn("virtual dir index build failed",
				"node_id", nodeID,
				"error", err)
		}
	}

	// Step 8: Mark virtual repo as onboarded on all nodes that have files.
	for nodeID := range nodeBatches {
		ns := r.getNodeByID(nodeID)
		if ns == nil || ns.client == nil {
			continue
		}

		onbCtx, onbCancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := ns.client.MarkRepositoryOnboarded(onbCtx, &pb.MarkRepositoryOnboardedRequest{
			StorageId: virtualStorageID,
		})
		onbCancel()

		if err != nil {
			r.logger.Warn("virtual onboarding mark failed",
				"node_id", nodeID,
				"error", err)
		}
	}

	r.logger.Info("virtual repo ingested successfully",
		"virtual_path", virtualDisplayPath,
		"files", len(batch))

	return nil
}

// getNodeByID returns the nodeState for a given node ID, or nil.
// Caller must NOT hold r.mu.
func (r *Router) getNodeByID(nodeID string) *nodeState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.nodes[nodeID]
}

// clientShardKey computes the HRW shard key the same way the FUSE client does.
// The client splits the full filesystem path, takes the first 3 parts as display path,
// hashes them with SHA-256, and appends ":" + remaining path as the file path.
// This MUST stay in sync with client.buildShardKey().
func clientShardKey(fullPath string) string {
	parts := strings.Split(fullPath, "/")
	if len(parts) > 3 {
		displayPath := strings.Join(parts[:3], "/")
		filePath := strings.Join(parts[3:], "/")
		hash := sha256.Sum256([]byte(displayPath))
		storageID := hex.EncodeToString(hash[:])
		return storageID + ":" + filePath
	}
	if len(parts) == 3 {
		hash := sha256.Sum256([]byte(fullPath))
		return hex.EncodeToString(hash[:])
	}
	return fullPath
}

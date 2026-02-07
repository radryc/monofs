package router

import (
	"context"
	"fmt"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/buildlayout"
	"github.com/radryc/monofs/internal/sharding"
)

// generateLayouts is called after successful ingestion.
// It runs all matching layout mappers and ingests virtual entries as new repos.
//
// Failures are logged but NOT propagated — the original ingestion already succeeded.
// Virtual layout generation is best-effort.
func (r *Router) generateLayouts(
	ctx context.Context,
	info buildlayout.RepoInfo,
	files []buildlayout.FileInfo,
	sendProgress func(stage pb.IngestProgress_Stage, msg string),
) {
	if r.layoutRegistry == nil || !r.layoutRegistry.HasMappers() {
		return
	}

	start := time.Now()

	entries, err := r.layoutRegistry.MapAll(info, files)
	if err != nil {
		r.logger.Warn("layout mapper failed (non-fatal)",
			"display_path", info.DisplayPath,
			"error", err)
		return
	}

	if len(entries) == 0 {
		return
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
	for virtualDisplayPath, virtualFiles := range grouped {
		if err := r.ingestVirtualRepo(ctx, info.StorageID, info, virtualDisplayPath, virtualFiles, fileIndex); err != nil {
			r.logger.Warn("virtual repo ingestion failed (non-fatal)",
				"virtual_path", virtualDisplayPath,
				"error", err)
			// Continue with other virtual repos
		}
	}

	r.logger.Info("build layout generation complete",
		"display_path", info.DisplayPath,
		"virtual_repos", len(grouped),
		"duration", time.Since(start))
}

// ingestVirtualRepo registers a virtual repo on all nodes and distributes file metadata.
//
// This reuses the EXISTING IngestFileBatch and BuildDirectoryIndexes RPCs.
// The virtual files have the same BlobHash as the originals, so the fetcher
// serves identical content without duplication.
//
// CRITICAL: Uses the ORIGINAL storage_id (hard link concept) so both paths
// resolve to the same storage, making files accessible via either path.
func (r *Router) ingestVirtualRepo(
	ctx context.Context,
	originalStorageID string,
	originalInfo buildlayout.RepoInfo,
	virtualDisplayPath string,
	virtualFiles []buildlayout.VirtualEntry,
	fileIndex map[string]*buildlayout.FileInfo,
) error {
	// Use the ORIGINAL storage_id (hard link) - both display paths point to same storage
	virtualStorageID := originalStorageID

	r.logger.Info("ingesting virtual repo",
		"virtual_path", virtualDisplayPath,
		"virtual_storage_id", virtualStorageID,
		"files", len(virtualFiles))

	// Step 1: Get list of healthy nodes.
	r.mu.RLock()
	var healthyNodes []sharding.Node
	var nodeStates []*nodeState
	for id, ns := range r.nodes {
		if ns.status == NodeActive {
			healthyNodes = append(healthyNodes, sharding.Node{
				ID:     id,
				Weight: ns.info.Weight,
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
	fetchType := pb.FetchType_FETCH_GIT

	switch strings.ToLower(originalInfo.IngestionType) {
	case "git":
		ingestionType = pb.IngestionType_INGESTION_GIT
	case "go":
		ingestionType = pb.IngestionType_INGESTION_GO
	}

	switch strings.ToLower(originalInfo.FetchType) {
	case "git":
		fetchType = pb.FetchType_FETCH_GIT
	case "gomod":
		fetchType = pb.FetchType_FETCH_GOMOD
	}

	// Step 3: Register virtual repo on ALL healthy nodes.
	for _, ns := range nodeStates {
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
	}

	// Step 4: Build protobuf FileMetadata from VirtualEntry + original FileInfo.
	var batch []*pb.FileMetadata
	for _, vf := range virtualFiles {
		orig, ok := fileIndex[vf.OriginalFilePath]
		if !ok {
			r.logger.Debug("skipping virtual file: original not found",
				"original_path", vf.OriginalFilePath,
				"virtual_path", vf.VirtualFilePath)
			continue
		}

		fm := &pb.FileMetadata{
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
		batch = append(batch, fm)
	}

	if len(batch) == 0 {
		return fmt.Errorf("no files could be mapped (all originals missing)")
	}

	// Step 5: Distribute files using HRW sharding (same as normal ingestion).
	sharder := sharding.NewHRW(healthyNodes)
	nodeBatches := make(map[string][]*pb.FileMetadata)
	for _, fm := range batch {
		shardKey := virtualStorageID + ":" + fm.Path
		targetNodes := sharder.GetNodes(shardKey, 1) // Primary only for virtual files
		if len(targetNodes) > 0 {
			nodeBatches[targetNodes[0].ID] = append(nodeBatches[targetNodes[0].ID], fm)
		}
	}

	// Step 6: Send batches to nodes (batch size 1000, same as normal ingestion).
	const batchSize = 1000
	for nodeID, fileBatch := range nodeBatches {
		ns := r.getNodeByID(nodeID)
		if ns == nil {
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
		if ns == nil {
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
		if ns == nil {
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

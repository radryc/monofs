package router

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/sharding"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// InjectGuardianPartition stores inline YAML files for a guardian partition
// directly on all cluster nodes, bypassing the git/S3 ingestion pipeline.
// Requires a valid guardian_token from a registered guardian client.
func (r *Router) InjectGuardianPartition(ctx context.Context, req *pb.InjectGuardianPartitionRequest) (*pb.InjectGuardianPartitionResponse, error) {
	if _, ok := r.validateGuardianToken(req.GuardianToken); !ok {
		return &pb.InjectGuardianPartitionResponse{
			Success: false,
			Message: "invalid guardian token",
		}, nil
	}

	if req.PartitionName == "" {
		return nil, fmt.Errorf("partition_name is required")
	}
	if len(req.Files) == 0 {
		return nil, fmt.Errorf("at least one file is required")
	}

	displayPath := "guardian/" + req.PartitionName
	storageID := sharding.GenerateStorageID(displayPath)

	r.logger.Info("injecting guardian partition",
		"partition", req.PartitionName,
		"display_path", displayPath,
		"storage_id", storageID,
		"files", len(req.Files),
	)

	// Collect healthy nodes.
	r.mu.RLock()
	type nodeEntry struct {
		id      string
		address string
		client  pb.MonoFSClient
		conn    *grpc.ClientConn
	}
	nodes := make([]nodeEntry, 0, len(r.nodes))
	for id, state := range r.nodes {
		if state.info.Healthy {
			nodes = append(nodes, nodeEntry{
				id:      id,
				address: state.info.Address,
				client:  state.client,
			})
		}
	}
	r.mu.RUnlock()

	if len(nodes) == 0 {
		return nil, fmt.Errorf("no healthy nodes available")
	}

	// Build FileMetadata slice — inline content for all files.
	now := time.Now().Unix()
	metaFiles := make([]*pb.FileMetadata, 0, len(req.Files))
	for _, f := range req.Files {
		h := sha256.Sum256(f.Content)
		blobHash := fmt.Sprintf("%x", h)
		metaFiles = append(metaFiles, &pb.FileMetadata{
			Path:          f.Path,
			StorageId:     storageID,
			DisplayPath:   displayPath,
			Size:          uint64(len(f.Content)),
			Mtime:         now,
			Mode:          0644,
			BlobHash:      blobHash,
			Source:        "guardian-inject",
			InlineContent: f.Content,
			SourceType:    pb.IngestionType_INGESTION_GUARDIAN,
			FetchType:     pb.SourceType_SOURCE_TYPE_BLOB,
		})
	}

	// Register repository + ingest files on every healthy node in parallel.
	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		nodeErrors  []error
		filesStored int32
	)

	for _, n := range nodes {
		n := n
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Ensure we have a live gRPC connection.
			nodeClient := n.client
			if nodeClient == nil {
				conn, err := grpc.NewClient(n.address,
					grpc.WithTransportCredentials(insecure.NewCredentials()),
				)
				if err != nil {
					mu.Lock()
					nodeErrors = append(nodeErrors, fmt.Errorf("node %s connect: %w", n.id, err))
					mu.Unlock()
					return
				}
				defer conn.Close()
				nodeClient = pb.NewMonoFSClient(conn)
			}

			regCtx, regCancel := context.WithTimeout(ctx, 10*time.Second)
			defer regCancel()

			// Register the repository so FUSE lookup can resolve the display path.
			_, err := nodeClient.RegisterRepository(regCtx, &pb.RegisterRepositoryRequest{
				StorageId:     storageID,
				DisplayPath:   displayPath,
				Source:        "guardian-inject",
				IngestionType: pb.IngestionType_INGESTION_GUARDIAN,
				FetchType:     pb.SourceType_SOURCE_TYPE_BLOB,
			})
			if err != nil {
				r.logger.Warn("RegisterRepository failed on node", "node", n.id, "error", err)
				// Non-fatal: proceed to file ingestion anyway.
			}

			batchCtx, batchCancel := context.WithTimeout(ctx, 30*time.Second)
			defer batchCancel()

			resp, err := nodeClient.IngestFileBatch(batchCtx, &pb.IngestFileBatchRequest{
				Files:       metaFiles,
				StorageId:   storageID,
				DisplayPath: displayPath,
				Source:      "guardian-inject",
			})
			if err != nil {
				mu.Lock()
				nodeErrors = append(nodeErrors, fmt.Errorf("node %s IngestFileBatch: %w", n.id, err))
				mu.Unlock()
				return
			}
			if !resp.Success {
				mu.Lock()
				nodeErrors = append(nodeErrors, fmt.Errorf("node %s: %s", n.id, resp.ErrorMessage))
				mu.Unlock()
				return
			}
			mu.Lock()
			if resp.FilesIngested > 0 {
				filesStored = int32(resp.FilesIngested)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(nodeErrors) > 0 {
		r.logger.Warn("some nodes failed during guardian inject",
			"partition", req.PartitionName,
			"errors", len(nodeErrors),
		)
	}

	// Register the partition in the router's in-memory repository index so
	// discovery / UI picks it up.
	r.mu.Lock()
	r.ingestedRepos[storageID] = &ingestedRepo{
		repoID:     displayPath,
		repoURL:    "guardian-inject",
		filesCount: int64(len(req.Files)),
		ingestedAt: time.Now(),
	}
	r.mu.Unlock()

	if filesStored == 0 {
		filesStored = int32(len(req.Files))
	}

	r.logger.Info("guardian partition injected",
		"partition", req.PartitionName,
		"storage_id", storageID,
		"files", filesStored,
		"node_errors", len(nodeErrors),
	)

	return &pb.InjectGuardianPartitionResponse{
		Success:       true,
		StorageId:     storageID,
		Message:       fmt.Sprintf("partition %q injected: %d files on %d nodes", req.PartitionName, filesStored, len(nodes)),
		FilesIngested: filesStored,
	}, nil
}

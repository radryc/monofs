package router

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/sharding"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type guardianNodeTarget struct {
	id      string
	address string
	client  pb.MonoFSClient
}

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

	nodes := r.collectHealthyGuardianNodes()
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no healthy nodes available")
	}
	existingFiles := r.lookupGuardianExistingFiles(ctx, nodes, displayPath, req.Files)

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

			nodeClient, closeConn, err := r.guardianNodeClient(n)
			if err != nil {
				mu.Lock()
				nodeErrors = append(nodeErrors, fmt.Errorf("node %s connect: %w", n.id, err))
				mu.Unlock()
				return
			}
			defer closeConn()

			regCtx, regCancel := context.WithTimeout(ctx, 10*time.Second)
			defer regCancel()

			// Register the repository so FUSE lookup can resolve the display path.
			_, err = nodeClient.RegisterRepository(regCtx, &pb.RegisterRepositoryRequest{
				StorageId:     storageID,
				DisplayPath:   displayPath,
				Source:        "guardian-inject",
				IngestionType: pb.IngestionType_INGESTION_GUARDIAN,
				FetchType:     pb.SourceType_SOURCE_TYPE_BLOB,
				GuardianUrl:   req.GuardianUiUrl,
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
		errorDetails := make([]string, 0, len(nodeErrors))
		for _, err := range nodeErrors {
			errorDetails = append(errorDetails, err.Error())
		}
		r.logger.Warn("some nodes failed during guardian inject",
			"partition", req.PartitionName,
			"errors", len(nodeErrors),
			"details", errorDetails,
		)
	}

	// Register the partition in the router's in-memory repository index so
	// discovery / UI picks it up.
	r.mu.Lock()
	repoURL := req.GuardianUiUrl
	if repoURL == "" {
		if existing, ok := r.ingestedRepos[storageID]; ok && existing != nil && existing.repoURL != "" {
			repoURL = existing.repoURL
		}
	}
	if repoURL == "" {
		repoURL = "guardian-inject"
	}
	r.ingestedRepos[storageID] = &ingestedRepo{
		repoID:     displayPath,
		repoURL:    repoURL,
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
	r.publishGuardianInjectedFiles(storageID, req.Files, existingFiles)

	return &pb.InjectGuardianPartitionResponse{
		Success:       true,
		StorageId:     storageID,
		Message:       fmt.Sprintf("partition %q injected: %d files on %d nodes", req.PartitionName, filesStored, len(nodes)),
		FilesIngested: filesStored,
	}, nil
}

func (r *Router) collectHealthyGuardianNodes() []guardianNodeTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()

	nodes := make([]guardianNodeTarget, 0, len(r.nodes))
	for id, state := range r.nodes {
		if state.info.Healthy {
			nodes = append(nodes, guardianNodeTarget{
				id:      id,
				address: state.info.Address,
				client:  state.client,
			})
		}
	}

	return nodes
}

func (r *Router) guardianNodeClient(target guardianNodeTarget) (pb.MonoFSClient, func(), error) {
	if target.client != nil {
		return target.client, func() {}, nil
	}

	conn, err := grpc.NewClient(target.address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}

	return pb.NewMonoFSClient(conn), func() {
		_ = conn.Close()
	}, nil
}

func (r *Router) lookupGuardianExistingFiles(ctx context.Context, nodes []guardianNodeTarget, displayPath string, files []*pb.InjectGuardianFile) map[string]bool {
	existing := make(map[string]bool, len(files))
	if len(nodes) == 0 || len(files) == 0 {
		return existing
	}

	nodeClient, closeConn, err := r.guardianNodeClient(nodes[0])
	if err != nil {
		r.logger.Warn("failed to initialize guardian lookup client", "node", nodes[0].id, "error", err)
		return existing
	}
	defer closeConn()

	for _, file := range files {
		relPath := cleanGuardianRelativePath(file.Path)
		if relPath == "" {
			continue
		}

		attrCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := nodeClient.GetAttr(attrCtx, &pb.GetAttrRequest{
			Path: displayPath + "/" + relPath,
		})
		cancel()
		if err == nil && resp != nil && resp.Found {
			existing[relPath] = true
		}
	}

	return existing
}

func (r *Router) publishGuardianInjectedFiles(storageID string, files []*pb.InjectGuardianFile, existing map[string]bool) {
	for _, file := range files {
		relPath := cleanGuardianRelativePath(file.Path)
		if relPath == "" {
			continue
		}

		changeType := pb.ChangeType_ADDED
		if existing[relPath] {
			changeType = pb.ChangeType_MODIFIED
		}

		event := &pb.ChangeEvent{
			StorageId:   storageID,
			FilePath:    relPath,
			Type:        changeType,
			NewBlobHash: guardianContentHash(file.Content),
		}
		if len(file.Content) < 64*1024 {
			event.InlineContent = append([]byte(nil), file.Content...)
		}

		r.publishGuardianChange(event)
	}
}

func cleanGuardianRelativePath(input string) string {
	cleaned := strings.TrimSpace(input)
	if cleaned == "" {
		return ""
	}
	cleaned = strings.TrimPrefix(cleaned, "/")
	cleaned = path.Clean(cleaned)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

func guardianContentHash(content []byte) string {
	hash := sha256.Sum256(content)
	return fmt.Sprintf("%x", hash)
}

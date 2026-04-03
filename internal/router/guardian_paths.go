package router

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"syscall"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type guardianLogicalChangeSubscriber struct {
	id                 uint64
	logicalPrefixes    []string
	includeInlineBytes bool
	events             chan *pb.GuardianChangeEvent
}

type guardianUpsertGroup struct {
	displayPath string
	storageID   string
	files       []*pb.FileMetadata
}

type guardianUpsertPlan struct {
	write      *pb.GuardianPathWrite
	mapped     guardianPhysicalPath
	changeType pb.ChangeType
}

type guardianDeletePlan struct {
	deleteReq    *pb.GuardianPathDelete
	mapped       guardianPhysicalPath
	existing     *storedGuardianFileVersion
	content      []byte
	changeType   pb.ChangeType
	physicalPath string
	isDir        bool
	deleteRepo   bool
	needsDelete  bool
}

func (r *Router) UpsertGuardianPaths(ctx context.Context, req *pb.UpsertGuardianPathsRequest) (*pb.UpsertGuardianPathsResponse, error) {
	principal, ok := r.authenticateGuardianToken(req.GetGuardianToken())
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "invalid guardian token")
	}
	if len(req.GetWrites()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one write is required")
	}

	now := time.Now()
	batchRevisionID := guardianBatchRevisionID(now)
	committedAt := now.Unix()
	correlationID := ""
	reason := ""
	if req.GetContext() != nil {
		correlationID = req.GetContext().GetCorrelationId()
		reason = req.GetContext().GetReason()
	}

	plans := make([]guardianUpsertPlan, 0, len(req.GetWrites()))
	groups := make(map[string]*guardianUpsertGroup)
	for _, write := range req.GetWrites() {
		mapped, err := mapGuardianLogicalPath(write.GetLogicalPath())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid logical path %q: %v", write.GetLogicalPath(), err)
		}
		if mapped.RelativePath == "" {
			return nil, status.Errorf(codes.InvalidArgument, "logical path %q must target a file", write.GetLogicalPath())
		}
		if err := authorizeGuardianMutation(principal, mapped.LogicalPath, false); err != nil {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}

		current, exists := r.guardianVersions.currentVersion(mapped.LogicalPath)
		exists = exists && !current.Tombstone
		switch expected := strings.TrimSpace(write.GetExpectedVersionId()); expected {
		case "":
		case "absent":
			if exists {
				return nil, status.Errorf(codes.AlreadyExists, "logical path %q already exists", mapped.LogicalPath)
			}
		default:
			if !exists || current.VersionID != expected {
				return nil, status.Errorf(codes.FailedPrecondition, "logical path %q current version mismatch", mapped.LogicalPath)
			}
		}

		changeType := pb.ChangeType_ADDED
		if exists {
			changeType = pb.ChangeType_MODIFIED
		}
		group := groups[mapped.DisplayPath]
		if group == nil {
			group = &guardianUpsertGroup{
				displayPath: mapped.DisplayPath,
				storageID:   mapped.StorageID,
			}
			groups[mapped.DisplayPath] = group
		}
		group.files = append(group.files, &pb.FileMetadata{
			Path:          mapped.RelativePath,
			StorageId:     mapped.StorageID,
			DisplayPath:   mapped.DisplayPath,
			Size:          uint64(len(write.GetContent())),
			Mtime:         committedAt,
			Mode:          0o644,
			BlobHash:      guardianContentHash(write.GetContent()),
			Source:        "guardian-path-api",
			InlineContent: append([]byte(nil), write.GetContent()...),
			SourceType:    pb.IngestionType_INGESTION_GUARDIAN,
			FetchType:     pb.SourceType_SOURCE_TYPE_BLOB,
		})
		plans = append(plans, guardianUpsertPlan{
			write:      write,
			mapped:     mapped,
			changeType: changeType,
		})
	}

	if err := r.applyGuardianUpsertGroups(ctx, groups, principal); err != nil {
		return nil, status.Errorf(codes.Internal, "upsert guardian paths: %v", err)
	}

	versions := make([]*pb.GuardianFileVersion, 0, len(plans))
	for _, plan := range plans {
		version, err := r.guardianVersions.commit(guardianVersionCommit{
			LogicalPath:     plan.mapped.LogicalPath,
			DisplayPath:     plan.mapped.DisplayPath,
			StorageID:       plan.mapped.StorageID,
			BatchRevisionID: batchRevisionID,
			PrincipalID:     principal.PrincipalID,
			Reason:          reason,
			CorrelationID:   correlationID,
			Content:         plan.write.GetContent(),
			Tombstone:       false,
			CommittedAt:     committedAt,
		})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "record guardian version: %v", err)
		}
		versions = append(versions, version)
		event := buildGuardianChangeEvent(version, plan.mapped.LogicalPath, plan.changeType, correlationID, plan.write.GetContent())
		r.publishGuardianLogicalChange(event)
		r.publishLegacyGuardianChange(event)
	}

	return &pb.UpsertGuardianPathsResponse{
		Success:         true,
		BatchRevisionId: batchRevisionID,
		Versions:        versions,
		Message:         fmt.Sprintf("upserted %d guardian path(s)", len(versions)),
	}, nil
}

func (r *Router) DeleteGuardianPaths(ctx context.Context, req *pb.DeleteGuardianPathsRequest) (*pb.DeleteGuardianPathsResponse, error) {
	principal, ok := r.authenticateGuardianToken(req.GetGuardianToken())
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "invalid guardian token")
	}
	if len(req.GetDeletes()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one delete is required")
	}

	now := time.Now()
	batchRevisionID := guardianBatchRevisionID(now)
	committedAt := now.Unix()
	correlationID := ""
	reason := ""
	if req.GetContext() != nil {
		correlationID = req.GetContext().GetCorrelationId()
		reason = req.GetContext().GetReason()
	}

	nodes := r.collectHealthyGuardianNodes()
	if len(nodes) == 0 {
		return nil, status.Error(codes.Unavailable, "no healthy nodes available")
	}
	nodeClient, closeConn, err := r.guardianNodeClient(nodes[0])
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "guardian node client: %v", err)
	}
	defer closeConn()

	plans := make([]guardianDeletePlan, 0, len(req.GetDeletes()))
	for _, deleteReq := range req.GetDeletes() {
		mapped, err := mapGuardianLogicalPath(deleteReq.GetLogicalPath())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid logical path %q: %v", deleteReq.GetLogicalPath(), err)
		}
		if err := authorizeGuardianMutation(principal, mapped.LogicalPath, true); err != nil {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}

		current, exists := r.guardianVersions.currentVersion(mapped.LogicalPath)
		if expected := strings.TrimSpace(deleteReq.GetExpectedVersionId()); expected != "" {
			if !exists || current.VersionID != expected {
				return nil, status.Errorf(codes.FailedPrecondition, "logical path %q current version mismatch", mapped.LogicalPath)
			}
		}

		plan := guardianDeletePlan{
			deleteReq:  deleteReq,
			mapped:     mapped,
			existing:   current,
			changeType: pb.ChangeType_DELETED,
		}

		switch {
		case mapped.RelativePath == "" && mapped.DisplayPath == "guardian-system":
			return nil, status.Error(codes.InvalidArgument, "guardian-system root cannot be deleted")
		case mapped.RelativePath == "":
			plan.deleteRepo = true
			plan.needsDelete = true
		default:
			plan.physicalPath = guardianDisplayPathJoin(mapped.DisplayPath, mapped.RelativePath)
			attrCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			attr, attrErr := nodeClient.GetAttr(attrCtx, &pb.GetAttrRequest{Path: plan.physicalPath})
			cancel()
			if attrErr == nil && attr != nil && attr.Found {
				plan.needsDelete = true
				plan.isDir = attr.Mode&uint32(syscall.S_IFDIR) != 0
				if !plan.isDir {
					plan.content, _ = readGuardianFileContent(ctx, nodeClient, plan.physicalPath)
				}
			}
		}

		if plan.existing != nil && len(plan.existing.Content) > 0 {
			plan.content = append([]byte(nil), plan.existing.Content...)
		}
		plans = append(plans, plan)
	}

	for _, plan := range plans {
		if !plan.needsDelete {
			continue
		}
		switch {
		case plan.deleteRepo:
			if _, err := r.deleteRepositoryInternal(ctx, plan.mapped.StorageID); err != nil {
				return nil, status.Errorf(codes.Internal, "delete guardian repository %q: %v", plan.mapped.LogicalPath, err)
			}
		case plan.isDir:
			if _, err := r.deleteGuardianDirFromAllNodes(plan.mapped.StorageID, plan.mapped.RelativePath); err != nil {
				return nil, status.Errorf(codes.Internal, "delete guardian directory %q: %v", plan.mapped.LogicalPath, err)
			}
		default:
			if _, err := r.deleteGuardianFileFromAllNodes(plan.mapped.StorageID, plan.mapped.RelativePath); err != nil {
				return nil, status.Errorf(codes.Internal, "delete guardian file %q: %v", plan.mapped.LogicalPath, err)
			}
		}
	}

	tombstones := make([]*pb.GuardianFileVersion, 0, len(plans))
	for _, plan := range plans {
		version, err := r.guardianVersions.commit(guardianVersionCommit{
			LogicalPath:     plan.mapped.LogicalPath,
			DisplayPath:     plan.mapped.DisplayPath,
			StorageID:       plan.mapped.StorageID,
			BatchRevisionID: batchRevisionID,
			PrincipalID:     principal.PrincipalID,
			Reason:          reason,
			CorrelationID:   correlationID,
			Content:         plan.content,
			Tombstone:       true,
			CommittedAt:     committedAt,
		})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "record guardian tombstone: %v", err)
		}
		tombstones = append(tombstones, version)
		event := buildGuardianChangeEvent(version, plan.mapped.LogicalPath, plan.changeType, correlationID, nil)
		r.publishGuardianLogicalChange(event)
		r.publishLegacyGuardianChange(event)
	}

	return &pb.DeleteGuardianPathsResponse{
		Success:         true,
		BatchRevisionId: batchRevisionID,
		Tombstones:      tombstones,
		Message:         fmt.Sprintf("deleted %d guardian path(s)", len(tombstones)),
	}, nil
}

func (r *Router) ListGuardianVersions(ctx context.Context, req *pb.ListGuardianVersionsRequest) (*pb.ListGuardianVersionsResponse, error) {
	if _, ok := r.authenticateGuardianToken(req.GetGuardianToken()); !ok {
		return nil, status.Error(codes.PermissionDenied, "invalid guardian token")
	}
	logicalPath, err := normalizeGuardianLogicalPath(req.GetLogicalPath())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid logical path %q: %v", req.GetLogicalPath(), err)
	}

	versions, nextToken, err := r.guardianVersions.list(logicalPath, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.ListGuardianVersionsResponse{
		Versions:      versions,
		NextPageToken: nextToken,
	}, nil
}

func (r *Router) GetGuardianVersion(ctx context.Context, req *pb.GetGuardianVersionRequest) (*pb.GetGuardianVersionResponse, error) {
	if _, ok := r.authenticateGuardianToken(req.GetGuardianToken()); !ok {
		return nil, status.Error(codes.PermissionDenied, "invalid guardian token")
	}
	logicalPath, err := normalizeGuardianLogicalPath(req.GetLogicalPath())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid logical path %q: %v", req.GetLogicalPath(), err)
	}
	if strings.TrimSpace(req.GetVersionId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "version_id is required")
	}

	version, content, ok := r.guardianVersions.get(logicalPath, req.GetVersionId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "version %q not found for %q", req.GetVersionId(), logicalPath)
	}
	return &pb.GetGuardianVersionResponse{
		Version: version,
		Content: content,
	}, nil
}

func (r *Router) SubscribeGuardianChanges(req *pb.SubscribeGuardianChangesRequest, stream grpc.ServerStreamingServer[pb.GuardianChangeEvent]) error {
	if _, ok := r.authenticateGuardianToken(req.GetGuardianToken()); !ok {
		return status.Error(codes.PermissionDenied, "invalid guardian token")
	}
	prefixes, err := normalizeGuardianLogicalPrefixes(req.GetLogicalPrefixes())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}

	sub := &guardianLogicalChangeSubscriber{
		id:                 r.guardianLogicalChangeSeq.Add(1),
		logicalPrefixes:    prefixes,
		includeInlineBytes: req.GetIncludeInlineContent(),
		events:             make(chan *pb.GuardianChangeEvent, guardianChangeBufferSize),
	}

	r.guardianLogicalChangeSubsMu.Lock()
	r.guardianLogicalChangeSubs[sub.id] = sub
	r.guardianLogicalChangeSubsMu.Unlock()
	defer func() {
		r.guardianLogicalChangeSubsMu.Lock()
		delete(r.guardianLogicalChangeSubs, sub.id)
		r.guardianLogicalChangeSubsMu.Unlock()
	}()

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case event := <-sub.events:
			if err := stream.Send(event); err != nil {
				return err
			}
		}
	}
}

func (r *Router) applyGuardianUpsertGroups(ctx context.Context, groups map[string]*guardianUpsertGroup, principal *guardianPrincipal) error {
	if len(groups) == 0 {
		return nil
	}
	nodes := r.collectHealthyGuardianNodes()
	if len(nodes) == 0 {
		return fmt.Errorf("no healthy nodes available")
	}

	for _, group := range groups {
		if err := r.applyGuardianUpsertGroup(ctx, nodes, group, principal); err != nil {
			return err
		}
	}
	return nil
}

func (r *Router) applyGuardianUpsertGroup(ctx context.Context, nodes []guardianNodeTarget, group *guardianUpsertGroup, principal *guardianPrincipal) error {
	var (
		wg      sync.WaitGroup
		errMu   sync.Mutex
		errs    []error
		repoURL = group.displayPath
	)
	if principal != nil && strings.TrimSpace(principal.BaseURL) != "" {
		repoURL = principal.BaseURL
	}

	for _, node := range nodes {
		node := node
		wg.Add(1)
		go func() {
			defer wg.Done()

			nodeClient, closeConn, err := r.guardianNodeClient(node)
			if err != nil {
				errMu.Lock()
				errs = append(errs, fmt.Errorf("node %s connect: %w", node.id, err))
				errMu.Unlock()
				return
			}
			defer closeConn()

			regCtx, regCancel := context.WithTimeout(ctx, 10*time.Second)
			_, regErr := nodeClient.RegisterRepository(regCtx, &pb.RegisterRepositoryRequest{
				StorageId:     group.storageID,
				DisplayPath:   group.displayPath,
				Source:        "guardian-path-api",
				IngestionType: pb.IngestionType_INGESTION_GUARDIAN,
				FetchType:     pb.SourceType_SOURCE_TYPE_BLOB,
				GuardianUrl:   repoURL,
			})
			regCancel()
			if regErr != nil {
				r.logger.Warn("RegisterRepository failed for guardian upsert", "node", node.id, "display_path", group.displayPath, "error", regErr)
			}

			batchCtx, batchCancel := context.WithTimeout(ctx, 30*time.Second)
			resp, err := nodeClient.IngestFileBatch(batchCtx, &pb.IngestFileBatchRequest{
				Files:       group.files,
				StorageId:   group.storageID,
				DisplayPath: group.displayPath,
				Source:      "guardian-path-api",
			})
			batchCancel()
			if err != nil {
				errMu.Lock()
				errs = append(errs, fmt.Errorf("node %s ingest %s: %w", node.id, group.displayPath, err))
				errMu.Unlock()
				return
			}
			if resp == nil || !resp.Success {
				message := ""
				if resp != nil {
					message = resp.GetErrorMessage()
				}
				errMu.Lock()
				errs = append(errs, fmt.Errorf("node %s ingest %s failed: %s", node.id, group.displayPath, message))
				errMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		return errs[0]
	}

	r.mu.Lock()
	repoURLValue := repoURL
	if repoURLValue == "" {
		repoURLValue = group.displayPath
	}
	r.ingestedRepos[group.storageID] = &ingestedRepo{
		repoID:     group.displayPath,
		repoURL:    repoURLValue,
		filesCount: int64(len(group.files)),
		ingestedAt: time.Now(),
	}
	r.mu.Unlock()

	return nil
}

func readGuardianFileContent(ctx context.Context, client pb.MonoFSClient, fullPath string) ([]byte, error) {
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	stream, err := client.Read(readCtx, &pb.ReadRequest{
		Path:   fullPath,
		Offset: 0,
		Size:   0,
	})
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		buf.Write(chunk.GetData())
	}
	return buf.Bytes(), nil
}

func buildGuardianChangeEvent(version *pb.GuardianFileVersion, logicalPath string, changeType pb.ChangeType, correlationID string, content []byte) *pb.GuardianChangeEvent {
	event := &pb.GuardianChangeEvent{
		LogicalPath:     logicalPath,
		DisplayPath:     version.GetDisplayPath(),
		StorageId:       version.GetStorageId(),
		Type:            changeType,
		VersionId:       version.GetVersionId(),
		BatchRevisionId: version.GetBatchRevisionId(),
		ContentSha256:   version.GetContentSha256(),
		PrincipalId:     version.GetPrincipalId(),
		CorrelationId:   correlationID,
		CommittedAt:     version.GetCommittedAt(),
	}
	if len(content) > 0 && len(content) < 64*1024 {
		event.InlineContent = append([]byte(nil), content...)
	}
	return event
}

func (r *Router) publishGuardianLogicalChange(event *pb.GuardianChangeEvent) {
	if event == nil {
		return
	}

	r.guardianLogicalChangeSubsMu.RLock()
	defer r.guardianLogicalChangeSubsMu.RUnlock()

	for _, sub := range r.guardianLogicalChangeSubs {
		if !matchesGuardianLogicalPrefixes(event.GetLogicalPath(), sub.logicalPrefixes) {
			continue
		}
		cloned := cloneGuardianLogicalChangeEvent(event)
		if !sub.includeInlineBytes {
			cloned.InlineContent = nil
		}
		select {
		case sub.events <- cloned:
		default:
			r.logger.Warn("dropping guardian logical change event for slow subscriber",
				"subscriber_id", sub.id,
				"logical_path", event.GetLogicalPath())
		}
	}
}

func (r *Router) publishLegacyGuardianChange(event *pb.GuardianChangeEvent) {
	if event == nil {
		return
	}
	mapped, err := mapGuardianLogicalPath(event.GetLogicalPath())
	if err != nil {
		r.logger.Warn("failed to map guardian logical path for legacy event", "logical_path", event.GetLogicalPath(), "error", err)
		return
	}
	r.publishGuardianChange(&pb.ChangeEvent{
		StorageId:     event.GetStorageId(),
		FilePath:      mapped.RelativePath,
		Type:          event.GetType(),
		NewBlobHash:   event.GetContentSha256(),
		InlineContent: append([]byte(nil), event.GetInlineContent()...),
	})
}

func normalizeGuardianLogicalPrefixes(prefixes []string) ([]string, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		cleaned := strings.TrimSpace(prefix)
		if cleaned == "" || cleaned == "/" {
			return nil, nil
		}
		logical, err := normalizeGuardianLogicalPath(cleaned)
		if err != nil {
			return nil, fmt.Errorf("invalid logical prefix %q: %w", prefix, err)
		}
		normalized = append(normalized, logical)
	}
	return normalized, nil
}

func matchesGuardianLogicalPrefixes(logicalPath string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, prefix := range prefixes {
		if prefix == logicalPath ||
			strings.HasPrefix(logicalPath, prefix+"/") ||
			strings.HasPrefix(prefix, logicalPath+"/") {
			return true
		}
	}
	return false
}

func cloneGuardianLogicalChangeEvent(event *pb.GuardianChangeEvent) *pb.GuardianChangeEvent {
	if event == nil {
		return nil
	}
	cloned := *event
	if len(event.InlineContent) > 0 {
		cloned.InlineContent = append([]byte(nil), event.InlineContent...)
	}
	return &cloned
}

func guardianBatchRevisionID(now time.Time) string {
	return fmt.Sprintf("batch_%d", now.UnixNano())
}

func authorizeGuardianMutation(principal *guardianPrincipal, logicalPath string, deleteOp bool) error {
	if principal == nil {
		return fmt.Errorf("guardian principal is required")
	}
	switch principal.Role {
	case "control-plane", "cli":
		return nil
	case "pusher":
		if strings.Contains(logicalPath, "/.state/") {
			return nil
		}
		if !strings.HasPrefix(logicalPath, "/.queues/") {
			return fmt.Errorf("pusher principal %q may only mutate queue and state paths", principal.PrincipalID)
		}
		trimmed := strings.TrimPrefix(logicalPath, "/.queues/")
		parts := strings.Split(trimmed, "/")
		if len(parts) < 2 {
			return fmt.Errorf("pusher principal %q cannot mutate queue root %q", principal.PrincipalID, logicalPath)
		}
		if deleteOp {
			return fmt.Errorf("pusher principal %q may not delete queue paths", principal.PrincipalID)
		}
		if parts[1] != ".claims" && parts[1] != ".results" {
			return fmt.Errorf("pusher principal %q may only write claim or result paths", principal.PrincipalID)
		}
		pusherName := strings.TrimPrefix(principal.PrincipalID, "guardian-pusher-")
		if pusherName != principal.PrincipalID && pusherName != "" && parts[0] != pusherName {
			return fmt.Errorf("pusher principal %q may only mutate its own queue", principal.PrincipalID)
		}
		return nil
	default:
		return fmt.Errorf("guardian principal %q has unsupported role %q", principal.PrincipalID, principal.Role)
	}
}

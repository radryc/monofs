package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ReadGuardianPath returns a consistent content+version pair. The content is
// read from a healthy replica and verified against the authoritative version
// record, so a stale replica cannot be returned as current.
func (r *Router) ReadGuardianPath(ctx context.Context, req *pb.ReadGuardianPathRequest) (*pb.ReadGuardianPathResponse, error) {
	if _, ok := r.authenticateGuardianToken(req.GetGuardianToken()); !ok {
		return nil, status.Error(codes.PermissionDenied, "invalid guardian token")
	}
	logicalPath, err := normalizeGuardianLogicalPath(req.GetLogicalPath())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid logical path %q: %v", req.GetLogicalPath(), err)
	}
	mapped, err := mapGuardianLogicalPath(logicalPath)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid logical path %q: %v", req.GetLogicalPath(), err)
	}

	// Hold the display-path mutation lock so a read cannot observe content
	// between the node apply and the version commit.
	unlockMutations := r.lockGuardianMutations(mapped.DisplayPath)
	defer unlockMutations()

	current, exists, err := r.guardianVersions.currentVersionWithError(logicalPath)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "guardian version store unavailable: %v", err)
	}
	if !exists || current == nil || current.Tombstone {
		recovered, content, ok, recErr := r.recoverGuardianVersionLocked(ctx, mapped)
		if recErr != nil {
			return nil, status.Errorf(codes.Unavailable, "guardian version recovery failed: %v", recErr)
		}
		if !ok {
			return &pb.ReadGuardianPathResponse{Found: false}, nil
		}
		return &pb.ReadGuardianPathResponse{
			Found:   true,
			Content: content,
			Version: recovered.toProto(),
		}, nil
	}

	content, ok := r.readGuardianContentVerified(ctx, mapped, current.ContentSHA256)
	if !ok {
		return nil, status.Errorf(codes.Unavailable, "guardian content for %q does not match version %s", logicalPath, current.VersionID)
	}
	return &pb.ReadGuardianPathResponse{
		Found:   true,
		Content: content,
		Version: current.toProto(),
	}, nil
}

// recoverGuardianVersionLocked synthesizes a version record from node content
// when the authoritative record is missing (for example after a router
// failover with an empty local state directory).
func (r *Router) recoverGuardianVersionLocked(ctx context.Context, mapped guardianPhysicalPath) (*storedGuardianFileVersion, []byte, bool, error) {
	content, ok := r.readGuardianContentVerified(ctx, mapped, "")
	if !ok {
		return nil, nil, false, nil
	}
	sum := sha256.Sum256(content)
	contentSHA := hex.EncodeToString(sum[:])
	now := time.Now()
	version, err := r.guardianVersions.commit(guardianVersionCommit{
		LogicalPath:       mapped.LogicalPath,
		DisplayPath:       mapped.DisplayPath,
		StorageID:         mapped.StorageID,
		Content:           content,
		ExpectedVersionID: "absent",
		PrincipalID:       "monofs-recovery",
		Reason:            "recovered",
		CommittedAt:       now.Unix(),
	})
	if err != nil {
		// Another writer won the race. Re-read and use the winner if it still
		// describes the content we found.
		current, exists, readErr := r.guardianVersions.currentVersionWithError(mapped.LogicalPath)
		if readErr == nil && exists && current != nil && !current.Tombstone &&
			(current.ContentSHA256 == "" || strings.EqualFold(current.ContentSHA256, contentSHA)) {
			guardianVersionStoreRecoveriesTotal.WithLabelValues("conflict").Inc()
			return current, content, true, nil
		}
		return nil, nil, false, err
	}
	stored := &storedGuardianFileVersion{
		LogicalPath:     mapped.LogicalPath,
		DisplayPath:     mapped.DisplayPath,
		StorageID:       mapped.StorageID,
		VersionID:       version.GetVersionId(),
		BatchRevisionID: version.GetBatchRevisionId(),
		ContentSHA256:   version.GetContentSha256(),
		CommittedAt:     version.GetCommittedAt(),
		PrincipalID:     version.GetPrincipalId(),
		Reason:          version.GetReason(),
		Content:         content,
	}
	guardianVersionStoreRecoveriesTotal.WithLabelValues("recovered").Inc()
	return stored, content, true, nil
}

// readGuardianContentVerified reads a path from healthy replicas and returns
// the first content whose SHA-256 matches expectedSHA. An empty expectedSHA
// accepts any successfully read content.
func (r *Router) readGuardianContentVerified(ctx context.Context, mapped guardianPhysicalPath, expectedSHA string) ([]byte, bool) {
	nodes := r.collectHealthyGuardianNodes()
	for _, node := range nodes {
		nodeClient, closeConn, err := r.guardianNodeClient(node)
		if err != nil {
			continue
		}
		content, err := readGuardianFileContent(ctx, nodeClient, mapped.RelativePath)
		closeConn()
		if err != nil {
			continue
		}
		if expectedSHA == "" {
			return content, true
		}
		sum := sha256.Sum256(content)
		if strings.EqualFold(hex.EncodeToString(sum[:]), expectedSHA) {
			return content, true
		}
	}
	return nil, false
}

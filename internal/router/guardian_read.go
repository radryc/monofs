package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errGuardianContentUnreachable reports that no replica could be read. Unlike a
// missing or mismatched file this is transient, so callers should retry instead
// of treating the path as absent.
var errGuardianContentUnreachable = errors.New("guardian content is temporarily unreadable")

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

	content, ok, conclusive := r.readGuardianContentVerified(ctx, mapped, current.ContentSHA256)
	if !ok {
		// The version record and node content disagree (a write applied
		// content but its version commit was lost, or replicas diverged).
		// Serve the content that is actually present together with the current
		// version instead of committing a repair version here. The next CAS
		// write uses that version, applies content, and commits a matching
		// version — healing the pair without churning versions on every read.
		if repairedContent, found, _ := r.readGuardianContentVerified(ctx, mapped, ""); found {
			return &pb.ReadGuardianPathResponse{
				Found:   true,
				Content: repairedContent,
				Version: current.toProto(),
			}, nil
		}
		if !conclusive {
			// At least one replica failed to answer. Do not mistake an outage
			// for a deleted file: report the read as transient so the caller
			// retries.
			return nil, status.Errorf(codes.Unavailable, "guardian content for %q is temporarily unreadable", logicalPath)
		}
		// At least one replica answered and none has the content. The version
		// record is stale; tombstone it so the next CAS write with expected
		// version "absent" can recreate the file instead of conflicting
		// forever on a version that can never be read.
		if err := r.tombstoneMissingGuardianVersionLocked(mapped); err != nil {
			return nil, status.Errorf(codes.Unavailable, "guardian version repair for %q failed: %v", logicalPath, err)
		}
		guardianVersionStoreRecoveriesTotal.WithLabelValues("content-missing").Inc()
		return &pb.ReadGuardianPathResponse{Found: false}, nil
	}
	return &pb.ReadGuardianPathResponse{
		Found:   true,
		Content: content,
		Version: current.toProto(),
	}, nil
}

// tombstoneMissingGuardianVersionLocked retires a version record whose content
// no longer exists on any replica. A tombstone keeps the history but allows the
// next CAS write (expected version "absent") to create the file again.
func (r *Router) tombstoneMissingGuardianVersionLocked(mapped guardianPhysicalPath) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		latest, exists, err := r.guardianVersions.currentVersionWithError(mapped.LogicalPath)
		if err != nil {
			return err
		}
		if !exists || latest == nil || latest.Tombstone {
			// Another writer or reader already retired the record.
			return nil
		}
		_, err = r.guardianVersions.commit(guardianVersionCommit{
			LogicalPath:       mapped.LogicalPath,
			DisplayPath:       mapped.DisplayPath,
			StorageID:         mapped.StorageID,
			ExpectedVersionID: latest.VersionID,
			Tombstone:         true,
			PrincipalID:       "monofs-repair",
			Reason:            "content missing",
			CommittedAt:       time.Now().Unix(),
		})
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, errGuardianVersionConflict) && !errors.Is(err, errGuardianVersionExists) {
			return err
		}
	}
	return lastErr
}

// recoverGuardianVersionLocked synthesizes a version record from node content
// when the authoritative record is missing (for example after a router
// failover with an empty local state directory).
func (r *Router) recoverGuardianVersionLocked(ctx context.Context, mapped guardianPhysicalPath) (*storedGuardianFileVersion, []byte, bool, error) {
	content, ok, conclusive := r.readGuardianContentVerified(ctx, mapped, "")
	if !ok {
		if !conclusive {
			return nil, nil, false, errGuardianContentUnreachable
		}
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
// accepts any successfully read content. The third result reports whether every
// healthy replica answered (content or NotFound). Only then is "no content" a
// conclusive statement about the cluster; a transport failure is retryable.
func (r *Router) readGuardianContentVerified(ctx context.Context, mapped guardianPhysicalPath, expectedSHA string) ([]byte, bool, bool) {
	nodes := r.collectHealthyGuardianNodes()
	answered := 0
	for _, node := range nodes {
		nodeClient, closeConn, err := r.guardianNodeClient(node)
		if err != nil {
			continue
		}
		content, err := readGuardianFileContent(ctx, nodeClient, guardianDisplayPathJoin(mapped.DisplayPath, mapped.RelativePath))
		if err != nil {
			// Older nodes resolve guardian reads relative to the last
			// registered repo; fall back to the relative path for them.
			content, err = readGuardianFileContent(ctx, nodeClient, mapped.RelativePath)
		}
		closeConn()
		if err != nil {
			// A NotFound answer still proves the replica is reachable and does
			// not have the file; only transport failures are inconclusive.
			if status.Code(err) == codes.NotFound {
				answered++
			}
			continue
		}
		answered++
		if expectedSHA == "" {
			return content, true, true
		}
		sum := sha256.Sum256(content)
		if strings.EqualFold(hex.EncodeToString(sum[:]), expectedSHA) {
			return content, true, true
		}
	}
	return nil, false, len(nodes) > 0 && answered == len(nodes)
}

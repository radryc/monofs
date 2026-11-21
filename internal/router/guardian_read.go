package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/router/guardian"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errGuardianContentUnreachable reports that no replica could be read. Unlike a
// missing or mismatched file this is transient, so callers should retry instead
// of treating the path as absent.
var errGuardianContentUnreachable = errors.New("guardian content is temporarily unreadable")

// guardianContentMissingGrace bounds how long a committed version may be
// unreadable before a read retires it. Ingest and cross-node replication are
// routinely a little behind the version commit, and a read can also see a stale
// node view that hides the replica holding the content. Retiring the version on
// the first miss turned that lag into a real delete: the next reader saw an
// absent file, Guardian fell back to "Compiled"/"removed intent", and the state
// only came back when the following reconcile rewrote it. Ten minutes is far
// beyond any plausible ingest or replication window, so a genuine content loss
// is still repaired while a transient miss never is.
// A var (rather than a const) so tests can shrink the window.
var guardianContentMissingGrace = 10 * time.Minute

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

	current, exists, err := r.guardianVersions.CurrentVersionWithError(logicalPath)
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
			Version: recovered.ToProto(),
		}, nil
	}

	content, ok, conclusive := r.readGuardianContentVerified(ctx, mapped, current.ContentSHA256)
	if !ok {
		// The version store keeps recently committed content in a bounded
		// in-memory cache. Prefer it over the nodes only when a replica was
		// unreachable: an investigative read that got a conclusive answer
		// (content present but mismatched, or absent on every replica) must
		// still follow the repair/transient path below.
		if !conclusive && len(current.Content) > 0 {
			sum := sha256.Sum256(current.Content)
			if current.ContentSHA256 == "" || strings.EqualFold(hex.EncodeToString(sum[:]), current.ContentSHA256) {
				guardianVersionStoreRecoveriesTotal.WithLabelValues("content-cache").Inc()
				return &pb.ReadGuardianPathResponse{
					Found:   true,
					Content: current.Content,
					Version: current.ToProto(),
				}, nil
			}
		}
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
				Version: current.ToProto(),
			}, nil
		}
		if !conclusive {
			// At least one replica failed to answer. Do not mistake an outage
			// for a deleted file: report the read as transient so the caller
			// retries.
			return nil, status.Errorf(codes.Unavailable, "guardian content for %q is temporarily unreadable", logicalPath)
		}
		// All reachable replicas answered without the content. That is still
		// usually transient: the write that committed this version may not be
		// visible on every replica yet, or a stale node view may hide the
		// replica that has it. A read must never turn that into a delete, so a
		// recent version is reported as transient and only a version that has
		// been unreadable well past any ingest/replication window is retired.
		if age := time.Since(time.Unix(current.CommittedAt, 0)); age < guardianContentMissingGrace {
			guardianVersionStoreRecoveriesTotal.WithLabelValues("content-missing-transient").Inc()
			return nil, status.Errorf(codes.Unavailable, "guardian content for %q is temporarily unreadable", logicalPath)
		}
		// The version is old and unreadable everywhere. Retire it so the next
		// CAS write with expected version "absent" can recreate the file
		// instead of conflicting forever on a version that can never be read.
		if err := r.tombstoneMissingGuardianVersionLocked(mapped); err != nil {
			return nil, status.Errorf(codes.Unavailable, "guardian version repair for %q failed: %v", logicalPath, err)
		}
		guardianVersionStoreRecoveriesTotal.WithLabelValues("content-missing").Inc()
		return &pb.ReadGuardianPathResponse{Found: false}, nil
	}
	return &pb.ReadGuardianPathResponse{
		Found:   true,
		Content: content,
		Version: current.ToProto(),
	}, nil
}

// tombstoneMissingGuardianVersionLocked retires a version record whose content
// no longer exists on any replica. A tombstone keeps the history but allows the
// next CAS write (expected version "absent") to create the file again.
func (r *Router) tombstoneMissingGuardianVersionLocked(mapped guardianPhysicalPath) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		latest, exists, err := r.guardianVersions.CurrentVersionWithError(mapped.LogicalPath)
		if err != nil {
			return err
		}
		if !exists || latest == nil || latest.Tombstone {
			// Another writer or reader already retired the record.
			return nil
		}
		_, err = r.guardianVersions.Commit(guardian.Commit{
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
		if !errors.Is(err, guardian.ErrConflict) && !errors.Is(err, guardian.ErrExists) {
			return err
		}
	}
	return lastErr
}

// recoverGuardianVersionLocked synthesizes a version record from node content
// when the authoritative record is missing (for example after a router
// failover with an empty local state directory).
func (r *Router) recoverGuardianVersionLocked(ctx context.Context, mapped guardianPhysicalPath) (*guardian.Record, []byte, bool, error) {
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
	version, err := r.guardianVersions.Commit(guardian.Commit{
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
		current, exists, readErr := r.guardianVersions.CurrentVersionWithError(mapped.LogicalPath)
		if readErr == nil && exists && current != nil && !current.Tombstone &&
			(current.ContentSHA256 == "" || strings.EqualFold(current.ContentSHA256, contentSHA)) {
			guardianVersionStoreRecoveriesTotal.WithLabelValues("conflict").Inc()
			return current, content, true, nil
		}
		return nil, nil, false, err
	}
	stored := &guardian.Record{
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

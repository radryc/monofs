package router

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	kvsv1 "github.com/radryc/kvs/api/proto/kvs/v1"
	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/telemetry"
	"github.com/radryc/monofs/pkg/grpcx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	guardianKVSRecordPrefix = "/.monofs/guardian-versions/"
	guardianKVSRetryLimit   = 3
)

// kvsVersionSeq is independent of the local store sequence so KVS version IDs
// remain unique across router restarts.
var kvsVersionSeq atomic.Uint64

func nextKVSVersionSeq() uint64 {
	for {
		current := kvsVersionSeq.Load()
		next := uint64(time.Now().UnixNano())
		if next <= current {
			next = current + 1
		}
		if kvsVersionSeq.CompareAndSwap(current, next) {
			return next
		}
	}
}

type kvsGuardianVersionMeta struct {
	VersionID       string `json:"versionId"`
	DisplayPath     string `json:"displayPath,omitempty"`
	StorageID       string `json:"storageId,omitempty"`
	BatchRevisionID string `json:"batchRevisionId,omitempty"`
	ContentSHA256   string `json:"contentSha256,omitempty"`
	CommittedAt     int64  `json:"committedAt"`
	Tombstone       bool   `json:"tombstone,omitempty"`
	PrincipalID     string `json:"principalId,omitempty"`
	Reason          string `json:"reason,omitempty"`
	CorrelationID   string `json:"correlationId,omitempty"`
}

type kvsGuardianVersionRecord struct {
	LogicalPath string                   `json:"logicalPath"`
	Current     *kvsGuardianVersionMeta  `json:"current,omitempty"`
	History     []kvsGuardianVersionMeta `json:"history"`
}

func (m kvsGuardianVersionMeta) stored() *storedGuardianFileVersion {
	return &storedGuardianFileVersion{
		LogicalPath:     "",
		DisplayPath:     m.DisplayPath,
		StorageID:       m.StorageID,
		VersionID:       m.VersionID,
		BatchRevisionID: m.BatchRevisionID,
		ContentSHA256:   m.ContentSHA256,
		CommittedAt:     m.CommittedAt,
		Tombstone:       m.Tombstone,
		PrincipalID:     m.PrincipalID,
		Reason:          m.Reason,
		CorrelationID:   m.CorrelationID,
	}
}

type kvsGuardianVersionBackend struct {
	router *Router
	store  *guardianVersionStore

	mu      sync.Mutex
	conns   map[string]*grpc.ClientConn
	clients map[string]kvsv1.KVStoreClient
}

type kvsGuardianCandidate struct {
	nodeID  string
	address string
}

func newKVSGuardianVersionBackend(router *Router, store *guardianVersionStore) *kvsGuardianVersionBackend {
	return &kvsGuardianVersionBackend{router: router, store: store}
}

// candidates returns healthy KVS-enabled nodes ordered with the current leader
// first. Every KVS member forwards reads and writes to the leader, so any
// healthy candidate is a valid failover target. Nodes recently marked
// unhealthy or in cooldown are excluded.
func (b *kvsGuardianVersionBackend) candidates() []kvsGuardianCandidate {
	b.router.mu.RLock()
	defer b.router.mu.RUnlock()

	now := time.Now()
	all := make([]kvsGuardianCandidate, 0, len(b.router.nodes))
	kvsNodes := make([]kvsGuardianCandidate, 0, len(b.router.nodes))
	var leader *kvsGuardianCandidate

	for id, state := range b.router.nodes {
		if state == nil || state.info == nil || state.info.Address == "" {
			continue
		}
		if !state.guardianFailedUntil.IsZero() && now.Before(state.guardianFailedUntil) {
			continue
		}
		candidate := kvsGuardianCandidate{nodeID: id, address: state.info.Address}
		all = append(all, candidate)

		status := normalizedKVSNodeStatus(state.kvsStatus)
		if !status.GetEnabled() || !status.GetHealthy() {
			continue
		}
		kvsNodes = append(kvsNodes, candidate)
		if strings.EqualFold(status.GetRole(), "leader") {
			leaderCopy := candidate
			leader = &leaderCopy
		}
	}

	out := kvsNodes
	if len(out) == 0 {
		// KVS status may be unknown right after startup or after a failure.
		// Any healthy cluster node can still forward to the leader, so fall
		// back to every non-cooling node rather than failing the operation.
		out = all
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].nodeID != out[j].nodeID {
			return out[i].nodeID < out[j].nodeID
		}
		return out[i].address < out[j].address
	})
	if leader != nil {
		ordered := make([]kvsGuardianCandidate, 0, len(out))
		ordered = append(ordered, *leader)
		for _, candidate := range out {
			if candidate.nodeID != leader.nodeID {
				ordered = append(ordered, candidate)
			}
		}
		return ordered
	}
	return out
}

func (b *kvsGuardianVersionBackend) clientFor(candidate kvsGuardianCandidate) (kvsv1.KVStoreClient, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if client := b.clients[candidate.address]; client != nil {
		return client, nil
	}
	conn, err := grpc.NewClient(candidate.address, append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpcx.IPv4DialerOption(),
		grpc.WithStatsHandler(telemetry.NewGRPCClientStatsHandler()),
	}, b.router.internalDialOptions()...)...)
	if err != nil {
		return nil, fmt.Errorf("dial KVS node %s: %w", candidate.address, err)
	}
	if b.conns == nil {
		b.conns = make(map[string]*grpc.ClientConn)
	}
	if b.clients == nil {
		b.clients = make(map[string]kvsv1.KVStoreClient)
	}
	client := kvsv1.NewKVStoreClient(conn)
	b.conns[candidate.address] = conn
	b.clients[candidate.address] = client
	return client, nil
}

// withClient runs fn against a healthy KVS node, rotating to a peer on
// transport failures. Non-transport errors (CAS conflicts, not-found) are
// returned unchanged so callers can handle them.
func (b *kvsGuardianVersionBackend) withClient(ctx context.Context, fn func(kvsv1.KVStoreClient) error) error {
	candidates := b.candidates()
	if len(candidates) == 0 {
		return fmt.Errorf("no healthy KVS node available")
	}
	var lastErr error
	for _, candidate := range candidates {
		client, err := b.clientFor(candidate)
		if err != nil {
			lastErr = err
			continue
		}
		err = fn(client)
		if err == nil {
			b.router.clearGuardianNodeFailure(candidate.nodeID)
			return nil
		}
		lastErr = err
		if !isGuardianTransportError(err) {
			return err
		}
		b.router.markGuardianNodeUnhealthy(candidate.nodeID, err.Error())
		b.dropClient(candidate.address)
	}
	return lastErr
}

func guardianKVSRecordPath(logicalPath string) string {
	encoded := base64.RawURLEncoding.EncodeToString([]byte(logicalPath))
	return guardianKVSRecordPrefix + encoded
}

func (b *kvsGuardianVersionBackend) readRecord(ctx context.Context, logicalPath string) (*kvsGuardianVersionRecord, string, bool, error) {
	var resp *kvsv1.ReadFileResponse
	err := b.withClient(ctx, func(client kvsv1.KVStoreClient) error {
		var callErr error
		resp, callErr = client.ReadFile(ctx, &kvsv1.ReadFileRequest{LogicalPath: guardianKVSRecordPath(logicalPath)})
		return callErr
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}

	var record kvsGuardianVersionRecord
	if err := json.Unmarshal(resp.GetContent(), &record); err != nil {
		return nil, "", false, fmt.Errorf("decode guardian version record for %s: %w", logicalPath, err)
	}
	recordVersion := ""
	if resp.GetVersion() != nil {
		recordVersion = resp.GetVersion().GetVersionId()
	}
	return &record, recordVersion, true, nil
}

func (b *kvsGuardianVersionBackend) current(ctx context.Context, logicalPath string) (*storedGuardianFileVersion, bool, error) {
	record, _, exists, err := b.readRecord(ctx, logicalPath)
	if err != nil {
		return nil, false, err
	}
	if !exists || record == nil || record.Current == nil {
		return nil, false, nil
	}
	current := record.Current.stored()
	current.LogicalPath = logicalPath
	return current, true, nil
}

func (b *kvsGuardianVersionBackend) commitBatch(ctx context.Context, changes []guardianVersionCommit) ([]*pb.GuardianFileVersion, error) {
	// Preserve input order for the caller while deduplicating paths for the
	// KVS batch (KVS rejects duplicate paths in one batch).
	type preparedWrite struct {
		index    int
		change   guardianVersionCommit
		record   *kvsGuardianVersionRecord
		expected string
		version  *pb.GuardianFileVersion
	}
	prepared := make([]*preparedWrite, len(changes))
	byPath := make(map[string]*preparedWrite)
	order := make([]string, 0, len(changes))

	for i, change := range changes {
		entry := &preparedWrite{index: i, change: change}
		prepared[i] = entry
		if _, ok := byPath[change.LogicalPath]; !ok {
			order = append(order, change.LogicalPath)
		}
		byPath[change.LogicalPath] = entry
	}

	for attempt := 0; attempt < guardianKVSRetryLimit; attempt++ {
		recordVersions := make(map[string]string, len(order))
		records := make(map[string]*kvsGuardianVersionRecord, len(order))
		for _, logicalPath := range order {
			record, recordVersion, exists, err := b.readRecord(ctx, logicalPath)
			if err != nil {
				return nil, err
			}
			if !exists {
				record = &kvsGuardianVersionRecord{LogicalPath: logicalPath}
			}
			records[logicalPath] = record
			recordVersions[logicalPath] = recordVersion
		}

		// Validate all expected versions before mutating any record.
		for _, logicalPath := range order {
			entry := byPath[logicalPath]
			current := records[logicalPath].Current
			switch entry.change.ExpectedVersionID {
			case "":
			case "absent":
				if current != nil && !current.Tombstone {
					return nil, fmt.Errorf("%w: %s", errGuardianVersionExists, logicalPath)
				}
			default:
				if current == nil || current.VersionID != entry.change.ExpectedVersionID {
					return nil, fmt.Errorf("%w: %s", errGuardianVersionConflict, logicalPath)
				}
			}
		}

		writes := make([]*kvsv1.PathWrite, 0, len(order))
		for _, logicalPath := range order {
			entry := byPath[logicalPath]
			change := entry.change
			contentSHA := ""
			if change.Content != nil {
				sum := sha256.Sum256(change.Content)
				contentSHA = hex.EncodeToString(sum[:])
			}
			seq := nextKVSVersionSeq()
			versionID := guardianVersionID(change.LogicalPath, contentSHA, change.BatchRevisionID, change.CommittedAt, seq, change.Tombstone)
			meta := kvsGuardianVersionMeta{
				VersionID:       versionID,
				DisplayPath:     change.DisplayPath,
				StorageID:       change.StorageID,
				BatchRevisionID: change.BatchRevisionID,
				ContentSHA256:   contentSHA,
				CommittedAt:     change.CommittedAt,
				Tombstone:       change.Tombstone,
				PrincipalID:     change.PrincipalID,
				Reason:          change.Reason,
				CorrelationID:   change.CorrelationID,
			}
			record := records[logicalPath]
			record.LogicalPath = logicalPath
			record.Current = &meta
			record.History = append(record.History, meta)
			if len(record.History) > guardianVersionsPerPathHistory {
				record.History = record.History[len(record.History)-guardianVersionsPerPathHistory:]
			}
			payload, err := json.Marshal(record)
			if err != nil {
				return nil, err
			}
			expected := recordVersions[logicalPath]
			if expected == "" {
				expected = "absent"
			}
			writes = append(writes, &kvsv1.PathWrite{
				LogicalPath:       guardianKVSRecordPath(logicalPath),
				Content:           payload,
				ExpectedVersionId: expected,
			})
			entry.expected = expected
		}

		err := b.withClient(ctx, func(client kvsv1.KVStoreClient) error {
			_, callErr := client.UpsertFiles(ctx, &kvsv1.UpsertFilesRequest{
				Writes: writes,
				Context: &kvsv1.MutationContext{
					PrincipalId:   "monofs-router",
					Reason:        "guardian version record",
					CorrelationId: changes[0].CorrelationID,
				},
			})
			return callErr
		})
		if err != nil {
			if status.Code(err) == codes.Aborted && strings.Contains(err.Error(), "version conflict") {
				continue // retry the whole batch with fresh versions
			}
			if status.Code(err) == codes.Aborted || status.Code(err) == codes.Unavailable {
				if attempt+1 < guardianKVSRetryLimit {
					time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
					continue
				}
			}
			return nil, err
		}

		// Build proto results in the caller's original order.
		versions := make([]*pb.GuardianFileVersion, len(changes))
		for _, logicalPath := range order {
			entry := byPath[logicalPath]
			current := records[logicalPath].Current
			versions[entry.index] = &pb.GuardianFileVersion{
				LogicalPath:     logicalPath,
				DisplayPath:     current.DisplayPath,
				StorageId:       current.StorageID,
				VersionId:       current.VersionID,
				BatchRevisionId: current.BatchRevisionID,
				ContentSha256:   current.ContentSHA256,
				CommittedAt:     current.CommittedAt,
				Tombstone:       current.Tombstone,
				PrincipalId:     current.PrincipalID,
				Reason:          current.Reason,
			}
			b.store.cacheContent(logicalPath, current.VersionID, entry.change.Content)
		}
		// Duplicate paths in the same batch resolve to the same final version.
		for i, entry := range prepared {
			if versions[i] != nil {
				continue
			}
			if winner := byPath[entry.change.LogicalPath]; winner != nil {
				versions[i] = versions[winner.index]
			}
		}
		return versions, nil
	}
	return nil, fmt.Errorf("%w: guardian KVS CAS retries exhausted", errGuardianVersionConflict)
}

func (b *kvsGuardianVersionBackend) list(ctx context.Context, logicalPath string, pageSize int32, pageToken string) ([]*pb.GuardianFileVersion, string, error) {
	record, _, exists, err := b.readRecord(ctx, logicalPath)
	if err != nil {
		return nil, "", err
	}
	if !exists || record == nil {
		return nil, "", nil
	}
	offset := 0
	if pageToken != "" {
		if _, err := fmt.Sscanf(pageToken, "%d", &offset); err != nil || offset < 0 {
			return nil, "", fmt.Errorf("invalid page token %q", pageToken)
		}
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	limit := int(pageSize)
	versions := make([]*pb.GuardianFileVersion, 0, len(record.History))
	for i := len(record.History) - 1; i >= 0; i-- {
		meta := record.History[i]
		stored := meta.stored()
		stored.LogicalPath = logicalPath
		versions = append(versions, stored.toProto())
	}
	if offset >= len(versions) {
		return nil, "", nil
	}
	end := offset + limit
	if end > len(versions) {
		end = len(versions)
	}
	next := ""
	if end < len(versions) {
		next = fmt.Sprintf("%d", end)
	}
	return versions[offset:end], next, nil
}

func (b *kvsGuardianVersionBackend) get(ctx context.Context, logicalPath, versionID string) (*pb.GuardianFileVersion, []byte, bool, bool, error) {
	record, _, exists, err := b.readRecord(ctx, logicalPath)
	if err != nil {
		return nil, nil, false, false, err
	}
	if !exists || record == nil {
		return nil, nil, false, false, nil
	}
	for i := len(record.History) - 1; i >= 0; i-- {
		meta := record.History[i]
		if meta.VersionID != versionID {
			continue
		}
		stored := meta.stored()
		stored.LogicalPath = logicalPath
		content, available := b.store.cachedContent(logicalPath, versionID)
		if meta.Tombstone {
			return stored.toProto(), nil, false, true, nil
		}
		return stored.toProto(), content, available, true, nil
	}
	return nil, nil, false, false, nil
}

// dropClient closes one cached connection after a transport failure. The node
// is already in cooldown, so this only affects the failed address; other
// connections are kept for reuse.
func (b *kvsGuardianVersionBackend) dropClient(address string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if conn := b.conns[address]; conn != nil {
		_ = conn.Close()
	}
	delete(b.conns, address)
	delete(b.clients, address)
}

func (b *kvsGuardianVersionBackend) close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var errs []error
	for address, conn := range b.conns {
		if err := conn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close KVS node %s: %w", address, err))
		}
		delete(b.conns, address)
		delete(b.clients, address)
	}
	return errors.Join(errs...)
}

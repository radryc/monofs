package workspacestore

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/protobuf/proto"
)

type EntityKind string

const (
	KindJob    EntityKind = "JOB"
	KindBundle EntityKind = "BUNDLE"
	KindAudit  EntityKind = "AUDIT"
	KindLedger EntityKind = "LEDGER"
)

type WalOp string

const (
	OpUpsert WalOp = "UPSERT"
	OpInsert WalOp = "INSERT"
)

type WALEntry struct {
	Seq  uint64          `json:"seq"`
	TS   time.Time       `json:"ts"`
	Op   WalOp           `json:"op"`
	Kind EntityKind      `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type BundleMetadata struct {
	BundleID       string   `json:"bundle_id"`
	WorkspaceID    string   `json:"workspace_id"`
	Kind           string   `json:"kind"`
	ByteSize       int64    `json:"byte_size"`
	RepoCount      int32    `json:"repo_count"`
	LocalCommitIDs []string `json:"local_commit_ids"`
	CreatedAtUnix  int64    `json:"created_at_unix"`
	ExpiresAtUnix  int64    `json:"expires_at_unix"`
	DiscardReason  string   `json:"discard_reason,omitempty"`
	JobID          string   `json:"job_id"`
}

type AuditEvent struct {
	WorkspaceID      string `json:"workspace_id"`
	JobID            string `json:"job_id"`
	BundleID         string `json:"bundle_id"`
	LocalCommitID    string `json:"local_commit_id"`
	ActorPrincipalID string `json:"actor_principal_id"`
	Decision         string `json:"decision"`
	ReasonCode       string `json:"reason_code"`
	Timestamp        int64  `json:"timestamp"`
	CorrelationID    string `json:"correlation_id"`
	Seq              uint64 `json:"-"`
}

type Checkpoint struct {
	LastCompactedSeq uint64 `json:"last_compacted_seq"`
}

type compactedSnapshot struct {
	CheckpointSeq uint64            `json:"checkpoint_seq"`
	Jobs          []*jobSnapshot    `json:"jobs"`
	Bundles       []*BundleMetadata `json:"bundles"`
	AuditEvents   []*AuditEvent     `json:"audit_events"`
}

type jobSnapshot struct {
	Data []byte `json:"data"`
}

type StoreConfig struct {
	StateDir                string
	CompactionInterval      time.Duration
	CompactionSizeThreshold int64
	JobRetentionDays        int
	MaxJobsPerWorkspace     int
	LocalRetentionDays      int
	FsyncEnabled            bool
}

func DefaultStoreConfig(stateDir string) StoreConfig {
	return StoreConfig{
		StateDir:                stateDir,
		CompactionInterval:      5 * time.Minute,
		CompactionSizeThreshold: 256 * 1024 * 1024,
		JobRetentionDays:        30,
		MaxJobsPerWorkspace:     1000,
		LocalRetentionDays:      30,
		FsyncEnabled:            false,
	}
}

type jobEntry struct {
	mu  sync.RWMutex
	job *pb.WorkspaceSyncJob
}

func (e *jobEntry) snapshot() *pb.WorkspaceSyncJob {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return proto.Clone(e.job).(*pb.WorkspaceSyncJob)
}

func auditEventsByTimestamp(audit []*AuditEvent) {
	sort.Slice(audit, func(i, j int) bool {
		return audit[i].Timestamp < audit[j].Timestamp
	})
}

func jobIndexKey(jobID string) string {
	return "job:" + jobID
}

func bundleIndexKey(bundleID string) string {
	return "bundle:" + bundleID
}

func auditIndexKey(jobID, timestamp string) string {
	return fmt.Sprintf("audit:%s:%s", jobID, timestamp)
}

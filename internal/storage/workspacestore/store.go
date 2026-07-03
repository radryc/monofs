package workspacestore

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/protobuf/proto"
)

type Store struct {
	cfg    StoreConfig
	logger *slog.Logger

	mu     sync.RWMutex
	nextSeq uint64

	jobs        map[string]*jobEntry
	bundles     map[string]*BundleMetadata
	auditEvents []*AuditEvent

	wal *walWriter

	compactMu   sync.Mutex
	stopCompact chan struct{}
	checkpoint  *Checkpoint
}

func New(cfg StoreConfig, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "workspacestore")

	s := &Store{
		cfg:         cfg,
		logger:      logger,
		jobs:        make(map[string]*jobEntry),
		bundles:     make(map[string]*BundleMetadata),
		auditEvents: make([]*AuditEvent, 0),
		stopCompact: make(chan struct{}),
		nextSeq:     1,
	}

	if cfg.StateDir == "" {
		logger.Info("workspace state dir not configured, operating in memory-only mode")
		return s, nil
	}

	if err := os.MkdirAll(cfg.StateDir, 0755); err != nil {
		return nil, fmt.Errorf("create workspace state dir %s: %w", cfg.StateDir, err)
	}

	if err := s.loadCheckpoint(); err != nil {
		return nil, fmt.Errorf("load checkpoint: %w", err)
	}

	var err error
	s.wal, err = newWALWriter(cfg.StateDir, logger, cfg.FsyncEnabled)
	if err != nil {
		return nil, fmt.Errorf("init WAL: %w", err)
	}

	if err := s.recover(); err != nil {
		return nil, fmt.Errorf("recover workspace state: %w", err)
	}

	go s.compactLoop()

	return s, nil
}

func (s *Store) UpsertJob(job *pb.WorkspaceSyncJob) error {
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}

	s.mu.Lock()
	seq := s.nextSeq
	s.nextSeq++

	cloned := proto.Clone(job).(*pb.WorkspaceSyncJob)
	s.jobs[job.GetJobId()] = &jobEntry{job: cloned}

	walEntry := WALEntry{
		Seq:  seq,
		TS:   time.Now(),
		Op:   OpUpsert,
		Kind: KindJob,
		Data: json.RawMessage(data),
	}
	s.mu.Unlock()

	if s.wal != nil {
		return s.wal.Append(walEntry)
	}
	return nil
}

func (s *Store) GetJob(jobID string) *pb.WorkspaceSyncJob {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.jobs[jobID]
	if !ok {
		return nil
	}
	return entry.snapshot()
}

func (s *Store) ListJobs(filter func(*pb.WorkspaceSyncJob) bool) []*pb.WorkspaceSyncJob {
	s.mu.RLock()
	defer s.mu.RUnlock()

	jobs := make([]*pb.WorkspaceSyncJob, 0, len(s.jobs))
	for _, entry := range s.jobs {
		job := entry.snapshot()
		if filter == nil || filter(job) {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

func (s *Store) JobCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.jobs)
}

func (s *Store) UpsertBundle(b *BundleMetadata) error {
	data, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("marshal bundle: %w", err)
	}

	s.mu.Lock()
	seq := s.nextSeq
	s.nextSeq++

	cp := &BundleMetadata{}
	*cp = *b
	s.bundles[b.BundleID] = cp

	walEntry := WALEntry{
		Seq:  seq,
		TS:   time.Now(),
		Op:   OpUpsert,
		Kind: KindBundle,
		Data: json.RawMessage(data),
	}
	s.mu.Unlock()

	if s.wal != nil {
		return s.wal.Append(walEntry)
	}
	return nil
}

func (s *Store) GetBundle(bundleID string) *BundleMetadata {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bundles[bundleID]
}

func (s *Store) InsertAudit(event *AuditEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal audit event: %w", err)
	}

	s.mu.Lock()
	seq := s.nextSeq
	s.nextSeq++

	cp := &AuditEvent{}
	*cp = *event
	cp.Seq = seq
	s.auditEvents = append(s.auditEvents, cp)

	walEntry := WALEntry{
		Seq:  seq,
		TS:   time.Now(),
		Op:   OpInsert,
		Kind: KindAudit,
		Data: json.RawMessage(data),
	}
	s.mu.Unlock()

	if s.wal != nil {
		return s.wal.Append(walEntry)
	}
	return nil
}

func (s *Store) ListAuditEvents() []*AuditEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*AuditEvent, len(s.auditEvents))
	for i, e := range s.auditEvents {
		cp := &AuditEvent{}
		*cp = *e
		result[i] = cp
	}
	return result
}

func (s *Store) Close() {
	close(s.stopCompact)

	if s.wal != nil {
		if err := s.compact(); err != nil {
			s.logger.Error("final compaction failed", "error", err)
		}
		s.wal.Close()
	}
}

func (s *Store) loadCheckpoint() error {
	checkpointPath := s.statePath("checkpoints", "checkpoint.json")
	data, err := os.ReadFile(checkpointPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	s.checkpoint = &Checkpoint{}
	if err := json.Unmarshal(data, s.checkpoint); err != nil {
		s.logger.Warn("corrupt checkpoint file, ignoring", "error", err)
		s.checkpoint = nil
		return nil
	}
	s.logger.Info("loaded checkpoint", "last_compacted_seq", s.checkpoint.LastCompactedSeq)
	return nil
}

func (s *Store) recover() error {
	fromSeq := uint64(0)
	if s.checkpoint != nil {
		fromSeq = s.checkpoint.LastCompactedSeq
	}

	entries, err := s.wal.ReplayEntries(fromSeq)
	if err != nil {
		return fmt.Errorf("replay WAL: %w", err)
	}

	for _, entry := range entries {
		switch entry.Kind {
		case KindJob:
			if entry.Op != OpUpsert {
				continue
			}
			var job pb.WorkspaceSyncJob
			if err := json.Unmarshal(entry.Data, &job); err != nil {
				s.logger.Warn("skip corrupt job entry", "seq", entry.Seq, "error", err)
				continue
			}
			cloned := proto.Clone(&job).(*pb.WorkspaceSyncJob)
			s.jobs[job.GetJobId()] = &jobEntry{job: cloned}
		case KindBundle:
			if entry.Op != OpUpsert {
				continue
			}
			var b BundleMetadata
			if err := json.Unmarshal(entry.Data, &b); err != nil {
				s.logger.Warn("skip corrupt bundle entry", "seq", entry.Seq, "error", err)
				continue
			}
			s.bundles[b.BundleID] = &b
		case KindAudit:
			if entry.Op != OpInsert {
				continue
			}
			var ae AuditEvent
			if err := json.Unmarshal(entry.Data, &ae); err != nil {
				s.logger.Warn("skip corrupt audit entry", "seq", entry.Seq, "error", err)
				continue
			}
			ae.Seq = entry.Seq
			s.auditEvents = append(s.auditEvents, &ae)
		}
		if entry.Seq >= s.nextSeq {
			s.nextSeq = entry.Seq + 1
		}
	}

	s.logger.Info("workspace state recovered",
		"jobs", len(s.jobs),
		"bundles", len(s.bundles),
		"audit_events", len(s.auditEvents),
		"next_seq", s.nextSeq)
	return nil
}

func (s *Store) compactLoop() {
	ticker := time.NewTicker(s.cfg.CompactionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := s.compact(); err != nil {
				s.logger.Error("compaction failed", "error", err)
			}
		case <-s.stopCompact:
			return
		}
	}
}

func (s *Store) compact() error {
	if s.wal == nil {
		return nil
	}

	s.compactMu.Lock()
	defer s.compactMu.Unlock()

	totalSize := s.wal.TotalSize()
	if totalSize < s.cfg.CompactionSizeThreshold {
		return nil
	}

	s.mu.RLock()
	checkpointSeq := s.nextSeq - 1
	if checkpointSeq < 1 {
		s.mu.RUnlock()
		return nil
	}

	snapshot := &compactedSnapshot{CheckpointSeq: checkpointSeq}

	for _, entry := range s.jobs {
		jobData, err := json.Marshal(entry.snapshot())
		if err != nil {
			s.logger.Warn("skip job in snapshot", "job_id", entry.job.GetJobId(), "error", err)
			continue
		}
		snapshot.Jobs = append(snapshot.Jobs, &jobSnapshot{Data: jobData})
	}

	for _, b := range s.bundles {
		snapshot.Bundles = append(snapshot.Bundles, b)
	}

	snapshot.AuditEvents = s.auditEvents
	s.mu.RUnlock()

	checkpointDir := s.statePath("checkpoints", "")
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		return fmt.Errorf("create checkpoint dir: %w", err)
	}

	checkpointPath := s.statePath("checkpoints", "checkpoint.json")
	chk := &Checkpoint{LastCompactedSeq: checkpointSeq}

	tmpPath := checkpointPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create checkpoint tmp file: %w", err)
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(chk); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write checkpoint: %w", err)
	}
	f.Close()
	if err := os.Rename(tmpPath, checkpointPath); err != nil {
		return fmt.Errorf("rename checkpoint: %w", err)
	}

	if err := s.wal.DeleteSegmentsBelow(checkpointSeq); err != nil {
		s.logger.Error("failed to delete compacted WAL segments", "error", err)
	}

	s.logger.Info("compaction complete",
		"checkpoint_seq", checkpointSeq,
		"wal_size_before", totalSize,
		"jobs_in_snapshot", len(snapshot.Jobs))

	return nil
}

func (s *Store) statePath(parts ...string) string {
	if len(parts) == 0 {
		return s.cfg.StateDir
	}
	result := s.cfg.StateDir
	for _, p := range parts {
		result += "/" + p
	}
	return result
}

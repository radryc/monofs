package workspacestore

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/protobuf/proto"
)

type Store struct {
	cfg    StoreConfig
	logger *slog.Logger

	mu      sync.RWMutex
	nextSeq uint64

	jobs          map[string]*jobEntry
	bundles       map[string]*BundleMetadata
	auditEvents   []*AuditEvent
	ledgerEntries [][]byte

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
		cfg:           cfg,
		logger:        logger,
		jobs:          make(map[string]*jobEntry),
		bundles:       make(map[string]*BundleMetadata),
		auditEvents:   make([]*AuditEvent, 0),
		ledgerEntries: make([][]byte, 0),
		stopCompact:   make(chan struct{}),
		nextSeq:       1,
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

func (s *Store) InsertLedger(data []byte) error {
	s.mu.Lock()
	seq := s.nextSeq
	s.nextSeq++
	s.ledgerEntries = append(s.ledgerEntries, append([]byte(nil), data...))

	walEntry := WALEntry{
		Seq:  seq,
		TS:   time.Now(),
		Op:   OpInsert,
		Kind: KindLedger,
		Data: json.RawMessage(data),
	}
	s.mu.Unlock()

	if s.wal != nil {
		return s.wal.Append(walEntry)
	}
	return nil
}

func (s *Store) ReplayLedgerEntries(callback func([]byte) error) error {
	s.mu.RLock()
	entries := make([][]byte, len(s.ledgerEntries))
	for i := range s.ledgerEntries {
		entries[i] = append([]byte(nil), s.ledgerEntries[i]...)
	}
	s.mu.RUnlock()

	for i, entry := range entries {
		if err := callback(entry); err != nil {
			return fmt.Errorf("replay ledger entry index=%d: %w", i, err)
		}
	}
	return nil
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
		if err := s.loadSnapshot(); err != nil {
			return fmt.Errorf("load snapshot: %w", err)
		}
		if s.nextSeq <= s.checkpoint.LastCompactedSeq {
			s.nextSeq = s.checkpoint.LastCompactedSeq + 1
		}
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
		case KindLedger:
			if entry.Op != OpInsert {
				continue
			}
			s.ledgerEntries = append(s.ledgerEntries, append([]byte(nil), entry.Data...))
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

	snapshot := &compactedSnapshot{
		Version:       1,
		CreatedAtUnix: time.Now().Unix(),
		CheckpointSeq: checkpointSeq,
	}

	for _, entry := range s.jobs {
		jobData, err := json.Marshal(entry.snapshot())
		if err != nil {
			s.logger.Warn("skip job in snapshot", "job_id", entry.job.GetJobId(), "error", err)
			continue
		}
		snapshot.Jobs = append(snapshot.Jobs, &jobSnapshot{Data: jobData})
	}

	for _, b := range s.bundles {
		cp := *b
		cp.LocalCommitIDs = append([]string(nil), b.LocalCommitIDs...)
		snapshot.Bundles = append(snapshot.Bundles, &cp)
	}

	for _, event := range s.auditEvents {
		cp := *event
		snapshot.AuditEvents = append(snapshot.AuditEvents, &cp)
	}
	for _, entry := range s.ledgerEntries {
		snapshot.LedgerEntries = append(snapshot.LedgerEntries, append([]byte(nil), entry...))
	}
	s.mu.RUnlock()

	checkpointDir := s.statePath("checkpoints", "")
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		return fmt.Errorf("create checkpoint dir: %w", err)
	}

	snapshotFile, err := s.writeSnapshot(snapshot)
	if err != nil {
		return fmt.Errorf("write snapshot: %w", err)
	}

	checkpointPath := s.statePath("checkpoints", "checkpoint.json")
	chk := &Checkpoint{LastCompactedSeq: checkpointSeq, SnapshotFile: snapshotFile}

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
		"jobs_in_snapshot", len(snapshot.Jobs),
		"ledger_entries_in_snapshot", len(snapshot.LedgerEntries),
		"snapshot_file", snapshotFile)

	s.cleanupOldSnapshots(snapshotFile)

	return nil
}

func (s *Store) loadSnapshot() error {
	if s.checkpoint == nil || s.checkpoint.LastCompactedSeq == 0 {
		return nil
	}

	snapshotFile := s.checkpoint.SnapshotFile
	if snapshotFile == "" {
		snapshotFile = fmt.Sprintf("snapshot-%020d.json", s.checkpoint.LastCompactedSeq)
	}

	path := s.statePath("checkpoints", snapshotFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read snapshot %s: %w", path, err)
	}

	var snapshot compactedSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("decode snapshot %s: %w", path, err)
	}

	if snapshot.Version != 1 {
		return fmt.Errorf("unsupported snapshot version %d", snapshot.Version)
	}

	s.jobs = make(map[string]*jobEntry, len(snapshot.Jobs))
	for _, js := range snapshot.Jobs {
		var job pb.WorkspaceSyncJob
		if err := json.Unmarshal(js.Data, &job); err != nil {
			s.logger.Warn("skip corrupt job in snapshot", "error", err)
			continue
		}
		cloned := proto.Clone(&job).(*pb.WorkspaceSyncJob)
		s.jobs[job.GetJobId()] = &jobEntry{job: cloned}
	}

	s.bundles = make(map[string]*BundleMetadata, len(snapshot.Bundles))
	for _, b := range snapshot.Bundles {
		cp := *b
		cp.LocalCommitIDs = append([]string(nil), b.LocalCommitIDs...)
		s.bundles[b.BundleID] = &cp
	}

	s.auditEvents = make([]*AuditEvent, 0, len(snapshot.AuditEvents))
	for _, event := range snapshot.AuditEvents {
		cp := *event
		s.auditEvents = append(s.auditEvents, &cp)
	}

	s.ledgerEntries = make([][]byte, 0, len(snapshot.LedgerEntries))
	for _, entry := range snapshot.LedgerEntries {
		s.ledgerEntries = append(s.ledgerEntries, append([]byte(nil), entry...))
	}

	if snapshot.CheckpointSeq >= s.nextSeq {
		s.nextSeq = snapshot.CheckpointSeq + 1
	}

	s.logger.Info("loaded snapshot",
		"file", snapshotFile,
		"checkpoint_seq", snapshot.CheckpointSeq,
		"jobs", len(s.jobs),
		"bundles", len(s.bundles),
		"audit_events", len(s.auditEvents),
		"ledger_entries", len(s.ledgerEntries))

	return nil
}

func (s *Store) writeSnapshot(snapshot *compactedSnapshot) (string, error) {
	snapshotFile := fmt.Sprintf("snapshot-%020d.json", snapshot.CheckpointSeq)
	path := s.statePath("checkpoints", snapshotFile)
	tmpPath := path + ".tmp"

	f, err := os.Create(tmpPath)
	if err != nil {
		return "", err
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(snapshot); err != nil {
		f.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}

	return snapshotFile, nil
}

func (s *Store) cleanupOldSnapshots(currentSnapshotFile string) {
	pattern := filepath.Join(s.statePath("checkpoints", ""), "snapshot-*.json")
	files, err := filepath.Glob(pattern)
	if err != nil {
		s.logger.Warn("failed to enumerate snapshots", "error", err)
		return
	}
	for _, file := range files {
		if filepath.Base(file) == currentSnapshotFile {
			continue
		}
		if err := os.Remove(file); err != nil {
			s.logger.Warn("failed to remove old snapshot", "file", file, "error", err)
		}
	}
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

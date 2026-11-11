package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/metastore"
)

const (
	emptyContentSHA256             = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	guardianVersionsCurrentBucket  = "current"
	guardianVersionsHistoryBucket  = "history"
	guardianVersionsMetaBucket     = "meta"
	guardianVersionsSeqKey         = "seq"
	guardianVersionsContentLimit   = 64 << 20
	guardianVersionsPerPathHistory = 50
)

type storedGuardianFileVersion struct {
	LogicalPath     string `json:"logical_path"`
	DisplayPath     string `json:"display_path"`
	StorageID       string `json:"storage_id"`
	VersionID       string `json:"version_id"`
	BatchRevisionID string `json:"batch_revision_id"`
	ContentSHA256   string `json:"content_sha256"`
	CommittedAt     int64  `json:"committed_at"`
	Tombstone       bool   `json:"tombstone"`
	PrincipalID     string `json:"principal_id"`
	Reason          string `json:"reason"`
	CorrelationID   string `json:"correlation_id,omitempty"`
	// Content is kept only in a bounded in-memory cache; it is never written
	// to the persistent store.
	Content []byte `json:"-"`
	// seq is the store sequence assigned to this record. It is not persisted.
	seq uint64
}

type legacyGuardianVersionSnapshot struct {
	Records map[string][]*storedGuardianFileVersion `json:"records"`
	Current map[string]string                       `json:"current"`
}

type guardianVersionCommit struct {
	LogicalPath       string
	DisplayPath       string
	StorageID         string
	BatchRevisionID   string
	PrincipalID       string
	Reason            string
	CorrelationID     string
	Content           []byte
	ExpectedVersionID string
	Tombstone         bool
	CommittedAt       int64
}

var (
	errGuardianVersionConflict = errors.New("guardian version conflict")
	errGuardianVersionExists   = errors.New("guardian version already exists")
)

// validateExpectedVersionLocked checks an expected guardian version ID against
// the current record. Callers must hold s.mu (or the equivalent backend lock).
func (s *guardianVersionStore) validateExpectedVersionLocked(logicalPath, expected string) error {
	switch expected {
	case "":
		return nil
	case "absent":
		current, exists := s.currentRecordForCASLocked(logicalPath)
		if exists && current != nil && !current.Tombstone {
			return errGuardianVersionExists
		}
		return nil
	default:
		current, exists := s.currentRecordForCASLocked(logicalPath)
		if !exists || current == nil || current.VersionID != expected {
			return errGuardianVersionConflict
		}
		return nil
	}
}

// currentRecordForCASLocked resolves the current record without re-acquiring
// s.mu. Callers must hold s.mu.
func (s *guardianVersionStore) currentRecordForCASLocked(logicalPath string) (*storedGuardianFileVersion, bool) {
	if record, ok := s.ephemeral[logicalPath]; ok && record != nil {
		return record, true
	}
	if !s.persistent() {
		return s.currentRecordLocked(logicalPath)
	}
	var record storedGuardianFileVersion
	err := s.db.View(func(tx metastore.Tx) error {
		value, getErr := tx.Get(guardianVersionsCurrentBucket, []byte(logicalPath))
		if getErr != nil {
			return getErr
		}
		return json.Unmarshal(value, &record)
	})
	if err != nil {
		return nil, false
	}
	return &record, true
}

type guardianVersionStore struct {
	mu         sync.RWMutex
	records    map[string][]*storedGuardianFileVersion // in-memory mode only
	current    map[string]string                       // in-memory mode only
	ephemeral  map[string]*storedGuardianFileVersion   // machinery paths, never persisted
	seq        uint64
	db         *metastore.Store
	legacyPath string

	contentMu    sync.RWMutex
	contentCache map[string][]byte
	contentQueue []string
	contentBytes int64

	kvs              *kvsGuardianVersionBackend
	kvsFallbackLocal bool

	writeMu       sync.Mutex // serializes persistent commits without blocking readers
	historyCounts map[string]int
}

func newGuardianVersionStore(stateDir string) (*guardianVersionStore, error) {
	store := &guardianVersionStore{
		records:       make(map[string][]*storedGuardianFileVersion),
		current:       make(map[string]string),
		ephemeral:     make(map[string]*storedGuardianFileVersion),
		contentCache:  make(map[string][]byte),
		historyCounts: make(map[string]int),
	}
	if strings.TrimSpace(stateDir) == "" {
		return store, nil
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create guardian state dir: %w", err)
	}
	db, err := metastore.Open(filepath.Join(stateDir, "guardian_versions.db"), metastore.Options{
		CacheSize:    32 << 20,
		MemTableSize: 16 << 20,
	})
	if err != nil {
		return nil, fmt.Errorf("open guardian versions store: %w", err)
	}
	store.db = db
	store.legacyPath = filepath.Join(stateDir, "guardian_versions.json")
	if err := store.load(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.importLegacySnapshot(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// importLegacySnapshot performs a one-time import of the old whole-file JSON
// snapshot. It is a no-op once the new store has any sequence state.
func (s *guardianVersionStore) importLegacySnapshot() error {
	if !s.persistent() || s.legacyPath == "" || s.seq != 0 {
		return nil
	}
	data, err := os.ReadFile(s.legacyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read legacy guardian versions: %w", err)
	}
	var snapshot legacyGuardianVersionSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("decode legacy guardian versions: %w", err)
	}
	if len(snapshot.Records) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.db.Update(func(tx metastore.Tx) error {
		for logicalPath, records := range snapshot.Records {
			if !isGuardianAssetPath(logicalPath) {
				// Older snapshots may have persisted machinery paths; they are
				// intentionally memory-only now.
				continue
			}
			var currentID string
			for _, record := range records {
				if record == nil {
					continue
				}
				s.seq++
				if record.VersionID == "" {
					record.VersionID = guardianVersionID(record.LogicalPath, record.ContentSHA256, record.BatchRevisionID, record.CommittedAt, s.seq, record.Tombstone)
				}
				value, marshalErr := json.Marshal(record)
				if marshalErr != nil {
					return marshalErr
				}
				if err := tx.Put(guardianVersionsHistoryBucket, guardianVersionHistoryKey(logicalPath, s.seq), value, 0); err != nil {
					return err
				}
				currentID = record.VersionID
			}
			if wanted := snapshot.Current[logicalPath]; wanted != "" {
				for _, record := range records {
					if record != nil && record.VersionID == wanted {
						currentID = wanted
						break
					}
				}
			}
			if currentID == "" {
				continue
			}
			for _, record := range records {
				if record == nil || record.VersionID != currentID {
					continue
				}
				value, marshalErr := json.Marshal(record)
				if marshalErr != nil {
					return marshalErr
				}
				if err := tx.Put(guardianVersionsCurrentBucket, []byte(logicalPath), value, 0); err != nil {
					return err
				}
				break
			}
		}
		return tx.Put(guardianVersionsMetaBucket, []byte(guardianVersionsSeqKey), []byte(strconv.FormatUint(s.seq, 10)), 0)
	})
	if err != nil {
		return fmt.Errorf("import legacy guardian versions: %w", err)
	}
	_ = os.Rename(s.legacyPath, s.legacyPath+".migrated")
	return nil
}

func (s *guardianVersionStore) persistent() bool { return s != nil && s.db != nil }

func (s *guardianVersionStore) kvsBackend() *kvsGuardianVersionBackend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.kvs
}

// nextSeq allocates a monotonic sequence number used to build version IDs.
func (s *guardianVersionStore) nextSeq() uint64 {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	return seq
}

func (s *guardianVersionStore) load() error {
	if !s.persistent() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.View(func(tx metastore.Tx) error {
		value, err := tx.Get(guardianVersionsMetaBucket, []byte(guardianVersionsSeqKey))
		if err == nil {
			if seq, parseErr := strconv.ParseUint(string(value), 10, 64); parseErr == nil {
				s.seq = seq
			}
		}
		return nil
	})
}

func (s *guardianVersionStore) close() {
	if s == nil {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	kvs := s.kvs
	s.kvs = nil
	db := s.db
	s.db = nil
	s.mu.Unlock()
	if kvs != nil {
		_ = kvs.close()
	}
	if db != nil {
		_ = db.Close()
	}
}

// isGuardianAssetPath reports whether logicalPath is a user-managed asset
// (intent YAML, partition config, etc.) rather than guardian's internal
// machinery (task queue files, claim files, result files, archive logs).
//
// Partition state snapshots and the registry/access control-plane documents
// are special: every Guardian replica reads and writes them, so their version
// records must live in the shared KVS backend. Keeping them router-local makes
// a read that lands on the other router see a different version, which makes
// every optimistic CAS update fail.
func isGuardianAssetPath(logicalPath string) bool {
	if strings.HasPrefix(logicalPath, "/.queues/") || strings.HasPrefix(logicalPath, "/.archive/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(logicalPath, "/"), "/")
	if len(parts) > 0 && (parts[0] == ".registry" || parts[0] == ".access") {
		return true
	}
	for i, seg := range parts {
		if len(seg) == 0 || seg[0] != '.' {
			continue
		}
		if seg == ".state" && i == 2 && parts[0] == "partitions" {
			continue
		}
		return false
	}
	return true
}

func cloneStoredVersion(record *storedGuardianFileVersion) *storedGuardianFileVersion {
	if record == nil {
		return nil
	}
	cloned := *record
	cloned.Content = nil
	return &cloned
}

func (s *guardianVersionStore) currentVersion(logicalPath string) (*storedGuardianFileVersion, bool) {
	current, ok, _ := s.currentVersionWithError(logicalPath)
	return current, ok
}

func (s *guardianVersionStore) currentVersionWithError(logicalPath string) (*storedGuardianFileVersion, bool, error) {
	if backend := s.kvsBackend(); backend != nil && isGuardianAssetPath(logicalPath) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		current, ok, err := backend.current(ctx, logicalPath)
		cancel()
		if err == nil {
			if !ok || current == nil {
				return nil, false, nil
			}
			if !current.Tombstone {
				if content, cached := s.cachedContent(logicalPath, current.VersionID); cached {
					current.Content = content
				}
			}
			return current, true, nil
		}
		if !s.kvsFallbackLocal {
			return nil, false, err
		}
	}
	return s.currentVersionLocal(logicalPath)
}

func (s *guardianVersionStore) currentVersionLocal(logicalPath string) (*storedGuardianFileVersion, bool, error) {
	s.mu.RLock()
	if rec, ok := s.ephemeral[logicalPath]; ok {
		cloned := *rec
		cloned.Content = append([]byte(nil), rec.Content...)
		s.mu.RUnlock()
		return &cloned, true, nil
	}
	if !s.persistent() {
		versionID, ok := s.current[logicalPath]
		if !ok {
			s.mu.RUnlock()
			return nil, false, nil
		}
		for i := len(s.records[logicalPath]) - 1; i >= 0; i-- {
			record := s.records[logicalPath][i]
			if record != nil && record.VersionID == versionID {
				cloned := *record
				s.mu.RUnlock()
				return &cloned, true, nil
			}
		}
		s.mu.RUnlock()
		return nil, false, nil
	}
	db := s.db
	s.mu.RUnlock()

	var record storedGuardianFileVersion
	err := db.View(func(tx metastore.Tx) error {
		value, getErr := tx.Get(guardianVersionsCurrentBucket, []byte(logicalPath))
		if getErr != nil {
			return getErr
		}
		return json.Unmarshal(value, &record)
	})
	if err != nil {
		return nil, false, nil
	}
	cloned := cloneStoredVersion(&record)
	if !record.Tombstone {
		if content, ok := s.cachedContent(logicalPath, record.VersionID); ok {
			cloned.Content = content
		}
	}
	return cloned, true, nil
}

func (s *guardianVersionStore) commit(change guardianVersionCommit) (*pb.GuardianFileVersion, error) {
	versions, err := s.commitBatch([]guardianVersionCommit{change})
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, nil
	}
	return versions[0], nil
}

// commitBatch records a batch of mutations. Asset versions are written in one
// metadata transaction, so injecting a partition with many files does not
// produce one transaction and one history-trim scan per file.
func (s *guardianVersionStore) commitBatch(changes []guardianVersionCommit) ([]*pb.GuardianFileVersion, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	backend := s.kvsBackend()
	if backend == nil {
		return s.commitBatchLocal(changes)
	}

	kvsChanges := make([]guardianVersionCommit, 0, len(changes))
	kvsIndexes := make([]int, 0, len(changes))
	localChanges := make([]guardianVersionCommit, 0, len(changes))
	localIndexes := make([]int, 0, len(changes))
	for i, change := range changes {
		if isGuardianAssetPath(change.LogicalPath) {
			kvsChanges = append(kvsChanges, change)
			kvsIndexes = append(kvsIndexes, i)
		} else {
			localChanges = append(localChanges, change)
			localIndexes = append(localIndexes, i)
		}
	}

	versions := make([]*pb.GuardianFileVersion, len(changes))
	if len(kvsChanges) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		kvsVersions, err := backend.commitBatch(ctx, kvsChanges)
		cancel()
		if err != nil {
			if !s.kvsFallbackLocal {
				return nil, err
			}
			localChanges = append(localChanges, kvsChanges...)
			localIndexes = append(localIndexes, kvsIndexes...)
		} else {
			for i, idx := range kvsIndexes {
				versions[idx] = kvsVersions[i]
			}
		}
	}
	if len(localChanges) > 0 {
		localVersions, err := s.commitBatchLocal(localChanges)
		if err != nil {
			return nil, err
		}
		for i, idx := range localIndexes {
			if versions[idx] == nil {
				versions[idx] = localVersions[i]
			}
		}
	}
	return versions, nil
}

func (s *guardianVersionStore) commitBatchLocal(changes []guardianVersionCommit) ([]*pb.GuardianFileVersion, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	if s.persistent() {
		return s.commitBatchPersistent(changes)
	}
	return s.commitBatchMemory(changes)
}

func (s *guardianVersionStore) commitBatchMemory(changes []guardianVersionCommit) ([]*pb.GuardianFileVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	versions := make([]*pb.GuardianFileVersion, len(changes))
	for i, change := range changes {
		if err := s.validateExpectedVersionLocked(change.LogicalPath, change.ExpectedVersionID); err != nil {
			return nil, fmt.Errorf("%w: %s", err, change.LogicalPath)
		}
		contentSHA := ""
		if change.Content != nil {
			sum := sha256.Sum256(change.Content)
			contentSHA = hex.EncodeToString(sum[:])
		}
		s.seq++
		record := &storedGuardianFileVersion{
			LogicalPath:     change.LogicalPath,
			DisplayPath:     change.DisplayPath,
			StorageID:       change.StorageID,
			VersionID:       guardianVersionID(change.LogicalPath, contentSHA, change.BatchRevisionID, change.CommittedAt, s.seq, change.Tombstone),
			BatchRevisionID: change.BatchRevisionID,
			ContentSHA256:   contentSHA,
			CommittedAt:     change.CommittedAt,
			Tombstone:       change.Tombstone,
			PrincipalID:     change.PrincipalID,
			Reason:          change.Reason,
			CorrelationID:   change.CorrelationID,
			seq:             s.seq,
		}
		versions[i] = record.toProto()

		if !isGuardianAssetPath(change.LogicalPath) {
			if change.Tombstone {
				delete(s.ephemeral, change.LogicalPath)
			} else {
				record.Content = append([]byte(nil), change.Content...)
				s.ephemeral[change.LogicalPath] = record
			}
			continue
		}

		record.Content = append([]byte(nil), change.Content...)
		s.records[change.LogicalPath] = append(s.records[change.LogicalPath], record)
		if len(s.records[change.LogicalPath]) > guardianVersionsPerPathHistory {
			s.records[change.LogicalPath] = s.records[change.LogicalPath][len(s.records[change.LogicalPath])-guardianVersionsPerPathHistory:]
		}
		s.current[change.LogicalPath] = record.VersionID
	}
	return versions, nil
}

type preparedVersionChange struct {
	change guardianVersionCommit
	record *storedGuardianFileVersion
}

func (s *guardianVersionStore) commitBatchPersistent(changes []guardianVersionCommit) ([]*pb.GuardianFileVersion, error) {
	// writeMu serializes persistent commits. s.mu is only held while assigning
	// sequence numbers and updating ephemeral/in-memory state, so readers are
	// not blocked by the Pebble transaction.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	versions := make([]*pb.GuardianFileVersion, len(changes))
	prepared := make([]preparedVersionChange, 0, len(changes))
	assetPaths := make(map[string]struct{}, len(changes))
	addedCounts := make(map[string]int, len(changes))
	for i, change := range changes {
		contentSHA := ""
		if change.Content != nil {
			sum := sha256.Sum256(change.Content)
			contentSHA = hex.EncodeToString(sum[:])
		}
		if !isGuardianAssetPath(change.LogicalPath) {
			if err := s.validateExpectedVersionLocked(change.LogicalPath, change.ExpectedVersionID); err != nil {
				s.mu.Unlock()
				return nil, fmt.Errorf("%w: %s", err, change.LogicalPath)
			}
			s.seq++
			record := &storedGuardianFileVersion{
				LogicalPath:     change.LogicalPath,
				DisplayPath:     change.DisplayPath,
				StorageID:       change.StorageID,
				VersionID:       guardianVersionID(change.LogicalPath, contentSHA, change.BatchRevisionID, change.CommittedAt, s.seq, change.Tombstone),
				BatchRevisionID: change.BatchRevisionID,
				ContentSHA256:   contentSHA,
				CommittedAt:     change.CommittedAt,
				Tombstone:       change.Tombstone,
				PrincipalID:     change.PrincipalID,
				Reason:          change.Reason,
				CorrelationID:   change.CorrelationID,
				seq:             s.seq,
			}
			versions[i] = record.toProto()
			if change.Tombstone {
				delete(s.ephemeral, change.LogicalPath)
			} else {
				record.Content = append([]byte(nil), change.Content...)
				s.ephemeral[change.LogicalPath] = record
			}
			continue
		}
		s.seq++
		record := &storedGuardianFileVersion{
			LogicalPath:     change.LogicalPath,
			DisplayPath:     change.DisplayPath,
			StorageID:       change.StorageID,
			VersionID:       guardianVersionID(change.LogicalPath, contentSHA, change.BatchRevisionID, change.CommittedAt, s.seq, change.Tombstone),
			BatchRevisionID: change.BatchRevisionID,
			ContentSHA256:   contentSHA,
			CommittedAt:     change.CommittedAt,
			Tombstone:       change.Tombstone,
			PrincipalID:     change.PrincipalID,
			Reason:          change.Reason,
			CorrelationID:   change.CorrelationID,
			seq:             s.seq,
		}
		versions[i] = record.toProto()
		prepared = append(prepared, preparedVersionChange{change: change, record: record})
		assetPaths[change.LogicalPath] = struct{}{}
		addedCounts[change.LogicalPath]++
	}
	seq := s.seq
	s.mu.Unlock()

	if len(prepared) == 0 {
		return versions, nil
	}

	newCounts := make(map[string]int, len(assetPaths))
	err := s.db.Update(func(tx metastore.Tx) error {
		for _, item := range prepared {
			if err := s.validateExpectedVersionTx(tx, item.change.LogicalPath, item.change.ExpectedVersionID); err != nil {
				return fmt.Errorf("%w: %s", err, item.change.LogicalPath)
			}
			value, err := json.Marshal(item.record)
			if err != nil {
				return err
			}
			routerGuardianVersionStoreWriteBytesTotal.Add(float64(len(value)))
			if err := tx.Put(guardianVersionsCurrentBucket, []byte(item.record.LogicalPath), value, 0); err != nil {
				return err
			}
			if err := tx.Put(guardianVersionsHistoryBucket, guardianVersionHistoryKey(item.record.LogicalPath, item.record.seq), value, 0); err != nil {
				return err
			}
		}
		if err := tx.Put(guardianVersionsMetaBucket, []byte(guardianVersionsSeqKey), []byte(strconv.FormatUint(seq, 10)), 0); err != nil {
			return err
		}
		for logicalPath := range assetPaths {
			count, known := s.historyCounts[logicalPath]
			var keys [][]byte
			if !known {
				if err := tx.ScanPrefix(guardianVersionsHistoryBucket, []byte(logicalPath+"\x00"), func(key, _ []byte) error {
					keys = append(keys, append([]byte(nil), key...))
					return nil
				}); err != nil {
					return err
				}
				count = len(keys)
			} else {
				count += addedCounts[logicalPath]
			}
			if count > guardianVersionsPerPathHistory {
				if keys == nil {
					if err := tx.ScanPrefix(guardianVersionsHistoryBucket, []byte(logicalPath+"\x00"), func(key, _ []byte) error {
						keys = append(keys, append([]byte(nil), key...))
						return nil
					}); err != nil {
						return err
					}
				}
				for _, key := range keys[guardianVersionsPerPathHistory:] {
					if err := tx.Delete(guardianVersionsHistoryBucket, key); err != nil && err != metastore.ErrKeyNotFound {
						return err
					}
				}
				count = guardianVersionsPerPathHistory
			}
			newCounts[logicalPath] = count
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for logicalPath, count := range newCounts {
		s.historyCounts[logicalPath] = count
	}
	for i, change := range changes {
		if isGuardianAssetPath(change.LogicalPath) {
			s.cacheContent(change.LogicalPath, versions[i].GetVersionId(), change.Content)
		}
	}
	return versions, nil
}

// validateExpectedVersionTx validates against the transaction's view (the
// committed snapshot plus this transaction's pending writes).
func (s *guardianVersionStore) validateExpectedVersionTx(tx metastore.Tx, logicalPath, expected string) error {
	if expected == "" {
		return nil
	}
	value, err := tx.Get(guardianVersionsCurrentBucket, []byte(logicalPath))
	exists := err == nil
	if err != nil && !errors.Is(err, metastore.ErrKeyNotFound) {
		return err
	}
	var current storedGuardianFileVersion
	if exists {
		if unmarshalErr := json.Unmarshal(value, &current); unmarshalErr != nil {
			return unmarshalErr
		}
	}
	if expected == "absent" {
		if exists && !current.Tombstone {
			return errGuardianVersionExists
		}
		return nil
	}
	if !exists || current.VersionID != expected {
		return errGuardianVersionConflict
	}
	return nil
}

func guardianVersionHistoryKey(logicalPath string, seq uint64) []byte {
	// Invert seq so a prefix scan returns newest versions first.
	inverted := ^uint64(0) - seq
	return []byte(fmt.Sprintf("%s\x00%020d", logicalPath, inverted))
}

func (s *guardianVersionStore) list(logicalPath string, pageSize int32, pageToken string) ([]*pb.GuardianFileVersion, string, error) {
	if backend := s.kvsBackend(); backend != nil && isGuardianAssetPath(logicalPath) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		versions, next, err := backend.list(ctx, logicalPath, pageSize, pageToken)
		cancel()
		if err == nil {
			return versions, next, nil
		}
		if !s.kvsFallbackLocal {
			return nil, "", err
		}
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	offset := 0
	if strings.TrimSpace(pageToken) != "" {
		value, err := strconv.Atoi(pageToken)
		if err != nil || value < 0 {
			return nil, "", fmt.Errorf("invalid page token %q", pageToken)
		}
		offset = value
	}

	s.mu.RLock()
	if record, ok := s.ephemeral[logicalPath]; ok && record != nil {
		s.mu.RUnlock()
		if offset > 0 {
			return nil, "", nil
		}
		return []*pb.GuardianFileVersion{record.toProto()}, "", nil
	}
	if !s.persistent() {
		source := s.records[logicalPath]
		s.mu.RUnlock()
		if offset >= len(source) {
			return nil, "", nil
		}
		newestFirst := make([]*pb.GuardianFileVersion, 0, len(source))
		for i := len(source) - 1; i >= 0; i-- {
			if source[i] != nil {
				newestFirst = append(newestFirst, source[i].toProto())
			}
		}
		if offset >= len(newestFirst) {
			return nil, "", nil
		}
		end := offset + int(pageSize)
		if end > len(newestFirst) {
			end = len(newestFirst)
		}
		next := ""
		if end < len(newestFirst) {
			next = strconv.Itoa(end)
		}
		return newestFirst[offset:end], next, nil
	}
	db := s.db
	s.mu.RUnlock()

	var versions []*pb.GuardianFileVersion
	hasMore := false
	err := db.View(func(tx metastore.Tx) error {
		seen := 0
		return tx.ScanPrefix(guardianVersionsHistoryBucket, []byte(logicalPath+"\x00"), func(_, value []byte) error {
			if seen < offset {
				seen++
				return nil
			}
			if len(versions) >= int(pageSize) {
				hasMore = true
				return metastore.ErrStopIteration
			}
			var record storedGuardianFileVersion
			if json.Unmarshal(value, &record) == nil {
				versions = append(versions, record.toProto())
			}
			seen++
			return nil
		})
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if hasMore {
		next = strconv.Itoa(offset + len(versions))
	}
	return versions, next, nil
}

func (s *guardianVersionStore) listCurrentUnderPrefix(prefix string, limit int32) []*pb.GuardianFileVersion {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if limit <= 0 {
		limit = 1000
	}

	s.mu.RLock()
	ephemeral := make([]*pb.GuardianFileVersion, 0)
	for logicalPath, record := range s.ephemeral {
		if record == nil || record.Tombstone || !strings.HasPrefix(logicalPath, prefix) {
			continue
		}
		ephemeral = append(ephemeral, record.toProto())
	}
	if !s.persistent() {
		seen := make(map[string]struct{}, len(s.current))
		result := make([]*pb.GuardianFileVersion, 0)
		add := func(logicalPath string) {
			if !strings.HasPrefix(logicalPath, prefix) {
				return
			}
			if _, ok := seen[logicalPath]; ok {
				return
			}
			record, ok := s.currentRecordLocked(logicalPath)
			if !ok || record == nil || record.Tombstone {
				return
			}
			seen[logicalPath] = struct{}{}
			result = append(result, record.toProto())
		}
		for logicalPath := range s.current {
			add(logicalPath)
		}
		s.mu.RUnlock()
		result = append(result, ephemeral...)
		sort.Slice(result, func(i, j int) bool { return result[i].GetLogicalPath() < result[j].GetLogicalPath() })
		if len(result) > int(limit) {
			result = result[:limit]
		}
		return result
	}
	db := s.db
	s.mu.RUnlock()

	result := make([]*pb.GuardianFileVersion, 0, len(ephemeral))
	result = append(result, ephemeral...)
	_ = db.View(func(tx metastore.Tx) error {
		return tx.ScanPrefix(guardianVersionsCurrentBucket, []byte(prefix), func(_, value []byte) error {
			if len(result) >= int(limit) {
				return metastore.ErrStopIteration
			}
			var record storedGuardianFileVersion
			if json.Unmarshal(value, &record) != nil || record.Tombstone {
				return nil
			}
			result = append(result, record.toProto())
			return nil
		})
	})
	sort.Slice(result, func(i, j int) bool { return result[i].GetLogicalPath() < result[j].GetLogicalPath() })
	if len(result) > int(limit) {
		result = result[:limit]
	}
	return result
}

// currentRecordLocked resolves the current record without re-acquiring the
// in-memory store lock. Callers must hold s.mu for reading.
func (s *guardianVersionStore) currentRecordLocked(logicalPath string) (*storedGuardianFileVersion, bool) {
	if record, ok := s.ephemeral[logicalPath]; ok && record != nil {
		return record, true
	}
	versionID, ok := s.current[logicalPath]
	if !ok {
		return nil, false
	}
	records := s.records[logicalPath]
	for i := len(records) - 1; i >= 0; i-- {
		if records[i] != nil && records[i].VersionID == versionID {
			return records[i], true
		}
	}
	return nil, false
}

// get returns the version metadata, its cached content (when available), a
// flag reporting whether content is actually available, and whether the
// version exists at all.
func (s *guardianVersionStore) get(logicalPath, versionID string) (*pb.GuardianFileVersion, []byte, bool, bool, error) {
	if backend := s.kvsBackend(); backend != nil && isGuardianAssetPath(logicalPath) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		version, content, available, found, err := backend.get(ctx, logicalPath, versionID)
		cancel()
		if err == nil {
			return version, content, available, found, nil
		}
		if !s.kvsFallbackLocal {
			return nil, nil, false, false, err
		}
	}
	s.mu.RLock()
	if record, ok := s.ephemeral[logicalPath]; ok && record != nil && record.VersionID == versionID {
		proto := record.toProto()
		content := append([]byte(nil), record.Content...)
		s.mu.RUnlock()
		return proto, content, !record.Tombstone, true, nil
	}
	if !s.persistent() {
		for _, record := range s.records[logicalPath] {
			if record == nil || record.VersionID != versionID {
				continue
			}
			proto := record.toProto()
			content := append([]byte(nil), record.Content...)
			s.mu.RUnlock()
			return proto, content, !record.Tombstone, true, nil
		}
		s.mu.RUnlock()
		return nil, nil, false, false, nil
	}
	db := s.db
	s.mu.RUnlock()

	var found *storedGuardianFileVersion
	_ = db.View(func(tx metastore.Tx) error {
		return tx.ScanPrefix(guardianVersionsHistoryBucket, []byte(logicalPath+"\x00"), func(_, value []byte) error {
			var record storedGuardianFileVersion
			if json.Unmarshal(value, &record) != nil || record.VersionID != versionID {
				return nil
			}
			found = &record
			return metastore.ErrStopIteration
		})
	})
	if found == nil {
		return nil, nil, false, false, nil
	}
	content, available := s.cachedContent(logicalPath, versionID)
	if found.Tombstone {
		return found.toProto(), nil, false, true, nil
	}
	if !available && found.ContentSHA256 == emptyContentSHA256 {
		return found.toProto(), []byte{}, true, true, nil
	}
	return found.toProto(), content, available, true, nil
}

func (s *guardianVersionStore) cacheContent(logicalPath, versionID string, content []byte) {
	if len(content) == 0 {
		// Empty content is identified by its SHA-256; caching it would make
		// the eviction queue grow without bound.
		return
	}
	key := logicalPath + "\x00" + versionID
	s.contentMu.Lock()
	defer s.contentMu.Unlock()
	if _, exists := s.contentCache[key]; exists {
		return
	}
	copied := append([]byte(nil), content...)
	s.contentCache[key] = copied
	s.contentQueue = append(s.contentQueue, key)
	s.contentBytes += int64(len(copied))
	for s.contentBytes > guardianVersionsContentLimit && len(s.contentQueue) > 0 {
		oldest := s.contentQueue[0]
		s.contentQueue = s.contentQueue[1:]
		if old, ok := s.contentCache[oldest]; ok {
			s.contentBytes -= int64(len(old))
			delete(s.contentCache, oldest)
		}
	}
}

func (s *guardianVersionStore) cachedContent(logicalPath, versionID string) ([]byte, bool) {
	key := logicalPath + "\x00" + versionID
	s.contentMu.RLock()
	defer s.contentMu.RUnlock()
	content, ok := s.contentCache[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), content...), true
}

func guardianVersionID(logicalPath, contentSHA, batchRevisionID string, committedAt int64, seq uint64, tombstone bool) string {
	sum := sha256.Sum256([]byte(logicalPath + "|" + contentSHA + "|" + batchRevisionID + "|" + strconv.FormatInt(committedAt, 10) + "|" + strconv.FormatUint(seq, 10) + "|" + strconv.FormatBool(tombstone)))
	return fmt.Sprintf("ver_%d_%s", committedAt, hex.EncodeToString(sum[:])[:12])
}

func (v *storedGuardianFileVersion) toProto() *pb.GuardianFileVersion {
	if v == nil {
		return nil
	}
	return &pb.GuardianFileVersion{
		LogicalPath:     v.LogicalPath,
		DisplayPath:     v.DisplayPath,
		StorageId:       v.StorageID,
		VersionId:       v.VersionID,
		BatchRevisionId: v.BatchRevisionID,
		ContentSha256:   v.ContentSHA256,
		CommittedAt:     v.CommittedAt,
		Tombstone:       v.Tombstone,
		PrincipalId:     v.PrincipalID,
		Reason:          v.Reason,
	}
}

package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

import pb "github.com/radryc/monofs/api/proto"

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
	Content         []byte `json:"content,omitempty"`
}

type guardianVersionSnapshot struct {
	Records map[string][]*storedGuardianFileVersion `json:"records"`
	Current map[string]string                       `json:"current"`
}

type guardianVersionCommit struct {
	LogicalPath     string
	DisplayPath     string
	StorageID       string
	BatchRevisionID string
	PrincipalID     string
	Reason          string
	CorrelationID   string
	Content         []byte
	Tombstone       bool
	CommittedAt     int64
}

type guardianVersionStore struct {
	mu          sync.RWMutex
	records     map[string][]*storedGuardianFileVersion
	current     map[string]string
	persistPath string
}

func newGuardianVersionStore(stateDir string) (*guardianVersionStore, error) {
	store := &guardianVersionStore{
		records: make(map[string][]*storedGuardianFileVersion),
		current: make(map[string]string),
	}
	if strings.TrimSpace(stateDir) == "" {
		return store, nil
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create guardian state dir: %w", err)
	}
	store.persistPath = filepath.Join(stateDir, "guardian_versions.json")
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *guardianVersionStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.persistPath == "" {
		return nil
	}
	data, err := os.ReadFile(s.persistPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read guardian versions: %w", err)
	}

	var snapshot guardianVersionSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("decode guardian versions: %w", err)
	}
	if snapshot.Records != nil {
		s.records = snapshot.Records
	}
	if snapshot.Current != nil {
		s.current = snapshot.Current
	}
	return nil
}

func (s *guardianVersionStore) saveLocked() error {
	if s.persistPath == "" {
		return nil
	}

	snapshot := guardianVersionSnapshot{
		Records: s.records,
		Current: s.current,
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode guardian versions: %w", err)
	}

	tmpPath := s.persistPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write guardian versions tmp: %w", err)
	}
	if err := os.Rename(tmpPath, s.persistPath); err != nil {
		return fmt.Errorf("replace guardian versions: %w", err)
	}
	return nil
}

func (s *guardianVersionStore) currentVersion(logicalPath string) (*storedGuardianFileVersion, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	versionID, ok := s.current[logicalPath]
	if !ok {
		return nil, false
	}
	for i := len(s.records[logicalPath]) - 1; i >= 0; i-- {
		record := s.records[logicalPath][i]
		if record != nil && record.VersionID == versionID {
			cloned := *record
			cloned.Content = append([]byte(nil), record.Content...)
			return &cloned, true
		}
	}
	return nil, false
}

func (s *guardianVersionStore) commit(change guardianVersionCommit) (*pb.GuardianFileVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	contentSHA := ""
	if len(change.Content) > 0 {
		sum := sha256.Sum256(change.Content)
		contentSHA = hex.EncodeToString(sum[:])
	}

	record := &storedGuardianFileVersion{
		LogicalPath:     change.LogicalPath,
		DisplayPath:     change.DisplayPath,
		StorageID:       change.StorageID,
		VersionID:       guardianVersionID(change.LogicalPath, contentSHA, change.CommittedAt, change.Tombstone),
		BatchRevisionID: change.BatchRevisionID,
		ContentSHA256:   contentSHA,
		CommittedAt:     change.CommittedAt,
		Tombstone:       change.Tombstone,
		PrincipalID:     change.PrincipalID,
		Reason:          change.Reason,
		CorrelationID:   change.CorrelationID,
		Content:         append([]byte(nil), change.Content...),
	}

	s.records[change.LogicalPath] = append(s.records[change.LogicalPath], record)
	s.current[change.LogicalPath] = record.VersionID
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return record.toProto(), nil
}

func (s *guardianVersionStore) list(logicalPath string, pageSize int32, pageToken string) ([]*pb.GuardianFileVersion, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

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

	source := s.records[logicalPath]
	if offset >= len(source) {
		return nil, "", nil
	}

	newestFirst := make([]*pb.GuardianFileVersion, 0, len(source))
	for i := len(source) - 1; i >= 0; i-- {
		if source[i] == nil {
			continue
		}
		newestFirst = append(newestFirst, source[i].toProto())
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

func (s *guardianVersionStore) get(logicalPath, versionID string) (*pb.GuardianFileVersion, []byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, record := range s.records[logicalPath] {
		if record == nil || record.VersionID != versionID {
			continue
		}
		return record.toProto(), append([]byte(nil), record.Content...), true
	}
	return nil, nil, false
}

func guardianVersionID(logicalPath, contentSHA string, committedAt int64, tombstone bool) string {
	sum := sha256.Sum256([]byte(logicalPath + "|" + contentSHA + "|" + strconv.FormatInt(committedAt, 10) + "|" + strconv.FormatBool(tombstone)))
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

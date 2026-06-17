package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
)

type UploadSession struct {
	ID        string    `json:"id"`
	Repo      string    `json:"repo"`
	Digest    string    `json:"digest"`
	Data      []byte    `json:"-"`
	Size      int64     `json:"size"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type UploadManager struct {
	mu      sync.RWMutex
	sessions map[string]*UploadSession
}

func NewUploadManager() *UploadManager {
	return &UploadManager{
		sessions: make(map[string]*UploadSession),
	}
}

func (um *UploadManager) Start(repo string) *UploadSession {
	um.mu.Lock()
	defer um.mu.Unlock()
	session := &UploadSession{
		ID:        uuid.New().String(),
		Repo:      repo,
		StartedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	um.sessions[session.ID] = session
	return session
}

func (um *UploadManager) Get(id string) (*UploadSession, bool) {
	um.mu.RLock()
	defer um.mu.RUnlock()
	s, ok := um.sessions[id]
	return s, ok
}

func (um *UploadManager) Append(id string, chunkData []byte) (*UploadSession, error) {
	um.mu.Lock()
	defer um.mu.Unlock()
	s, ok := um.sessions[id]
	if !ok {
		return nil, fmt.Errorf("upload session not found: %s", id)
	}
	s.Data = append(s.Data, chunkData...)
	s.Size = int64(len(s.Data))
	s.UpdatedAt = time.Now()
	return s, nil
}

func (um *UploadManager) Complete(id, digest string) (*UploadSession, error) {
	um.mu.Lock()
	defer um.mu.Unlock()
	s, ok := um.sessions[id]
	if !ok {
		return nil, fmt.Errorf("upload session not found: %s", id)
	}
	s.Digest = digest
	return s, nil
}

func (um *UploadManager) Remove(id string) {
	um.mu.Lock()
	defer um.mu.Unlock()
	delete(um.sessions, id)
}

func (um *UploadManager) Cleanup(maxAge time.Duration) int {
	um.mu.Lock()
	defer um.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for id, s := range um.sessions {
		if s.UpdatedAt.Before(cutoff) {
			delete(um.sessions, id)
			removed++
		}
	}
	return removed
}

func hexEncode(data []byte) string {
	return hex.EncodeToString(data)
}

func sha256Sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func digestContent(data []byte) string {
	return "sha256:" + sha256Sum(data)
}

func readAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

type uploadStateJSON struct {
	ID        string `json:"id"`
	Repo      string `json:"repo"`
	Size      int64  `json:"size"`
	StartedAt int64  `json:"started_at"`
}

func (um *UploadManager) MarshalJSON() ([]byte, error) {
	um.mu.RLock()
	defer um.mu.RUnlock()
	states := make([]uploadStateJSON, 0, len(um.sessions))
	for _, s := range um.sessions {
		states = append(states, uploadStateJSON{
			ID:        s.ID,
			Repo:      s.Repo,
			Size:      s.Size,
			StartedAt: s.StartedAt.Unix(),
		})
	}
	return json.Marshal(states)
}

package registry

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

type UploadSession struct {
	ID        string    `json:"id"`
	Repo      string    `json:"repo"`
	File      *os.File  `json:"-"`
	Size      int64     `json:"size"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type UploadManager struct {
	mu       sync.RWMutex
	sessions map[string]*UploadSession
}

func NewUploadManager() *UploadManager {
	return &UploadManager{
		sessions: make(map[string]*UploadSession),
	}
}

func (um *UploadManager) Start(repo string) (*UploadSession, error) {
	f, err := os.CreateTemp("", "monofs-registry-upload-*")
	if err != nil {
		return nil, fmt.Errorf("create upload temp file: %w", err)
	}
	um.mu.Lock()
	defer um.mu.Unlock()
	session := &UploadSession{
		ID:        uuid.New().String(),
		Repo:      repo,
		File:      f,
		StartedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	um.sessions[session.ID] = session
	return session, nil
}

func (um *UploadManager) Get(id string) (*UploadSession, bool) {
	um.mu.RLock()
	defer um.mu.RUnlock()
	s, ok := um.sessions[id]
	return s, ok
}

func (um *UploadManager) Append(id string, r io.Reader) (*UploadSession, int64, error) {
	um.mu.Lock()
	defer um.mu.Unlock()
	s, ok := um.sessions[id]
	if !ok {
		return nil, 0, fmt.Errorf("upload session not found: %s", id)
	}
	n, err := io.Copy(s.File, r)
	if err != nil {
		return nil, 0, fmt.Errorf("write to upload temp file: %w", err)
	}
	s.Size += n
	s.UpdatedAt = time.Now()
	return s, n, nil
}

func (um *UploadManager) Remove(id string) {
	um.mu.Lock()
	defer um.mu.Unlock()
	s, ok := um.sessions[id]
	if !ok {
		return
	}
	s.File.Close()
	os.Remove(s.File.Name())
	delete(um.sessions, id)
}

func (um *UploadManager) Cleanup(maxAge time.Duration) int {
	um.mu.Lock()
	defer um.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for id, s := range um.sessions {
		if s.UpdatedAt.Before(cutoff) {
			s.File.Close()
			os.Remove(s.File.Name())
			delete(um.sessions, id)
			removed++
		}
	}
	return removed
}

func (s *UploadSession) SeekToStart() error {
	_, err := s.File.Seek(0, 0)
	return err
}

func (s *UploadSession) Digest() (string, error) {
	if err := s.SeekToStart(); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, s.File); err != nil {
		return "", fmt.Errorf("compute digest: %w", err)
	}
	_ = s.SeekToStart()
	return "sha256:" + fmt.Sprintf("%x", h.Sum(nil)), nil
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

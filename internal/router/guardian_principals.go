package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type guardianPrincipal struct {
	PrincipalID string `json:"principal_id"`
	TokenHash   string `json:"token_hash"`
	Role        string `json:"role"`
	DisplayName string `json:"display_name"`
	CreatedAt   int64  `json:"created_at"`
	Disabled    bool   `json:"disabled"`
	BaseURL     string `json:"base_url,omitempty"`
}

type guardianPrincipalStore struct {
	mu          sync.RWMutex
	principals  map[string]*guardianPrincipal
	persistPath string
}

func newGuardianPrincipalStore(stateDir string) (*guardianPrincipalStore, error) {
	store := &guardianPrincipalStore{
		principals: make(map[string]*guardianPrincipal),
	}
	if strings.TrimSpace(stateDir) == "" {
		return store, nil
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create guardian state dir: %w", err)
	}
	store.persistPath = filepath.Join(stateDir, "guardian_principals.json")
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *guardianPrincipalStore) load() error {
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
		return fmt.Errorf("read guardian principals: %w", err)
	}

	var stored []*guardianPrincipal
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("decode guardian principals: %w", err)
	}

	for _, principal := range stored {
		if principal == nil || principal.PrincipalID == "" {
			continue
		}
		cloned := *principal
		s.principals[principal.PrincipalID] = &cloned
	}
	return nil
}

func (s *guardianPrincipalStore) saveLocked() error {
	if s.persistPath == "" {
		return nil
	}

	entries := make([]*guardianPrincipal, 0, len(s.principals))
	for _, principal := range s.principals {
		if principal == nil {
			continue
		}
		cloned := *principal
		entries = append(entries, &cloned)
	}

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encode guardian principals: %w", err)
	}

	tmpPath := s.persistPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write guardian principals tmp: %w", err)
	}
	if err := os.Rename(tmpPath, s.persistPath); err != nil {
		return fmt.Errorf("replace guardian principals: %w", err)
	}
	return nil
}

func (s *guardianPrincipalStore) upsertConnectedClient(principalID, token, role, displayName, baseURL string) (*guardianPrincipal, error) {
	if strings.TrimSpace(principalID) == "" || strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("guardian principal requires principal id and token")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	principal := s.principals[principalID]
	if principal == nil {
		principal = &guardianPrincipal{
			PrincipalID: principalID,
			CreatedAt:   now,
		}
	}
	principal.TokenHash = guardianTokenHash(token)
	if strings.TrimSpace(role) == "" {
		role = inferGuardianPrincipalRole(principalID)
	}
	if strings.TrimSpace(displayName) == "" {
		displayName = principalID
	}
	principal.Role = role
	principal.DisplayName = displayName
	principal.BaseURL = baseURL
	principal.Disabled = false
	if principal.CreatedAt == 0 {
		principal.CreatedAt = now
	}
	s.principals[principalID] = principal

	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	cloned := *principal
	return &cloned, nil
}

func (s *guardianPrincipalStore) validateToken(token string) (*guardianPrincipal, bool) {
	if strings.TrimSpace(token) == "" {
		return nil, false
	}

	tokenHash := guardianTokenHash(token)

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, principal := range s.principals {
		if principal == nil || principal.Disabled {
			continue
		}
		if principal.TokenHash == tokenHash {
			cloned := *principal
			return &cloned, true
		}
	}
	return nil, false
}

func (s *guardianPrincipalStore) validateTokenForPrincipal(token, principalID string) (*guardianPrincipal, bool) {
	if strings.TrimSpace(token) == "" || strings.TrimSpace(principalID) == "" {
		return nil, false
	}

	tokenHash := guardianTokenHash(token)

	s.mu.RLock()
	defer s.mu.RUnlock()

	principal, ok := s.principals[principalID]
	if !ok || principal == nil || principal.Disabled || principal.TokenHash != tokenHash {
		return nil, false
	}
	cloned := *principal
	return &cloned, true
}

func guardianTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func inferGuardianPrincipalRole(clientID string) string {
	switch {
	case strings.Contains(clientID, "pusher"):
		return "pusher"
	case strings.Contains(clientID, "cli"):
		return "cli"
	default:
		return "control-plane"
	}
}

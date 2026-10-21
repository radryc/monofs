package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/radryc/monofs/pkg/authz"
)

type guardianPrincipal struct {
	PrincipalID string        `json:"principal_id"`
	TokenHash   string        `json:"token_hash"`
	Role        string        `json:"role"`
	DisplayName string        `json:"display_name"`
	CreatedAt   int64         `json:"created_at"`
	Disabled    bool          `json:"disabled"`
	BaseURL     string        `json:"base_url,omitempty"`
	Grants      []authz.Grant `json:"grants,omitempty"`
}

type guardianPrincipalStore struct {
	mu          sync.RWMutex
	principals  map[string]*guardianPrincipal
	byTokenHash map[string][]*guardianPrincipal
	grants      map[string][]authz.Grant
	persistPath string
	dirty       bool
	lastSave    time.Time
	flushTimer  *time.Timer
	closed      bool
}

func newGuardianPrincipalStore(stateDir string) (*guardianPrincipalStore, error) {
	store := &guardianPrincipalStore{
		principals:  make(map[string]*guardianPrincipal),
		byTokenHash: make(map[string][]*guardianPrincipal),
		grants:      make(map[string][]authz.Grant),
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

func (s *guardianPrincipalStore) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.flushTimer != nil {
		s.flushTimer.Stop()
		s.flushTimer = nil
	}
	if s.dirty {
		_ = s.saveLocked()
		s.dirty = false
	}
}

func (s *guardianPrincipalStore) indexPrincipalLocked(principal *guardianPrincipal, oldTokenHash string) {
	if principal == nil || principal.PrincipalID == "" {
		return
	}
	if oldTokenHash != "" && oldTokenHash != principal.TokenHash {
		list := s.byTokenHash[oldTokenHash]
		filtered := list[:0]
		for _, p := range list {
			if p != nil && p.PrincipalID != principal.PrincipalID {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) == 0 {
			delete(s.byTokenHash, oldTokenHash)
		} else {
			s.byTokenHash[oldTokenHash] = filtered
		}
	}
	if principal.TokenHash == "" {
		return
	}
	list := s.byTokenHash[principal.TokenHash]
	for i, p := range list {
		if p != nil && p.PrincipalID == principal.PrincipalID {
			list[i] = principal
			return
		}
	}
	s.byTokenHash[principal.TokenHash] = append(list, principal)
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
		s.indexPrincipalLocked(&cloned, "")
		if len(principal.Grants) > 0 {
			grantsCopy := make([]authz.Grant, len(principal.Grants))
			copy(grantsCopy, principal.Grants)
			s.grants[principal.PrincipalID] = grantsCopy
		}
	}
	return nil
}

func (s *guardianPrincipalStore) saveLocked() error {
	if s.persistPath == "" {
		return nil
	}

	entries := make([]*guardianPrincipal, 0, len(s.principals))
	for pid, principal := range s.principals {
		if principal == nil {
			continue
		}
		cloned := *principal
		if grants, ok := s.grants[pid]; ok && len(grants) > 0 {
			cloneGrants := make([]authz.Grant, len(grants))
			copy(cloneGrants, grants)
			cloned.Grants = cloneGrants
		} else {
			cloned.Grants = nil
		}
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
	s.lastSave = time.Now()
	s.dirty = false
	return nil
}

func (s *guardianPrincipalStore) saveCoalescedLocked() error {
	if s.persistPath == "" {
		return nil
	}
	const minSaveInterval = 2 * time.Second
	if s.lastSave.IsZero() || time.Since(s.lastSave) >= minSaveInterval {
		return s.saveLocked()
	}
	s.dirty = true
	if s.flushTimer == nil && !s.closed {
		s.flushTimer = time.AfterFunc(minSaveInterval, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.flushTimer = nil
			if s.dirty && !s.closed {
				_ = s.saveLocked()
			}
		})
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
	tokenHash := guardianTokenHash(token)
	if strings.TrimSpace(role) == "" {
		role = inferGuardianPrincipalRole(principalID)
	}
	if strings.TrimSpace(displayName) == "" {
		displayName = principalID
	}

	principal := s.principals[principalID]
	if principal != nil &&
		principal.TokenHash == tokenHash &&
		principal.Role == role &&
		principal.DisplayName == displayName &&
		principal.BaseURL == baseURL &&
		!principal.Disabled &&
		principal.CreatedAt != 0 {
		cloned := *principal
		return &cloned, nil
	}

	oldTokenHash := ""
	if principal == nil {
		principal = &guardianPrincipal{
			PrincipalID: principalID,
			CreatedAt:   now,
		}
	} else {
		oldTokenHash = principal.TokenHash
	}
	principal.TokenHash = tokenHash
	principal.Role = role
	principal.DisplayName = displayName
	principal.BaseURL = baseURL
	principal.Disabled = false
	if principal.CreatedAt == 0 {
		principal.CreatedAt = now
	}
	s.principals[principalID] = principal
	s.indexPrincipalLocked(principal, oldTokenHash)

	if err := s.saveCoalescedLocked(); err != nil {
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
	for _, principal := range s.byTokenHash[tokenHash] {
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

func (s *guardianPrincipalStore) setGrants(principalID string, grants []authz.Grant) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.principals[principalID]; !ok {
		return fmt.Errorf("unknown principal %q", principalID)
	}

	if len(grants) == 0 {
		delete(s.grants, principalID)
	} else {
		clone := make([]authz.Grant, len(grants))
		copy(clone, grants)
		s.grants[principalID] = clone
	}

	return s.saveLocked()
}

func (s *guardianPrincipalStore) grantsFor(principalID string) []authz.Grant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	grants, ok := s.grants[principalID]
	if !ok {
		return nil
	}
	clone := make([]authz.Grant, len(grants))
	copy(clone, grants)
	return clone
}

type principalGrantEvaluator struct {
	grants []authz.Grant
}

func (e *principalGrantEvaluator) Can(_ context.Context, id authz.Identity, partition string, action authz.Action) bool {
	if id.IsAnonymous() {
		return false
	}
	for _, g := range e.grants {
		matchPartition := g.Partition == authz.WildcardPartition || g.Partition == partition
		if !matchPartition {
			continue
		}
		matchedSubject := false
		if s := strings.TrimSpace(g.Subject); s != "" {
			matchedSubject = s == id.Subject || s == id.ClientID || (id.Email != "" && s == id.Email)
		}
		matchedGroup := false
		if grp := strings.TrimSpace(g.Group); grp != "" {
			matchedGroup = id.HasGroup(grp)
		}
		if !matchedSubject && !matchedGroup {
			continue
		}
		if g.Role.Allows(action) {
			return true
		}
	}
	return false
}

func (s *guardianPrincipalStore) grantEvaluator() authz.GrantEvaluator {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var allGrants []authz.Grant
	for _, g := range s.grants {
		allGrants = append(allGrants, g...)
	}
	return &principalGrantEvaluator{grants: allGrants}
}

// GuardianControlPlanePrincipalID is the principal ID Guardian's registry,
// reconciler, results processor and observed collector claim for their writes.
const GuardianControlPlanePrincipalID = "guardiand"

// SeedGuardianPrincipal registers a guardian principal at startup. It is used
// to seed the break-glass token so every router pod in a multi-router
// deployment accepts Guardian control-plane and CLI mutations: the guardian
// principal store is in-memory per pod, so a mutation that lands on a pod that
// has not seen the client registration would otherwise be rejected with
// "invalid guardian token".
func (r *Router) SeedGuardianPrincipal(principalID, token, role, displayName, baseURL string) error {
	if r.guardianPrincipals == nil {
		return fmt.Errorf("guardian principal store is not configured")
	}
	_, err := r.guardianPrincipals.upsertConnectedClient(principalID, token, role, displayName, baseURL)
	return err
}

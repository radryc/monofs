package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Grant authorizes a subject (individual principal) or a group with a Role on a
// partition. Exactly one of Subject or Group must be set. Partition "*" matches
// every partition.
type Grant struct {
	// Subject matches an individual principal by Subject, ClientID, or Email.
	Subject string `json:"subject,omitempty"`
	// Group matches any principal whose IdP groups include this value.
	Group string `json:"group,omitempty"`
	// Partition is the partition name this grant applies to, or "*" for all.
	Partition string `json:"partition"`
	// Role is the role granted on the partition.
	Role Role `json:"role"`
}

// WildcardPartition matches every partition.
const WildcardPartition = "*"

// Validate reports whether the grant is well-formed.
func (g Grant) Validate() error {
	hasSubject := strings.TrimSpace(g.Subject) != ""
	hasGroup := strings.TrimSpace(g.Group) != ""
	if hasSubject == hasGroup {
		return fmt.Errorf("authz: grant must set exactly one of subject or group")
	}
	if strings.TrimSpace(g.Partition) == "" {
		return fmt.Errorf("authz: grant partition is required")
	}
	if !g.Role.Valid() {
		return fmt.Errorf("authz: grant has invalid role %q", g.Role)
	}
	return nil
}

func (g Grant) matchesPartition(partition string) bool {
	return g.Partition == WildcardPartition || g.Partition == partition
}

func (g Grant) matchesIdentity(id Identity) bool {
	if s := strings.TrimSpace(g.Subject); s != "" {
		return s == id.Subject || s == id.ClientID || (id.Email != "" && s == id.Email)
	}
	if grp := strings.TrimSpace(g.Group); grp != "" {
		return id.HasGroup(grp)
	}
	return false
}

// GrantStore is a thread-safe, optionally file-backed set of grants that
// implements GrantEvaluator.
type GrantStore struct {
	mu          sync.RWMutex
	grants      []Grant
	persistPath string
}

// NewGrantStore creates a store. When persistPath is non-empty, existing grants
// are loaded and mutations are persisted.
func NewGrantStore(persistPath string) (*GrantStore, error) {
	s := &GrantStore{persistPath: strings.TrimSpace(persistPath)}
	if s.persistPath == "" {
		return s, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.persistPath), 0o755); err != nil {
		return nil, fmt.Errorf("authz: create grant dir: %w", err)
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *GrantStore) load() error {
	data, err := os.ReadFile(s.persistPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("authz: read grants: %w", err)
	}
	var grants []Grant
	if err := json.Unmarshal(data, &grants); err != nil {
		return fmt.Errorf("authz: decode grants: %w", err)
	}
	s.grants = grants
	return nil
}

func (s *GrantStore) saveLocked() error {
	if s.persistPath == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.grants, "", "  ")
	if err != nil {
		return fmt.Errorf("authz: encode grants: %w", err)
	}
	tmp := s.persistPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("authz: write grants tmp: %w", err)
	}
	if err := os.Rename(tmp, s.persistPath); err != nil {
		return fmt.Errorf("authz: replace grants: %w", err)
	}
	return nil
}

// Add appends a grant after validating it, persisting the change.
func (s *GrantStore) Add(g Grant) error {
	if err := g.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = append(s.grants, g)
	return s.saveLocked()
}

// Replace atomically swaps all grants (used by the OWNERS policy compiler).
func (s *GrantStore) Replace(grants []Grant) error {
	for _, g := range grants {
		if err := g.Validate(); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = append([]Grant(nil), grants...)
	return s.saveLocked()
}

// Grants returns a copy of the current grants.
func (s *GrantStore) Grants() []Grant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Grant(nil), s.grants...)
}

// Can reports whether id may perform action on partition. It implements
// GrantEvaluator.
func (s *GrantStore) Can(_ context.Context, id Identity, partition string, action Action) bool {
	if id.IsAnonymous() {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, g := range s.grants {
		if g.matchesPartition(partition) && g.matchesIdentity(id) && g.Role.Allows(action) {
			return true
		}
	}
	return false
}

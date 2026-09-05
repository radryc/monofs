// Package mergerequest provides a native merge-request (proposal) store for
// MonoFS-managed partitions that are not backed by an external git provider.
// Non-owners of a subtree open a proposal; owners of the affected subtrees
// review, approve, and merge it. This is the MonoFS-native counterpart to the
// external GitHub/GitLab pull-request flow (authz epic D3).
package mergerequest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/radryc/monofs/pkg/authz"
)

// State is the lifecycle state of a proposal.
type State string

const (
	StateOpen     State = "open"
	StateApproved State = "approved"
	StateMerged   State = "merged"
	StateRejected State = "rejected"
)

// Proposal is a proposed change to a set of partition subtree paths authored by
// a non-owner, awaiting owner review.
type Proposal struct {
	ID          string
	Partition   string
	Paths       []string
	Author      string // principal id of the proposer
	Title       string
	Description string
	State       State
	Approvals   []string // principal ids of owners who approved
	CreatedAt   int64
	UpdatedAt   int64
}

// OwnershipChecker reports whether an identity owns (may directly modify) all of
// the given paths. *authz.OwnershipResolver satisfies this interface.
type OwnershipChecker interface {
	OwnsAll(ctx context.Context, paths []string, id authz.Identity) (ownsAll bool, unowned []string, err error)
}

// Store is an in-memory, thread-safe proposal store gated on subtree ownership.
type Store struct {
	mu        sync.Mutex
	proposals map[string]*Proposal
	owners    OwnershipChecker
	seq       int
	now       func() time.Time
}

// NewStore builds a proposal store whose approval/merge gate uses owners.
func NewStore(owners OwnershipChecker) *Store {
	return &Store{
		proposals: make(map[string]*Proposal),
		owners:    owners,
		now:       time.Now,
	}
}

// Create opens a new proposal authored by author for the given partition paths.
func (s *Store) Create(author, partition string, paths []string, title, description string) (*Proposal, error) {
	if author == "" {
		return nil, fmt.Errorf("mergerequest: author is required")
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("mergerequest: at least one path is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	now := s.now().Unix()
	p := &Proposal{
		ID:          fmt.Sprintf("mr-%d", s.seq),
		Partition:   partition,
		Paths:       append([]string(nil), paths...),
		Author:      author,
		Title:       title,
		Description: description,
		State:       StateOpen,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.proposals[p.ID] = p
	clone := *p
	return &clone, nil
}

// Get returns a copy of a proposal by id.
func (s *Store) Get(id string) (*Proposal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proposals[id]
	if !ok {
		return nil, false
	}
	clone := *p
	return &clone, true
}

// List returns copies of proposals, optionally filtered by partition ("" = all).
func (s *Store) List(partition string) []*Proposal {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Proposal, 0, len(s.proposals))
	for _, p := range s.proposals {
		if partition != "" && p.Partition != partition {
			continue
		}
		clone := *p
		out = append(out, &clone)
	}
	return out
}

// Approve records an owner approval. The approver must own every affected path,
// must not be the author, and the proposal must be open. A single approval moves
// the proposal to the approved state.
func (s *Store) Approve(ctx context.Context, id string, approver authz.Identity) (*Proposal, error) {
	approverID := approver.PrincipalID()
	if approverID == "" {
		return nil, fmt.Errorf("mergerequest: approver identity required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proposals[id]
	if !ok {
		return nil, fmt.Errorf("mergerequest: proposal %q not found", id)
	}
	if p.State != StateOpen {
		return nil, fmt.Errorf("mergerequest: proposal %q is %s, not open", id, p.State)
	}
	if approverID == p.Author {
		return nil, fmt.Errorf("mergerequest: author cannot approve own proposal")
	}
	if err := s.requireOwner(ctx, p, approver); err != nil {
		return nil, err
	}
	if !contains(p.Approvals, approverID) {
		p.Approvals = append(p.Approvals, approverID)
	}
	p.State = StateApproved
	p.UpdatedAt = s.now().Unix()
	clone := *p
	return &clone, nil
}

// Merge merges an approved proposal. The merger must own every affected path.
func (s *Store) Merge(ctx context.Context, id string, merger authz.Identity) (*Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proposals[id]
	if !ok {
		return nil, fmt.Errorf("mergerequest: proposal %q not found", id)
	}
	if p.State != StateApproved {
		return nil, fmt.Errorf("mergerequest: proposal %q must be approved before merge (state=%s)", id, p.State)
	}
	if err := s.requireOwner(ctx, p, merger); err != nil {
		return nil, err
	}
	p.State = StateMerged
	p.UpdatedAt = s.now().Unix()
	clone := *p
	return &clone, nil
}

// Reject closes a proposal without merging. Only an owner may reject.
func (s *Store) Reject(ctx context.Context, id string, by authz.Identity) (*Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proposals[id]
	if !ok {
		return nil, fmt.Errorf("mergerequest: proposal %q not found", id)
	}
	if p.State == StateMerged {
		return nil, fmt.Errorf("mergerequest: proposal %q already merged", id)
	}
	if err := s.requireOwner(ctx, p, by); err != nil {
		return nil, err
	}
	p.State = StateRejected
	p.UpdatedAt = s.now().Unix()
	clone := *p
	return &clone, nil
}

func (s *Store) requireOwner(ctx context.Context, p *Proposal, id authz.Identity) error {
	if s.owners == nil {
		return fmt.Errorf("mergerequest: no ownership checker configured")
	}
	ownsAll, unowned, err := s.owners.OwnsAll(ctx, p.Paths, id)
	if err != nil {
		return fmt.Errorf("mergerequest: ownership check: %w", err)
	}
	if !ownsAll {
		return fmt.Errorf("mergerequest: %q does not own paths %v", id.PrincipalID(), unowned)
	}
	return nil
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

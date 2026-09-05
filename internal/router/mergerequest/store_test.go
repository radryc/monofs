package mergerequest

import (
	"context"
	"testing"

	"github.com/radryc/monofs/pkg/authz"
)

// ownedBy grants ownership of all listed paths to the given principal ids.
type ownedBy struct {
	owners map[string]bool // principalID -> owns everything
}

func (o ownedBy) OwnsAll(_ context.Context, paths []string, id authz.Identity) (bool, []string, error) {
	if o.owners[id.PrincipalID()] {
		return true, nil, nil
	}
	return false, paths, nil
}

func TestProposalLifecycle(t *testing.T) {
	owners := ownedBy{owners: map[string]bool{"owner-1": true}}
	s := NewStore(owners)
	ctx := context.Background()

	// Non-owner opens a proposal.
	p, err := s.Create("contributor", "doctor", []string{"doctor/a/x.txt"}, "Fix x", "desc")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.State != StateOpen {
		t.Fatalf("expected open, got %s", p.State)
	}

	owner := authz.Identity{Subject: "owner-1"}
	contributor := authz.Identity{Subject: "contributor"}

	// Non-owner cannot approve.
	if _, err := s.Approve(ctx, p.ID, contributor); err == nil {
		t.Fatal("non-owner should not be able to approve")
	}

	// Owner approves -> approved.
	approved, err := s.Approve(ctx, p.ID, owner)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.State != StateApproved || len(approved.Approvals) != 1 {
		t.Fatalf("unexpected approved proposal: %+v", approved)
	}

	// Owner merges.
	merged, err := s.Merge(ctx, p.ID, owner)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged.State != StateMerged {
		t.Fatalf("expected merged, got %s", merged.State)
	}
}

func TestAuthorCannotSelfApprove(t *testing.T) {
	// Even if the author were an owner, they can't approve their own proposal.
	owners := ownedBy{owners: map[string]bool{"owner-1": true}}
	s := NewStore(owners)
	p, _ := s.Create("owner-1", "doctor", []string{"doctor/a"}, "t", "d")
	if _, err := s.Approve(context.Background(), p.ID, authz.Identity{Subject: "owner-1"}); err == nil {
		t.Fatal("author must not self-approve")
	}
}

func TestMergeRequiresApproval(t *testing.T) {
	owners := ownedBy{owners: map[string]bool{"owner-1": true}}
	s := NewStore(owners)
	p, _ := s.Create("contributor", "doctor", []string{"doctor/a"}, "t", "d")
	// Merge before approval fails.
	if _, err := s.Merge(context.Background(), p.ID, authz.Identity{Subject: "owner-1"}); err == nil {
		t.Fatal("merge should require approval first")
	}
}

func TestRejectByOwner(t *testing.T) {
	owners := ownedBy{owners: map[string]bool{"owner-1": true}}
	s := NewStore(owners)
	p, _ := s.Create("contributor", "doctor", []string{"doctor/a"}, "t", "d")
	rejected, err := s.Reject(context.Background(), p.ID, authz.Identity{Subject: "owner-1"})
	if err != nil || rejected.State != StateRejected {
		t.Fatalf("reject: state=%v err=%v", rejected, err)
	}
	// Non-owner cannot reject.
	p2, _ := s.Create("contributor", "doctor", []string{"doctor/a"}, "t", "d")
	if _, err := s.Reject(context.Background(), p2.ID, authz.Identity{Subject: "someone"}); err == nil {
		t.Fatal("non-owner should not reject")
	}
}

func TestListAndGet(t *testing.T) {
	s := NewStore(ownedBy{owners: map[string]bool{}})
	_, _ = s.Create("c", "doctor", []string{"doctor/a"}, "t", "d")
	_, _ = s.Create("c", "monitoring", []string{"monitoring/b"}, "t", "d")
	if got := s.List("doctor"); len(got) != 1 {
		t.Fatalf("expected 1 doctor proposal, got %d", len(got))
	}
	if got := s.List(""); len(got) != 2 {
		t.Fatalf("expected 2 total proposals, got %d", len(got))
	}
	if _, ok := s.Get("nope"); ok {
		t.Fatal("unexpected proposal")
	}
}

package router

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/radryc/monofs/internal/workspacebundle"
	"github.com/radryc/monofs/pkg/authz"
)

// fakeOwnership implements ownershipChecker for tests.
type fakeOwnership struct {
	owned map[string]bool // path -> owned by the expected identity
	err   error
}

func (f fakeOwnership) OwnsAll(_ context.Context, paths []string, _ authz.Identity) (bool, []string, error) {
	if f.err != nil {
		return false, nil, f.err
	}
	var unowned []string
	for _, p := range paths {
		if !f.owned[p] {
			unowned = append(unowned, p)
		}
	}
	return len(unowned) == 0, unowned, nil
}

func TestEvaluateSubtreeOwnershipDisabled(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	// No resolver configured -> always direct.
	dec, unowned, err := r.evaluateSubtreeOwnership(context.Background(), []string{"a/b"})
	if err != nil || dec != MergeDecisionDirect || unowned != nil {
		t.Fatalf("disabled gate: dec=%v unowned=%v err=%v", dec, unowned, err)
	}
}

func TestEvaluateSubtreeOwnershipDecisions(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	r.SetOwnershipResolver(fakeOwnership{owned: map[string]bool{"a/b/c.txt": true}})

	ctx := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "alice"})

	// Owner of all paths -> direct.
	dec, unowned, err := r.evaluateSubtreeOwnership(ctx, []string{"a/b/c.txt"})
	if err != nil || dec != MergeDecisionDirect || len(unowned) != 0 {
		t.Fatalf("owner: dec=%v unowned=%v err=%v", dec, unowned, err)
	}

	// Any unowned path -> merge request, with the unowned subset reported.
	dec, unowned, err = r.evaluateSubtreeOwnership(ctx, []string{"a/b/c.txt", "x/y.txt"})
	if err != nil || dec != MergeDecisionMergeRequest {
		t.Fatalf("non-owner: dec=%v err=%v", dec, err)
	}
	if len(unowned) != 1 || unowned[0] != "x/y.txt" {
		t.Fatalf("expected unowned [x/y.txt], got %v", unowned)
	}

	// Empty path set -> direct (nothing to gate).
	if dec, _, _ := r.evaluateSubtreeOwnership(ctx, nil); dec != MergeDecisionDirect {
		t.Fatalf("empty paths should be direct, got %v", dec)
	}
}

func TestEvaluateSubtreeOwnershipErrorFallsBackDirect(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	r.SetOwnershipResolver(fakeOwnership{err: errors.New("boom")})
	dec, _, err := r.evaluateSubtreeOwnership(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if dec != MergeDecisionDirect {
		t.Fatalf("error path should default to direct, got %v", dec)
	}
}

func TestMergeDecisionString(t *testing.T) {
	if MergeDecisionDirect.String() != "direct" || MergeDecisionMergeRequest.String() != "merge_request" {
		t.Fatal("unexpected MergeDecision strings")
	}
}

func TestChangedPathsFromSourceBundle(t *testing.T) {
	bundle := &workspacebundle.SourceCommitBundle{
		WorkspaceID: "ws",
		Commits: []workspacebundle.SourceCommit{{
			ID: "c1",
			Repositories: []workspacebundle.SourceCommitRepository{{
				DisplayPath: "guardian/doctor",
				Operations: []workspacebundle.Operation{
					{Kind: "write", Path: "a/x.txt"},
					{Kind: "write", Path: "a/x.txt"}, // duplicate collapses
					{Kind: "write", Path: "/b/y.txt"},
				},
			}},
		}},
	}
	got := changedPathsFromSourceBundle(bundle)
	want := map[string]bool{"guardian/doctor/a/x.txt": true, "guardian/doctor/b/y.txt": true}
	if len(got) != 2 {
		t.Fatalf("expected 2 distinct paths, got %v", got)
	}
	for _, p := range got {
		if !want[p] {
			t.Fatalf("unexpected path %q in %v", p, got)
		}
	}
	if changedPathsFromSourceBundle(nil) != nil {
		t.Fatal("nil bundle should yield nil paths")
	}
}

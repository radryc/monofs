package router

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/radryc/monofs/internal/workspacebundle"
	"github.com/radryc/monofs/pkg/authz"
)

// memOwnersSource is an in-memory authz.OwnersSource keyed by directory.
type memOwnersSource map[string]*authz.OwnersFile

func (m memOwnersSource) LoadOwners(_ context.Context, dir string) (*authz.OwnersFile, error) {
	return m[dir], nil
}

func mustOwners(t *testing.T, maintainers ...string) *authz.OwnersFile {
	t.Helper()
	refs := make([]authz.OwnerRef, 0, len(maintainers))
	for _, m := range maintainers {
		refs = append(refs, authz.OwnerRef{Subject: m})
	}
	return &authz.OwnersFile{Version: 1, Maintainers: refs}
}

func sourcePushBundle(displayPath, opPath string) *workspacebundle.SourceCommitBundle {
	return &workspacebundle.SourceCommitBundle{
		WorkspaceID: "ws",
		Commits: []workspacebundle.SourceCommit{{
			ID: "c1",
			Repositories: []workspacebundle.SourceCommitRepository{{
				StorageID:   "storage-1",
				DisplayPath: displayPath,
				RepoURL:     "https://example.com/repo.git",
				Branch:      "main",
				BaseCommit:  "base",
				Operations:  []workspacebundle.Operation{{Kind: "write", Path: opPath}},
			}},
		}},
	}
}

func gateTestRouter(t *testing.T, owners map[string]*authz.OwnersFile) *Router {
	t.Helper()
	cfg := DefaultRouterConfig()
	cfg.OwnershipGateEnabled = true
	r := NewRouter(cfg, slog.New(slog.DiscardHandler))
	r.ConfigureOwnership(authz.NewOwnershipResolver(memOwnersSource(owners), nil))
	return r
}

func TestEnforceOwnershipGateDisabled(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	if err := r.enforceOwnershipGate(context.Background(), "", sourcePushBundle("guardian/doctor", "a/x.txt")); err != nil {
		t.Fatalf("disabled gate must allow, got %v", err)
	}
}

func TestEnforceOwnershipGateOwnedSubtreeAllows(t *testing.T) {
	r := gateTestRouter(t, map[string]*authz.OwnersFile{
		"guardian/doctor": mustOwners(t, "alice"),
	})
	ctx := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "alice"})
	if err := r.enforceOwnershipGate(ctx, "", sourcePushBundle("guardian/doctor", "a/x.txt")); err != nil {
		t.Fatalf("owner push must allow, got %v", err)
	}
}

func TestEnforceOwnershipGateForeignSubtreeDenies(t *testing.T) {
	r := gateTestRouter(t, map[string]*authz.OwnersFile{
		"guardian/doctor": mustOwners(t, "alice"),
	})
	ctx := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "bob"})
	err := r.enforceOwnershipGate(ctx, "", sourcePushBundle("guardian/doctor", "a/x.txt"))
	if err == nil {
		t.Fatal("non-owner direct push must be denied")
	}
	if !strings.Contains(err.Error(), "review required") || !strings.Contains(err.Error(), "alice") {
		t.Fatalf("deny reason should mention review and owner, got %q", err.Error())
	}
}

func TestEnforceOwnershipGateNonDirectAllows(t *testing.T) {
	r := gateTestRouter(t, map[string]*authz.OwnersFile{
		"guardian/doctor": mustOwners(t, "alice"),
	})
	ctx := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "bob"})
	if err := r.enforceOwnershipGate(ctx, "ws/feature", sourcePushBundle("guardian/doctor", "a/x.txt")); err != nil {
		t.Fatalf("non-direct push must allow (PR review at forge), got %v", err)
	}
}

func TestEnforceOwnershipGateUngovernedAllows(t *testing.T) {
	r := gateTestRouter(t, map[string]*authz.OwnersFile{
		"guardian/doctor": mustOwners(t, "alice"),
	})
	ctx := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "bob"})
	if err := r.enforceOwnershipGate(ctx, "", sourcePushBundle("guardian/monitoring", "a/x.txt")); err != nil {
		t.Fatalf("ungoverned subtree must be open by default, got %v", err)
	}
}

func TestSplitManagedDisplayPath(t *testing.T) {
	cases := []struct {
		in          string
		displayPath string
		relative    string
		ok          bool
	}{
		{"guardian/doctor/.guardian/OWNERS", "guardian/doctor", ".guardian/OWNERS", true},
		{"doctor/v1/.guardian/OWNERS", "doctor/v1", ".guardian/OWNERS", true},
		{"guardian-system/.queues/x", "guardian-system", ".queues/x", true},
		{"plain/repo/file", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		dp, rel, ok := splitManagedDisplayPath(c.in)
		if ok != c.ok || dp != c.displayPath || rel != c.relative {
			t.Fatalf("splitManagedDisplayPath(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, dp, rel, ok, c.displayPath, c.relative, c.ok)
		}
	}
}

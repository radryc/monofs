package authz

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func mustAdd(t *testing.T, s *GrantStore, g Grant) {
	t.Helper()
	if err := s.Add(g); err != nil {
		t.Fatalf("Add(%+v): %v", g, err)
	}
}

func TestGrantValidate(t *testing.T) {
	bad := []Grant{
		{Partition: "p", Role: RoleViewer},                           // no subject/group
		{Subject: "a", Group: "g", Partition: "p", Role: RoleViewer}, // both
		{Subject: "a", Role: RoleViewer},                             // no partition
		{Subject: "a", Partition: "p", Role: Role("nope")},           // bad role
	}
	for _, g := range bad {
		if err := g.Validate(); err == nil {
			t.Errorf("expected validation error for %+v", g)
		}
	}
	good := Grant{Subject: "a", Partition: "p", Role: RoleIngester}
	if err := good.Validate(); err != nil {
		t.Errorf("unexpected error for valid grant: %v", err)
	}
}

func TestCanMatrix(t *testing.T) {
	ctx := context.Background()
	s, _ := NewGrantStore("")
	mustAdd(t, s, Grant{Subject: "ci-bot", Partition: "doctor", Role: RoleIngester})
	mustAdd(t, s, Grant{Group: "strata-platform-eng", Partition: "monitoring", Role: RoleMaintainer})
	mustAdd(t, s, Grant{Subject: "root", Partition: WildcardPartition, Role: RoleAdmin})

	ci := Identity{ClientID: "ci-bot"}
	eng := Identity{Subject: "alice", Groups: []string{"strata-platform-eng"}}
	root := Identity{Subject: "root"}
	nobody := Identity{Subject: "nobody"}

	// ingester on doctor: can ingest doctor, not monitoring, cannot modify.
	if !s.Can(ctx, ci, "doctor", ActionIngest) {
		t.Error("ci should ingest doctor")
	}
	if s.Can(ctx, ci, "monitoring", ActionIngest) {
		t.Error("ci should NOT ingest monitoring")
	}
	if s.Can(ctx, ci, "doctor", ActionModify) {
		t.Error("ci ingester should NOT modify doctor")
	}
	if !s.Can(ctx, ci, "doctor", ActionView) {
		t.Error("ingester implies view")
	}

	// maintainer group on monitoring: modify monitoring only.
	if !s.Can(ctx, eng, "monitoring", ActionModify) {
		t.Error("eng should modify monitoring")
	}
	if s.Can(ctx, eng, "doctor", ActionModify) {
		t.Error("eng should NOT modify doctor")
	}

	// admin wildcard: everything, everywhere.
	for _, p := range []string{"doctor", "monitoring", "anything"} {
		for _, a := range []Action{ActionView, ActionIngest, ActionModify} {
			if !s.Can(ctx, root, p, a) {
				t.Errorf("root admin should allow %s on %s", a, p)
			}
		}
	}

	// unknown principal: nothing.
	if s.Can(ctx, nobody, "doctor", ActionView) {
		t.Error("unknown principal should have no access")
	}
	// anonymous: nothing.
	if s.Can(ctx, Identity{}, "doctor", ActionView) {
		t.Error("anonymous should have no access")
	}
}

func TestGrantStorePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "grants.json")

	s1, err := NewGrantStore(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	mustAdd(t, s1, Grant{Subject: "ci-bot", Partition: "doctor", Role: RoleIngester})

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("grants file not written: %v", err)
	}

	// Reload into a fresh store.
	s2, err := NewGrantStore(path)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	if !s2.Can(context.Background(), Identity{ClientID: "ci-bot"}, "doctor", ActionIngest) {
		t.Error("reloaded store lost grant")
	}
}

func TestGrantStoreReplace(t *testing.T) {
	s, _ := NewGrantStore("")
	mustAdd(t, s, Grant{Subject: "old", Partition: "p", Role: RoleAdmin})
	if err := s.Replace([]Grant{{Group: "new-team", Partition: "p", Role: RoleViewer}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if len(s.Grants()) != 1 {
		t.Fatalf("expected 1 grant after replace, got %d", len(s.Grants()))
	}
	if s.Can(context.Background(), Identity{Subject: "old"}, "p", ActionModify) {
		t.Error("old grant should be gone after replace")
	}
}

package authz

import (
	"testing"
)

func TestCompileGrants(t *testing.T) {
	owners := &OwnersFile{
		Version:     1,
		Viewers:     []OwnerRef{{Team: "everyone"}},
		Ingesters:   []OwnerRef{{Subject: "ci-bot"}},
		Maintainers: []OwnerRef{{Team: "platform-eng"}},
	}
	mapping := TeamMapping{"platform-eng": "strata-platform-eng"}

	grants, warnings := CompileGrants("doctor", owners, mapping)

	// everyone is unmapped -> warning + handle used as group.
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %v", warnings)
	}
	if len(grants) != 3 {
		t.Fatalf("expected 3 grants, got %d: %+v", len(grants), grants)
	}

	// Deterministic order: viewer, ingester, maintainer.
	if grants[0].Role != RoleViewer || grants[0].Group != "everyone" {
		t.Fatalf("grant[0] unexpected: %+v", grants[0])
	}
	if grants[1].Role != RoleIngester || grants[1].Subject != "ci-bot" {
		t.Fatalf("grant[1] unexpected: %+v", grants[1])
	}
	if grants[2].Role != RoleMaintainer || grants[2].Group != "strata-platform-eng" {
		t.Fatalf("grant[2] unexpected: %+v", grants[2])
	}

}

func TestParseTeamMapping(t *testing.T) {
	m, err := ParseTeamMapping([]byte("version: 1\nteams:\n  platform-eng: strata-platform-eng\n"))
	if err != nil {
		t.Fatalf("ParseTeamMapping: %v", err)
	}
	if g, ok := m.GroupFor("platform-eng"); !ok || g != "strata-platform-eng" {
		t.Fatalf("GroupFor mapped = %q ok=%v", g, ok)
	}
	if g, ok := m.GroupFor("unknown"); ok || g != "unknown" {
		t.Fatalf("GroupFor unmapped = %q ok=%v", g, ok)
	}
	if _, err := ParseTeamMapping([]byte("version: 2")); err == nil {
		t.Error("expected version error")
	}
}

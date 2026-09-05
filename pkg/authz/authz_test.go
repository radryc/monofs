package authz

import (
	"context"
	"testing"
)

func TestRoleAllows(t *testing.T) {
	tests := []struct {
		role   Role
		action Action
		want   bool
	}{
		{RoleViewer, ActionView, true},
		{RoleViewer, ActionIngest, false},
		{RoleViewer, ActionModify, false},
		{RoleIngester, ActionView, true},
		{RoleIngester, ActionIngest, true},
		{RoleIngester, ActionModify, false},
		{RoleMaintainer, ActionView, true},
		{RoleMaintainer, ActionIngest, true},
		{RoleMaintainer, ActionModify, true},
		{RoleAdmin, ActionView, true},
		{RoleAdmin, ActionIngest, true},
		{RoleAdmin, ActionModify, true},
		{Role("bogus"), ActionView, false},
	}
	for _, tt := range tests {
		if got := tt.role.Allows(tt.action); got != tt.want {
			t.Errorf("Role(%q).Allows(%q) = %v, want %v", tt.role, tt.action, got, tt.want)
		}
	}
}

func TestRoleValid(t *testing.T) {
	for _, r := range []Role{RoleViewer, RoleIngester, RoleMaintainer, RoleAdmin} {
		if !r.Valid() {
			t.Errorf("Role(%q).Valid() = false, want true", r)
		}
	}
	if Role("nope").Valid() {
		t.Error("Role(\"nope\").Valid() = true, want false")
	}
}

func TestIdentityIsAnonymous(t *testing.T) {
	if !(Identity{}).IsAnonymous() {
		t.Error("empty identity should be anonymous")
	}
	if (Identity{Subject: "alice"}).IsAnonymous() {
		t.Error("identity with subject should not be anonymous")
	}
	if (Identity{ClientID: "ci-bot"}).IsAnonymous() {
		t.Error("identity with client id should not be anonymous")
	}
}

func TestIdentityHasGroup(t *testing.T) {
	id := Identity{Groups: []string{"strata-platform-eng", "  CI-Bots "}}
	if !id.HasGroup("strata-platform-eng") {
		t.Error("expected exact group match")
	}
	if !id.HasGroup("ci-bots") {
		t.Error("expected case-insensitive, trimmed group match")
	}
	if id.HasGroup("other") {
		t.Error("did not expect match for absent group")
	}
	if id.HasGroup("") {
		t.Error("empty group should never match")
	}
}

func TestIdentityPrincipalID(t *testing.T) {
	if got := (Identity{ClientID: "ci", Subject: "alice"}).PrincipalID(); got != "ci" {
		t.Errorf("PrincipalID = %q, want client id preference", got)
	}
	if got := (Identity{Subject: "alice"}).PrincipalID(); got != "alice" {
		t.Errorf("PrincipalID = %q, want subject fallback", got)
	}
	if got := (Identity{}).PrincipalID(); got != "" {
		t.Errorf("PrincipalID = %q, want empty", got)
	}
}

func TestNoopVerifier(t *testing.T) {
	id, err := NoopVerifier{}.Verify(context.Background(), "any-token")
	if err != nil {
		t.Fatalf("NoopVerifier.Verify returned error: %v", err)
	}
	if !id.IsAnonymous() {
		t.Error("NoopVerifier should return anonymous identity")
	}
}

func TestEvaluatorDefaults(t *testing.T) {
	ctx := context.Background()
	if !(AllowAllEvaluator{}).Can(ctx, Identity{}, "p", ActionModify) {
		t.Error("AllowAllEvaluator should permit everything")
	}
	if (DenyAllEvaluator{}).Can(ctx, Identity{}, "p", ActionView) {
		t.Error("DenyAllEvaluator should reject everything")
	}
}

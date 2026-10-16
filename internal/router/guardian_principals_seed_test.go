package router

import (
	"log/slog"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
)

func TestSeedGuardianPrincipal(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))

	// The router seeds the principal ID that Guardian's registry, reconciler,
	// results processor and observed collector claim for their writes.
	if err := r.SeedGuardianPrincipal(GuardianControlPlanePrincipalID, "seed-token", "control-plane", "Guardian control plane", ""); err != nil {
		t.Fatalf("SeedGuardianPrincipal: %v", err)
	}

	principal, ok := r.authenticateGuardianToken("seed-token")
	if !ok {
		t.Fatal("seeded token must authenticate")
	}
	if principal.PrincipalID != GuardianControlPlanePrincipalID || principal.Role != "control-plane" {
		t.Fatalf("principal = %+v", principal)
	}

	// Guardian's registry writes claim "guardiand"; the seeded principal must
	// authorize that exact claim on any router pod.
	principal, ok = r.authenticateGuardianMutation("seed-token", &pb.GuardianMutationContext{PrincipalId: GuardianControlPlanePrincipalID})
	if !ok {
		t.Fatal("seeded token must authenticate the guardian control-plane claim")
	}
	if principal.PrincipalID != GuardianControlPlanePrincipalID {
		t.Fatalf("authenticated principal = %q", principal.PrincipalID)
	}

	// Other Guardian components claim ids that are not client registrations
	// (for example "guardian-repo-import"); the seeded operator token must
	// still authenticate, as the control-plane identity.
	principal, ok = r.authenticateGuardianMutation("seed-token", &pb.GuardianMutationContext{PrincipalId: "guardian-repo-import"})
	if !ok {
		t.Fatal("seeded token must authenticate unregistered Guardian claims")
	}
	if principal.PrincipalID != GuardianControlPlanePrincipalID || principal.Role != "control-plane" {
		t.Fatalf("fallback principal = %+v, want the seeded control-plane identity", principal)
	}

	// Token-only authentication resolves to the seeded identity as well.
	if principal, ok := r.authenticateGuardianToken("seed-token"); !ok || principal.PrincipalID != GuardianControlPlanePrincipalID {
		t.Fatalf("authenticateGuardianToken(seed-token) = (%+v, %v)", principal, ok)
	}

	if _, ok := r.authenticateGuardianToken("wrong-token"); ok {
		t.Fatal("unknown token must not authenticate")
	}
}

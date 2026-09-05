package router

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/radryc/monofs/pkg/authz"
)

func TestGuardianPrincipalGrantsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := newGuardianPrincipalStore(dir)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := store.upsertConnectedClient("guardian-doctor", "tok-1", "", "Doctor", ""); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	grants := []authz.Grant{
		{Subject: "guardian-doctor", Partition: "doctor", Role: authz.RoleMaintainer},
	}
	if err := store.setGrants("guardian-doctor", grants); err != nil {
		t.Fatalf("setGrants: %v", err)
	}

	// Reload from disk and confirm grants persisted.
	reloaded, err := newGuardianPrincipalStore(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := reloaded.grantsFor("guardian-doctor")
	if len(got) != 1 || got[0].Partition != "doctor" || got[0].Role != authz.RoleMaintainer {
		t.Fatalf("grants not persisted: %+v", got)
	}

	// The derived evaluator authorizes the granted principal on its partition.
	eval := reloaded.grantEvaluator()
	id := authz.Identity{ClientID: "guardian-doctor"}
	if !eval.Can(context.Background(), id, "doctor", authz.ActionModify) {
		t.Error("expected maintainer to modify doctor")
	}
	if eval.Can(context.Background(), id, "monitoring", authz.ActionModify) {
		t.Error("did not expect authorization on monitoring")
	}
}

func TestGuardianPrincipalLegacyJSONLoads(t *testing.T) {
	dir := t.TempDir()
	// Legacy principals file without the "grants" field must still load.
	legacy := `[{"principal_id":"guardian-legacy","token_hash":"abc","role":"control-plane","display_name":"Legacy","created_at":1,"disabled":false}]`
	if err := os.WriteFile(filepath.Join(dir, "guardian_principals.json"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	store, err := newGuardianPrincipalStore(dir)
	if err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	if grants := store.grantsFor("guardian-legacy"); grants != nil {
		t.Fatalf("legacy principal should have no grants, got %+v", grants)
	}
	// setGrants on an unknown principal errors.
	if err := store.setGrants("nope", nil); err == nil {
		t.Error("expected error for unknown principal")
	}
}

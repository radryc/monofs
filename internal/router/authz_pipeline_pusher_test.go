package router

import "testing"

func TestAuthorizeGuardianMutationPipelineRole(t *testing.T) {
	p := &guardianPrincipal{PrincipalID: "monofs-pipeline", Role: "pipeline"}

	for _, path := range []string{
		"/.pipelines/ci.yaml",
		"/.queues/pipeline/run-1/tasks/task-1.json",
	} {
		if err := authorizeGuardianMutation(p, path, false); err != nil {
			t.Fatalf("pipeline role denied %q: %v", path, err)
		}
	}

	for _, path := range []string{
		"/partitions/genomics/intents/x.yaml",
		"/.scans/acme/.claims/t.json",
		"/doctor/v1/x.json",
	} {
		if err := authorizeGuardianMutation(p, path, false); err == nil {
			t.Fatalf("pipeline role should be denied %q", path)
		}
	}
}

func TestAuthorizeGuardianMutationPusherRequiresNamespacePrefix(t *testing.T) {
	// A pusher whose PrincipalID lacks the namespace prefix must not inherit a
	// cross-namespace bypass.
	unscoped := &guardianPrincipal{PrincipalID: "docker-pusher", Role: "pusher"}
	if err := authorizeGuardianMutation(unscoped, "/.scans/acme/.claims/t.json", false); err == nil {
		t.Fatal("prefixless pusher should be denied")
	}
	if err := authorizeGuardianMutation(unscoped, "/.queues/acme/tasks/t.json", false); err == nil {
		t.Fatal("prefixless pusher should be denied for queues")
	}

	// A properly namespaced pusher may only touch its own namespace.
	scoped := &guardianPrincipal{PrincipalID: "guardian-pusher-acme", Role: "pusher"}
	if err := authorizeGuardianMutation(scoped, "/.scans/acme/.claims/t.json", false); err != nil {
		t.Fatalf("scoped pusher denied own claims: %v", err)
	}
	if err := authorizeGuardianMutation(scoped, "/.queues/acme/.state/x", false); err != nil {
		t.Fatalf("scoped pusher denied own state: %v", err)
	}
	if err := authorizeGuardianMutation(scoped, "/.scans/other/.claims/t.json", false); err == nil {
		t.Fatal("scoped pusher must not touch another namespace")
	}
	if err := authorizeGuardianMutation(scoped, "/partitions/payments/.state/x", false); err == nil {
		t.Fatal("scoped pusher must not touch a foreign .state path")
	}
}

package policy

import (
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/authz"
)

const quotaPolicyYAML = `
version: 1
defaults:
  partitions: allow
  logs: allow
  traces: allow
quota_profiles:
  standard:
    query:
      requestsPerSecond: 50
      maxConcurrent: 10
  premium:
    query:
      requestsPerSecond: 200
      maxConcurrent: 40
quota_defaults:
  query:
    requestsPerSecond: 20
    maxConcurrent: 4
quotas:
  users:
    alice:
      query:
        requestsPerSecond: 100
        maxConcurrent: 12
  groups:
    platform:
      profile: premium
`

func quotaCatalog() *Catalog {
	return &Catalog{Partitions: []Partition{
		{
			Name: "checkout",
			Quota: &Quota{
				Profile: "standard",
				Query:   &QueryQuota{MaxConcurrent: 20},
			},
		},
		{Name: "payments"},
	}}
}

func TestCompileQuotaProfilesAndOverrides(t *testing.T) {
	file, err := ParseFile([]byte(quotaPolicyYAML))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	bundle, issues, err := Compile(file, quotaCatalog(), time.Now())
	if err != nil {
		t.Fatalf("Compile: %v (issues=%#v)", err, issues)
	}
	if bundle.Quotas == nil {
		t.Fatal("expected quota bundle")
	}
	if got := bundle.Quotas.Defaults.Query.RequestsPerSecond; got != 20 {
		t.Fatalf("defaults rps = %v, want 20", got)
	}
	checkout, ok := bundle.Quotas.Partitions["checkout"]
	if !ok {
		t.Fatal("missing checkout quota")
	}
	if checkout.Query.RequestsPerSecond != 50 {
		t.Fatalf("checkout rps = %v, want 50", checkout.Query.RequestsPerSecond)
	}
	if checkout.Query.MaxConcurrent != 20 {
		t.Fatalf("checkout concurrency = %v, want 20 (partition override)", checkout.Query.MaxConcurrent)
	}
	alice, ok := bundle.Quotas.Subjects["user:alice"]
	if !ok {
		t.Fatal("missing user:alice quota")
	}
	if alice.Query.RequestsPerSecond != 100 || alice.Query.MaxConcurrent != 12 {
		t.Fatalf("alice quota = %#v", alice.Query)
	}
	platform, ok := bundle.Quotas.Subjects["group:platform"]
	if !ok {
		t.Fatal("missing group:platform quota")
	}
	if platform.Query.RequestsPerSecond != 200 || platform.Query.MaxConcurrent != 40 {
		t.Fatalf("platform quota = %#v", platform.Query)
	}
}

func TestCompileQuotaUnknownProfileFails(t *testing.T) {
	file := &File{
		Version: 1,
		QuotaProfiles: map[string]Quota{
			"standard": {Query: &QueryQuota{RequestsPerSecond: 1}},
		},
	}
	catalog := &Catalog{Partitions: []Partition{
		{Name: "checkout", Quota: &Quota{Profile: "does-not-exist"}},
	}}
	if _, _, err := Compile(file, catalog, time.Now()); err == nil {
		t.Fatal("expected unknown profile to fail compilation")
	}
}

func TestSubjectQuotasMatchUserGroupAndClient(t *testing.T) {
	file, err := ParseFile([]byte(quotaPolicyYAML))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	bundle, _, err := Compile(file, quotaCatalog(), time.Now())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	pol := New(*bundle)
	id := authz.Identity{Subject: "alice", Groups: []string{"platform"}, ClientID: "grafana"}
	scoped := pol.SubjectQuotas(id)
	keys := map[string]bool{}
	for _, entry := range scoped {
		keys[entry.Key] = true
	}
	for _, want := range []string{"user:alice", "group:platform"} {
		if !keys[want] {
			t.Fatalf("missing %q in %#v", want, scoped)
		}
	}
	if keys["client:grafana"] {
		t.Fatal("client quota should be absent when not configured")
	}
}

func TestQuotaForPartitionFallsBackToDefaults(t *testing.T) {
	file, err := ParseFile([]byte(quotaPolicyYAML))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	bundle, _, err := Compile(file, quotaCatalog(), time.Now())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	pol := New(*bundle)
	got := pol.QuotaForPartition("payments")
	if got.Query == nil || got.Query.RequestsPerSecond != 20 {
		t.Fatalf("payments quota = %#v, want defaults", got)
	}
}

func TestBuiltinQuotaProfilesResolveWithoutDeclaration(t *testing.T) {
	names := QuotaProfileNames()
	if len(names) < 2 {
		t.Fatalf("built-in profile names = %#v", names)
	}
	for _, name := range names {
		resolved, err := ResolveQuota(Quota{Profile: name}, nil)
		if err != nil {
			t.Fatalf("ResolveQuota(%q): %v", name, err)
		}
		if resolved.Profile != name {
			t.Fatalf("profile = %q, want %q", resolved.Profile, name)
		}
		if resolved.Ingest == nil || resolved.Ingest.Traces == nil || resolved.Ingest.Metrics == nil {
			t.Fatalf("%q missing ingest envelope: %#v", name, resolved)
		}
		if resolved.Query == nil || resolved.Query.RequestsPerSecond <= 0 {
			t.Fatalf("%q missing query envelope: %#v", name, resolved)
		}
		if resolved.Storage == nil || resolved.Storage.MaxBytes <= 0 {
			t.Fatalf("%q missing storage envelope: %#v", name, resolved)
		}
	}
}

func TestBuiltinQuotaProfileOverriddenByLocalDefinition(t *testing.T) {
	local := map[string]Quota{
		QuotaProfileSmall: {Query: &QueryQuota{RequestsPerSecond: 7}},
	}
	resolved, err := ResolveQuota(Quota{Profile: QuotaProfileSmall}, local)
	if err != nil {
		t.Fatalf("ResolveQuota: %v", err)
	}
	if resolved.Query == nil || resolved.Query.RequestsPerSecond != 7 {
		t.Fatalf("local profile did not win: %#v", resolved.Query)
	}
}

func TestUnknownQuotaProfileNamesBuiltins(t *testing.T) {
	if _, err := ResolveQuota(Quota{Profile: "does-not-exist"}, nil); err == nil {
		t.Fatal("expected an unknown-profile error")
	}
}

func TestCompilePartitionUsesBuiltinProfile(t *testing.T) {
	file, err := ParseFile([]byte(`
version: 1
defaults:
  partitions: allow
  logs: allow
  traces: allow
`))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	catalog := &Catalog{Partitions: []Partition{{
		Name:  "small-app",
		Quota: &Quota{Profile: QuotaProfileSmall},
	}}}
	bundle, _, err := Compile(file, catalog, time.Now())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	pol := New(*bundle)
	got := pol.QuotaForPartition("small-app")
	if got.Query == nil || got.Query.RequestsPerSecond != 10 {
		t.Fatalf("quota = %#v, want the small class query envelope", got.Query)
	}
	if got.Ingest == nil || got.Ingest.Traces == nil || got.Ingest.Traces.BytesPerSecond != 1<<20 {
		t.Fatalf("quota = %#v, want the small class ingest envelope", got.Ingest)
	}
}

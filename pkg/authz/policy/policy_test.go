package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/authz"
)

func testCatalog() *Catalog {
	return &Catalog{Partitions: []Partition{
		{Name: "aws-x", Labels: map[string]string{"team": "payments", "platform": "aws", "aws.account": "123456789012", "aws.region": "us-east-1"}},
		{Name: "k8s-z", Labels: map[string]string{"team": "payments", "platform": "k8s"}},
		{Name: "k8s-other", Labels: map[string]string{"team": "infra", "platform": "k8s"}},
	}}
}

const testPolicyYAML = `
version: 1
defaults:
  partitions: deny
  logs: deny
  traces: deny
  monofs: deny
selectors:
  payments: { labels: { team: payments } }
groups:
  sre:
    partitions: ["*"]
    logs: ["*"]
    traces: ["*"]
    monofs: ["*"]
    role: maintainer
  team-payments:
    partitions: [payments]
    logs: [payments]
    traces: [payments]
    role: viewer
users:
  foo@acme.com:
    partitions: [aws-x, k8s-z]
    logs: [aws-x]
    traces: [k8s-z]
    role: viewer
  contractor@vendor.com:
    partitions: [aws-x]
    logs: [aws-x]
    deny: [logs]
  admin@acme.com:
    partitions: ["*"]
    logs: ["*"]
    traces: ["*"]
    monofs: ["*"]
    role: admin
clients:
  guardiand:
    partitions: ["*"]
    role: admin
`

func compileTestPolicy(t *testing.T, yaml string, catalog *Catalog) *Policy {
	t.Helper()
	file, err := ParseFile([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	bundle, issues, err := Compile(file, catalog, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("Compile: %v (issues: %+v)", err, issues)
	}
	return New(*bundle)
}

func identity(email string, groups ...string) authz.Identity {
	return authz.Identity{Subject: "sub-" + email, Email: email, Groups: groups}
}

func TestUserExampleVisibility(t *testing.T) {
	p := compileTestPolicy(t, testPolicyYAML, testCatalog())
	foo := identity("foo@acme.com")

	if !p.CanSeePartition(foo, "aws-x") || !p.CanSeePartition(foo, "k8s-z") {
		t.Fatal("foo should see aws-x and k8s-z")
	}
	if p.CanSeePartition(foo, "k8s-other") {
		t.Fatal("foo must not see k8s-other")
	}
	if !p.CanReadSignal(foo, "aws-x", SignalLogs) {
		t.Fatal("foo should read logs from aws-x")
	}
	if p.CanReadSignal(foo, "k8s-z", SignalLogs) {
		t.Fatal("foo must not read logs from k8s-z")
	}
	if !p.CanReadSignal(foo, "k8s-z", SignalTraces) {
		t.Fatal("foo should read traces from k8s-z")
	}
	if p.CanReadSignal(foo, "aws-x", SignalTraces) {
		t.Fatal("foo must not read traces from aws-x")
	}
	if !p.CanReadSignal(foo, "k8s-other", SignalMetrics) {
		t.Fatal("metrics are always visible")
	}
	if p.CanReadPath(foo, "github.com/acme/payments-api") {
		t.Fatal("foo has no monofs grant")
	}
}

func TestAnonymousAndAdmin(t *testing.T) {
	p := compileTestPolicy(t, testPolicyYAML, testCatalog())

	var anon authz.Identity
	if p.CanSeePartition(anon, "aws-x") {
		t.Fatal("anonymous must not see partitions")
	}
	if !p.CanReadSignal(anon, "aws-x", SignalMetrics) {
		t.Fatal("metrics are visible to anonymous callers")
	}

	admin := identity("admin@acme.com")
	if !p.IsAdmin(admin) {
		t.Fatal("admin@acme.com should be admin")
	}
	if !p.CanSeePartition(admin, "k8s-other") || !p.CanReadSignal(admin, "k8s-other", SignalLogs) {
		t.Fatal("admin should see everything")
	}
	names, all := p.VisiblePartitions(admin)
	if !all || names != nil {
		t.Fatalf("admin visibility should be wildcard, got %v all=%v", names, all)
	}
}

func TestGroupGrantsAndSelectors(t *testing.T) {
	p := compileTestPolicy(t, testPolicyYAML, testCatalog())
	member := identity("dev@acme.com", "team-payments")

	if !p.CanSeePartition(member, "aws-x") || !p.CanSeePartition(member, "k8s-z") {
		t.Fatal("team-payments should see both payments partitions via selector")
	}
	if p.CanSeePartition(member, "k8s-other") {
		t.Fatal("team-payments must not see infra partitions")
	}
	if !p.CanReadSignal(member, "k8s-z", SignalLogs) {
		t.Fatal("team-payments selector should grant logs on all payments partitions")
	}
	if p.CanReadSignal(member, "k8s-other", SignalLogs) {
		t.Fatal("team-payments must not read logs from infra partitions")
	}
	if role := p.Role(member, "aws-x"); role != authz.RoleViewer {
		t.Fatalf("expected viewer role, got %q", role)
	}

	sre := identity("ops@acme.com", "sre")
	if role := p.Role(sre, "anything"); role != authz.RoleMaintainer {
		t.Fatalf("expected maintainer role for sre, got %q", role)
	}
	if !p.CanReadPath(sre, "github.com/anything/at-all") {
		t.Fatal("sre monofs wildcard should allow any path")
	}
}

func TestDenyOverrides(t *testing.T) {
	p := compileTestPolicy(t, testPolicyYAML, testCatalog())
	contractor := identity("contractor@vendor.com", "team-payments")

	// The user grant allows partitions aws-x and logs aws-x, but deny: [logs]
	// must win even though the group grant allows logs.
	if !p.CanSeePartition(contractor, "aws-x") {
		t.Fatal("contractor should still see aws-x")
	}
	if p.CanReadSignal(contractor, "aws-x", SignalLogs) {
		t.Fatal("contractor deny must override the group logs grant")
	}
}

func TestVisiblePartitionsExplicit(t *testing.T) {
	p := compileTestPolicy(t, testPolicyYAML, testCatalog())
	names, all := p.VisiblePartitions(identity("foo@acme.com"))
	if all {
		t.Fatal("foo should not have wildcard visibility")
	}
	if strings.Join(names, ",") != "aws-x,k8s-z" {
		t.Fatalf("unexpected visible partitions: %v", names)
	}
	resolved := p.ResolveVisiblePartitions(identity("foo@acme.com"), []string{"aws-x", "k8s-z", "k8s-other"})
	if strings.Join(resolved, ",") != "aws-x,k8s-z" {
		t.Fatalf("unexpected resolved visibility: %v", resolved)
	}
}

func TestClientIdentity(t *testing.T) {
	p := compileTestPolicy(t, testPolicyYAML, testCatalog())
	guardiand := authz.Identity{ClientID: "guardiand"}
	if !p.IsAdmin(guardiand) {
		t.Fatal("guardiand client should be admin")
	}
}

func TestCanReadPathPatterns(t *testing.T) {
	yaml := `
version: 1
defaults: { partitions: deny, logs: deny, traces: deny, monofs: deny }
users:
  dev@acme.com:
    monofs: ["github.com/acme/payments-api", "github.com/acme/payments-*", "gitlab.com/org/mono/**"]
`
	p := compileTestPolicy(t, yaml, nil)
	dev := identity("dev@acme.com")

	if !p.CanReadPath(dev, "github.com/acme/payments-api") {
		t.Fatal("exact match should be allowed")
	}
	if !p.CanReadPath(dev, "github.com/acme/payments-worker") {
		t.Fatal("glob match should be allowed")
	}
	if p.CanReadPath(dev, "github.com/acme/orders-api") {
		t.Fatal("unrelated repo must be denied")
	}
	if !p.CanReadPath(dev, "gitlab.com/org/mono/sub/dir") {
		t.Fatal("prefix/** should allow subtree")
	}
}

func TestAWSBindings(t *testing.T) {
	p := compileTestPolicy(t, testPolicyYAML, testCatalog())
	partition, ok := p.PartitionForAWS("123456789012", "us-east-1")
	if !ok || partition != "aws-x" {
		t.Fatalf("expected aws-x binding, got %q ok=%v", partition, ok)
	}
	if _, ok := p.PartitionForAWS("999999999999", "eu-west-1"); ok {
		t.Fatal("unbound account/region must not resolve")
	}
	if len(p.AWSBindings()) != 1 {
		t.Fatalf("expected 1 derived binding, got %d", len(p.AWSBindings()))
	}
}

func TestCompileDeterministic(t *testing.T) {
	file, err := ParseFile([]byte(testPolicyYAML))
	if err != nil {
		t.Fatal(err)
	}
	b1, _, err := Compile(file, testCatalog(), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	b2, _, err := Compile(file, testCatalog(), time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if b1.Version != b2.Version {
		t.Fatalf("version must not depend on generation time: %s vs %s", b1.Version, b2.Version)
	}
	if !strings.HasPrefix(b1.Version, "sha256:") {
		t.Fatalf("unexpected version format: %s", b1.Version)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"version", "version: 2\n", "unsupported version"},
		{"unknown field", "version: 1\nfoo: bar\n", "field foo not found"},
		{"metrics scope", "version: 1\ndefaults: { metrics: allow }\n", "always visible"},
		{"selector labels", "version: 1\nselectors: { empty: { labels: {} } }\n", "no labels"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFile([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCompileValidation(t *testing.T) {
	file, err := ParseFile([]byte(`
version: 1
defaults: { partitions: deny }
users:
  bad@acme.com:
    partitions: [ghost-partition]
    role: superuser
`))
	if err != nil {
		t.Fatal(err)
	}
	bundle, issues, err := Compile(file, testCatalog(), time.Unix(0, 0))
	if err == nil {
		t.Fatal("expected validation error for invalid role")
	}
	if bundle != nil {
		t.Fatal("bundle must not be produced on error")
	}
	found := map[string]bool{}
	for _, issue := range issues {
		found[issue.Severity] = true
		if issue.Severity == "warning" && strings.Contains(issue.Message, "ghost-partition") {
			found["ghost"] = true
		}
	}
	if !found["error"] || !found["warning"] || !found["ghost"] {
		t.Fatalf("expected error+warning issues, got %+v", issues)
	}
}

func TestImportsMerge(t *testing.T) {
	base := `
version: 1
defaults: { partitions: deny, logs: deny, traces: deny, monofs: deny }
groups:
  sre:
    partitions: ["*"]
    role: maintainer
`
	env := `
version: 1
imports: [../base.yaml]
users:
  foo@acme.com:
    partitions: [aws-x]
`
	read := func(rel string) ([]byte, error) {
		if rel == "../base.yaml" {
			return []byte(base), nil
		}
		return nil, &notFound{rel}
	}
	file, warnings, err := LoadFile([]byte(env), "envs/dev.yaml", read)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if _, ok := file.Groups["sre"]; !ok {
		t.Fatal("base groups must be imported")
	}
	if _, ok := file.Users["foo@acme.com"]; !ok {
		t.Fatal("env users must be present")
	}

	// Cycle detection: two files importing each other.
	a := "version: 1\nimports: [b.yaml]\n"
	b := "version: 1\nimports: [a.yaml]\n"
	_, _, err = LoadFile([]byte(a), "a.yaml", func(rel string) ([]byte, error) {
		switch rel {
		case "b.yaml":
			return []byte(b), nil
		case "a.yaml":
			return []byte(a), nil
		}
		return nil, &notFound{rel}
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

type notFound struct{ path string }

func (e *notFound) Error() string { return "not found: " + e.path }

func TestBundleRoundTrip(t *testing.T) {
	p := compileTestPolicy(t, testPolicyYAML, testCatalog())
	data, err := MarshalPolicy(p)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Version() != p.Version() {
		t.Fatalf("version mismatch after round trip: %s vs %s", parsed.Version(), p.Version())
	}
	foo := identity("foo@acme.com")
	if !parsed.CanReadSignal(foo, "aws-x", SignalLogs) {
		t.Fatal("round-tripped policy must evaluate identically")
	}
}

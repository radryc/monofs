package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/audit"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const accessTestPolicyYAML = `
version: 1
defaults:
  partitions: deny
  logs: deny
  traces: deny
  monofs: deny
users:
  foo@example.com:
    partitions: [aws-x]
    monofs: ["github.com/acme/payments-api"]
  admin@example.com:
    partitions: ["*"]
    monofs: ["*"]
    role: admin
`

func accessTestPolicy(t *testing.T) *policy.Policy {
	t.Helper()
	file, err := policy.ParseFile([]byte(accessTestPolicyYAML))
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, err := policy.Compile(file, nil, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	return policy.New(*bundle)
}

func TestPolicyAllowsPath(t *testing.T) {
	p := accessTestPolicy(t)
	foo := authz.Identity{Subject: "foo", Email: "foo@example.com"}
	admin := authz.Identity{Subject: "admin", Email: "admin@example.com"}
	var anon authz.Identity

	cases := []struct {
		name string
		id   authz.Identity
		path string
		want bool
	}{
		{"repo file allowed", foo, "github.com/acme/payments-api/src/main.go", true},
		{"repo file denied", foo, "github.com/acme/orders-api/src/main.go", false},
		{"guardian partition allowed", foo, "guardian/aws-x/config.yaml", true},
		{"guardian partition denied", foo, "guardian/k8s-z/config.yaml", false},
		{"doctor namespace for authenticated", foo, "doctor/v1/query/x/results.json", true},
		{"dependency for authenticated", foo, "dependency/some/pkg/file.go", true},
		{"root passes", foo, "", true},
		{"intermediate path passes", foo, "github.com/acme", true},
		{"guardian-system requires admin", foo, "guardian-system/.access/envs/dev.yaml", false},
		{"guardian-system admin", admin, "guardian-system/.access/envs/dev.yaml", true},
		{"admin sees repo", admin, "github.com/anything/at-all", true},
		{"anonymous repo denied", anon, "github.com/acme/payments-api/x.go", false},
		{"anonymous doctor denied", anon, "doctor/v1/query/x", false},
		{"anonymous dependency denied", anon, "dependency/x", false},
	}
	for _, tc := range cases {
		if got := PolicyAllowsPath(p, tc.id, tc.path); got != tc.want {
			t.Fatalf("%s: PolicyAllowsPath(%q) = %v, want %v", tc.name, tc.path, got, tc.want)
		}
	}
}

func TestAccessControlAuthorizeRead(t *testing.T) {
	file, err := policy.ParseFile([]byte(accessTestPolicyYAML))
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, err := policy.Compile(file, nil, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	data, err := policy.MarshalBundle(*bundle)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	ac := NewAccessControl(AccessControlOptions{
		PolicyFile:    path,
		InternalToken: "cluster-secret",
		Enforce:       true,
		Interval:      time.Minute,
	})
	if ac == nil {
		t.Fatal("expected access control")
	}
	if err := ac.loader.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	fooCtx := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "foo", Email: "foo@example.com"})
	if err := ac.AuthorizeRead(fooCtx, "github.com/acme/payments-api/x.go"); err != nil {
		t.Fatalf("allowed repo read rejected: %v", err)
	}
	if err := ac.AuthorizeRead(fooCtx, "github.com/acme/orders-api/x.go"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("denied repo read: got %v, want PermissionDenied", err)
	}

	// Anonymous fails closed under enforcement.
	if err := ac.AuthorizeRead(context.Background(), "github.com/acme/payments-api/x.go"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("anonymous read: got %v, want PermissionDenied", err)
	}

	// Internal calls bypass even without identity.
	internalCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-monofs-internal", "cluster-secret"))
	internalCtx = ac.authenticate(internalCtx)
	if !isInternalCall(internalCtx) {
		t.Fatal("internal token should mark the call internal")
	}
	if err := ac.AuthorizeRead(internalCtx, "github.com/acme/orders-api/x.go"); err != nil {
		t.Fatalf("internal read must bypass policy: %v", err)
	}

	// Break-glass subject bypasses.
	bgCtx := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "break-glass-admin"})
	if err := ac.AuthorizeRead(bgCtx, "github.com/acme/orders-api/x.go"); err != nil {
		t.Fatalf("break-glass read must bypass policy: %v", err)
	}

	// Break-glass ClientID (e.g., from authz.NewBreakGlassVerifier) also bypasses.
	bgClientCtx := authz.ContextWithIdentity(context.Background(), authz.Identity{ClientID: "break-glass-admin"})
	if err := ac.AuthorizeRead(bgClientCtx, "github.com/acme/orders-api/x.go"); err != nil {
		t.Fatalf("break-glass ClientID read must bypass policy: %v", err)
	}

	// Non-break-glass ClientID must not bypass.
	otherClientCtx := authz.ContextWithIdentity(context.Background(), authz.Identity{ClientID: "regular-svc"})
	if err := ac.AuthorizeRead(otherClientCtx, "github.com/acme/orders-api/x.go"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-break-glass ClientID read: got %v, want PermissionDenied", err)
	}
}

func TestAccessControlUnaryInterceptorAttachesIdentity(t *testing.T) {
	ac := NewAccessControl(AccessControlOptions{
		PolicyFile: filepath.Join(t.TempDir(), "missing.json"),
		Enforce:    true,
	})
	if ac == nil {
		t.Fatal("expected access control")
	}
	interceptor := ac.UnaryInterceptor()
	var seen authz.Identity
	_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{},
		func(ctx context.Context, _ any) (any, error) {
			seen, _ = authz.IdentityFromContext(ctx)
			return nil, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if !seen.IsAnonymous() {
		t.Fatalf("expected anonymous identity, got %+v", seen)
	}
}

func readWALEvents(t *testing.T, walPath string) []audit.Event {
	t.Helper()
	raw, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("read WAL %q: %v", walPath, err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	events := make([]audit.Event, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev audit.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unmarshal WAL event: %v", err)
		}
		events = append(events, ev)
	}
	return events
}

func TestAccessControlAuthorizeReadEmitsAuditEvents(t *testing.T) {
	file, err := policy.ParseFile([]byte(accessTestPolicyYAML))
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, err := policy.Compile(file, nil, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	data, err := policy.MarshalBundle(*bundle)
	if err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policyPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	ac := NewAccessControl(AccessControlOptions{
		PolicyFile: policyPath,
		Enforce:    true,
		Interval:   time.Minute,
	})
	if ac == nil {
		t.Fatal("expected access control")
	}
	if err := ac.loader.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	walPath := filepath.Join(t.TempDir(), "audit.wal")
	emitter, err := audit.NewWALSpoolEmitter(walPath, audit.WithFailClosed(true))
	if err != nil {
		t.Fatalf("NewWALSpoolEmitter: %v", err)
	}
	defer emitter.Close()
	ac.SetAuditEmitter(emitter)

	fooCtx := authz.ContextWithIdentity(context.Background(), authz.Identity{
		Subject:  "foo-sub",
		Email:    "foo@example.com",
		ClientID: "cli-reader",
	})
	allowedResource := "github.com/acme/payments-api/x.go"
	if err := ac.AuthorizeRead(fooCtx, allowedResource); err != nil {
		t.Fatalf("allowed repo read rejected: %v", err)
	}

	deniedResource := "github.com/acme/orders-api/x.go"
	if err := ac.AuthorizeRead(fooCtx, deniedResource); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("denied repo read: got %v, want PermissionDenied", err)
	}

	// UnaryInterceptor should also emit allow/deny decisions.
	interceptor := ac.UnaryInterceptor()
	if _, err := interceptor(fooCtx, nil, &grpc.UnaryServerInfo{FullMethod: "/monofs.MonoFS/Read"},
		func(_ context.Context, _ any) (any, error) {
			return nil, nil
		}); err != nil {
		t.Fatalf("interceptor allow: %v", err)
	}
	if _, err := interceptor(fooCtx, nil, &grpc.UnaryServerInfo{FullMethod: "/monofs.MonoFS/Write"},
		func(_ context.Context, _ any) (any, error) {
			return nil, status.Error(codes.PermissionDenied, "denied")
		}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("interceptor deny: got %v, want PermissionDenied", err)
	}

	if err := emitter.Close(); err != nil {
		t.Fatalf("close emitter: %v", err)
	}

	count, err := audit.VerifyChain(walPath)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if count != 4 {
		t.Fatalf("VerifyChain count = %d, want 4", count)
	}

	events := readWALEvents(t, walPath)
	if len(events) != 4 {
		t.Fatalf("got %d WAL events, want 4", len(events))
	}

	if events[0].Action != audit.ActionRead || events[0].Decision != audit.DecisionAllow ||
		events[0].Resource != allowedResource || events[0].ActorSubject != "foo-sub" || events[0].ActorClientID != "cli-reader" {
		t.Fatalf("event[0] = %+v, want allow read for foo-sub/cli-reader on %q", events[0], allowedResource)
	}
	if events[1].Action != audit.ActionRead || events[1].Decision != audit.DecisionDeny ||
		events[1].Resource != deniedResource || events[1].ActorSubject != "foo-sub" || events[1].ActorClientID != "cli-reader" {
		t.Fatalf("event[1] = %+v, want deny read for foo-sub/cli-reader on %q", events[1], deniedResource)
	}
	if events[2].Action != audit.ActionAuth || events[2].Decision != audit.DecisionAllow ||
		events[2].Resource != "/monofs.MonoFS/Read" {
		t.Fatalf("event[2] = %+v, want allow auth for /monofs.MonoFS/Read", events[2])
	}
	if events[3].Action != audit.ActionAuth || events[3].Decision != audit.DecisionDeny ||
		events[3].Resource != "/monofs.MonoFS/Write" {
		t.Fatalf("event[3] = %+v, want deny auth for /monofs.MonoFS/Write", events[3])
	}
}

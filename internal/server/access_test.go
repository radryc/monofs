package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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

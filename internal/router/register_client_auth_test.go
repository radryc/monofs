package router

import (
	"context"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/pkg/authz"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func registerClientReq(clientID, token, principalID, role string) *pb.RegisterClientRequest {
	return &pb.RegisterClientRequest{
		ClientId: clientID,
		GuardianConfig: &pb.GuardianConfig{
			AuthToken:   token,
			PrincipalId: principalID,
			Role:        role,
			DisplayName: principalID,
		},
	}
}

func TestRegisterClientRejectsAnonymous(t *testing.T) {
	r := newAuthzTestRouter(t, false)

	resp, err := r.RegisterClient(context.Background(), registerClientReq(
		"guardian-pusher-anon", "anon-token", GuardianControlPlanePrincipalID, "control-plane"))
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("anonymous registration = (%v, %v), want PermissionDenied", resp, err)
	}
	if resp != nil {
		t.Fatalf("anonymous registration returned a response: %+v", resp)
	}
	if _, ok := r.validateGuardianToken("anon-token"); ok {
		t.Fatal("anonymous registration must not seed a guardian principal")
	}
}

func TestRegisterClientViewerCannotAssertControlPlane(t *testing.T) {
	r := newAuthzTestRouter(t, false)
	ctx := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "reader@example.com"})

	resp, err := r.RegisterClient(ctx, registerClientReq(
		"guardian-pusher-viewer", "viewer-token", GuardianControlPlanePrincipalID, "control-plane"))
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer control-plane claim = (%v, %v), want PermissionDenied", resp, err)
	}
}

func TestRegisterClientViewerCannotChooseRole(t *testing.T) {
	r := newAuthzTestRouter(t, false)
	ctx := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "reader@example.com"})

	resp, err := r.RegisterClient(ctx, registerClientReq(
		"guardian-pusher-viewer", "viewer-token", "", "admin"))
	if err != nil {
		t.Fatalf("RegisterClient() error = %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("RegisterClient() failed: %+v", resp)
	}
	principal, ok := r.guardianPrincipals.validateTokenForPrincipal("viewer-token", "reader@example.com")
	if !ok {
		t.Fatal("viewer principal was not registered under the verified identity")
	}
	if principal.Role != string(authz.RoleViewer) {
		t.Fatalf("registered role = %q, want %q", principal.Role, authz.RoleViewer)
	}
}

func TestRegisterClientAdminCanAssertControlPlane(t *testing.T) {
	r := newAuthzTestRouter(t, false)
	ctx := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "admin@example.com"})

	resp, err := r.RegisterClient(ctx, registerClientReq(
		"guardianctl", "admin-token", GuardianControlPlanePrincipalID, "control-plane"))
	if err != nil {
		t.Fatalf("RegisterClient() error = %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("RegisterClient() failed: %+v", resp)
	}
	principal, ok := r.guardianPrincipals.validateTokenForPrincipal("admin-token", GuardianControlPlanePrincipalID)
	if !ok {
		t.Fatal("admin-asserted control-plane principal was not registered")
	}
	if principal.Role != "control-plane" {
		t.Fatalf("registered role = %q, want control-plane", principal.Role)
	}
}

func TestRegisterClientBreakGlassCanAssertControlPlane(t *testing.T) {
	r := newAuthzTestRouter(t, false)
	ctx := authz.ContextWithIdentity(context.Background(), authz.Identity{ClientID: "break-glass-admin"})

	resp, err := r.RegisterClient(ctx, registerClientReq(
		"guardiand", "break-glass-token", GuardianControlPlanePrincipalID, "control-plane"))
	if err != nil {
		t.Fatalf("RegisterClient() error = %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("RegisterClient() failed: %+v", resp)
	}
}

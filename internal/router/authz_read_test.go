package router

import (
	"context"
	"log/slog"
	"testing"

	"github.com/radryc/monofs/pkg/authz"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAuthorizeReadDisabledIsNoop(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	store, _ := authz.NewGrantStore("")
	r.SetGrantEvaluator(store, false)
	// AuthzEnforceRead defaults to false.
	if err := r.authorizeRead(context.Background(), "doctor"); err != nil {
		t.Fatalf("disabled read enforcement should be a no-op, got %v", err)
	}
}

func TestAuthorizeReadEnforcement(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	store, _ := authz.NewGrantStore("")
	_ = store.Add(authz.Grant{Subject: "reader", Partition: "doctor", Role: authz.RoleViewer})
	r.SetGrantEvaluator(store, false)
	r.SetAuthzEnforceRead(true)

	// Anonymous -> denied.
	if err := r.authorizeRead(context.Background(), "doctor"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("anonymous read: want PermissionDenied, got %v", err)
	}

	// Viewer -> allowed on its partition, denied elsewhere.
	ctx := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "reader"})
	if err := r.authorizeRead(ctx, "doctor"); err != nil {
		t.Fatalf("viewer read doctor: want nil, got %v", err)
	}
	if err := r.authorizeRead(ctx, "monitoring"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer read monitoring: want PermissionDenied, got %v", err)
	}
}

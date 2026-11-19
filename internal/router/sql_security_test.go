package router

import (
	"context"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/pkg/authz"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSQLRequiresPolicyByDefault(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), nil)
	defer r.Close()

	_, err := r.QuerySQL(context.Background(), &pb.QuerySQLRequest{
		Dataset: "logs",
		Query:   "SELECT count(*) AS n FROM logs",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("QuerySQL error = %v, want PermissionDenied", err)
	}

	_, err = r.QueryDuckDBSQL(context.Background(), &pb.QueryDuckDBSQLRequest{
		Dataset: "logs",
		Query:   "SELECT count(*) AS n FROM logs",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("QueryDuckDBSQL error = %v, want PermissionDenied", err)
	}
}

func TestSQLAdminBypassesPolicy(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), nil)
	defer r.Close()

	ctx := authz.ContextWithIdentity(context.Background(), authz.Identity{ClientID: "break-glass-admin"})
	_, err := r.QuerySQL(ctx, &pb.QuerySQLRequest{
		Dataset: "logs",
		Query:   "SELECT count(*) AS n FROM logs",
	})
	if status.Code(err) == codes.PermissionDenied {
		t.Fatalf("admin was denied: %v", err)
	}
	// No nodes are registered in this test; reaching Unavailable proves the
	// authorization gate let the call through.
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("error = %v, want Unavailable after the authorization gate", err)
	}
}

func TestSQLPolicyCanBeDisabledForDevelopment(t *testing.T) {
	cfg := DefaultRouterConfig()
	cfg.RequireSQLPolicy = false
	r := NewRouter(cfg, nil)
	defer r.Close()

	_, err := r.QuerySQL(context.Background(), &pb.QuerySQLRequest{
		Dataset: "logs",
		Query:   "SELECT count(*) AS n FROM logs",
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("error = %v, want Unavailable (no nodes) after opting out", err)
	}
}

package router

import (
	"context"
	"log/slog"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/pkg/authz"
)

func TestAccessAdminBreakGlassIdentities(t *testing.T) {
	r := newAuthzTestRouter(t, true)
	r.AddBreakGlassAdmin("configured-admin")

	tests := []struct {
		name string
		id   authz.Identity
		want bool
	}{
		{"subject break-glass", authz.Identity{Subject: "break-glass-admin"}, true},
		{"client id break-glass", authz.Identity{ClientID: "break-glass-admin"}, true},
		{"configured break-glass client", authz.Identity{ClientID: "configured-admin"}, true},
		{"policy admin", authz.Identity{Subject: "admin@example.com"}, true},
		{"viewer", authz.Identity{Subject: "reader@example.com"}, false},
		{"anonymous", authz.Identity{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.accessAdmin(tt.id); got != tt.want {
				t.Fatalf("accessAdmin() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAddBreakGlassAdmin(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	r.accessPolicy = newTestAccessPolicy(t, true)
	r.AddBreakGlassAdmin("break-glass-admin")

	id := authz.Identity{ClientID: "break-glass-admin"}
	ctx := authz.ContextWithIdentity(context.Background(), id)

	req := &pb.IngestRequest{IngestionType: pb.IngestionType_INGESTION_GIT, SourceId: "any-partition"}
	if err := r.authorizeIngest(ctx, req, "any-partition"); err != nil {
		t.Fatalf("break-glass ingest should be allowed: %v", err)
	}
}

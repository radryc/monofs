package router

import (
	"context"
	"log/slog"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/pkg/authz"
)

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

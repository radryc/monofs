package router

import (
	"context"
	"log/slog"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/pkg/authz"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newAuthzTestRouter(t *testing.T, enforce bool, grants ...authz.Grant) *Router {
	t.Helper()
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	store, err := authz.NewGrantStore("")
	if err != nil {
		t.Fatalf("new grant store: %v", err)
	}
	for _, g := range grants {
		if err := store.Add(g); err != nil {
			t.Fatalf("add grant: %v", err)
		}
	}
	r.SetGrantEvaluator(store, enforce)
	return r
}

func TestIngestPartitionForGrant(t *testing.T) {
	tests := []struct {
		name        string
		req         *pb.IngestRequest
		displayPath string
		want        string
	}{
		{"guardian uses source id", &pb.IngestRequest{IngestionType: pb.IngestionType_INGESTION_GUARDIAN, SourceId: "doctor"}, "guardian/doctor", "doctor"},
		{"git single segment", &pb.IngestRequest{IngestionType: pb.IngestionType_INGESTION_GIT, SourceId: "team-a"}, "team-a", "team-a"},
		{"git multi segment top", &pb.IngestRequest{IngestionType: pb.IngestionType_INGESTION_GIT}, "github_com/owner/repo", "github_com"},
		{"guardian prefix stripped", &pb.IngestRequest{IngestionType: pb.IngestionType_INGESTION_GIT}, "guardian/monitoring", "monitoring"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ingestPartitionForGrant(tt.req, tt.displayPath); got != tt.want {
				t.Errorf("ingestPartitionForGrant = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAuthorizeIngestEnforcement(t *testing.T) {
	r := newAuthzTestRouter(t, true,
		authz.Grant{Subject: "ci-bot", Partition: "team-a", Role: authz.RoleIngester})

	gitReq := func(partition string) *pb.IngestRequest {
		return &pb.IngestRequest{IngestionType: pb.IngestionType_INGESTION_GIT, SourceId: partition}
	}

	// No identity -> denied.
	if err := r.authorizeIngest(context.Background(), gitReq("team-a"), "team-a"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("anonymous ingest: want PermissionDenied, got %v", err)
	}

	// Granted principal -> allowed.
	ciCtx := authz.ContextWithIdentity(context.Background(), authz.Identity{ClientID: "ci-bot"})
	if err := r.authorizeIngest(ciCtx, gitReq("team-a"), "team-a"); err != nil {
		t.Fatalf("granted ingest: want nil, got %v", err)
	}

	// Granted principal, wrong partition -> denied.
	if err := r.authorizeIngest(ciCtx, gitReq("team-b"), "team-b"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("cross-partition ingest: want PermissionDenied, got %v", err)
	}
}

func TestAuthorizeIngestDisabledIsNoop(t *testing.T) {
	r := newAuthzTestRouter(t, false,
		authz.Grant{Subject: "ci-bot", Partition: "team-a", Role: authz.RoleIngester})
	// Enforcement off: even an anonymous caller passes.
	req := &pb.IngestRequest{IngestionType: pb.IngestionType_INGESTION_GIT, SourceId: "team-a"}
	if err := r.authorizeIngest(context.Background(), req, "team-a"); err != nil {
		t.Fatalf("disabled enforcement should be a no-op, got %v", err)
	}
}

func TestIngestRepositoryDeniedByAuthz(t *testing.T) {
	r := newAuthzTestRouter(t, true,
		authz.Grant{Subject: "ci-bot", Partition: "team-a", Role: authz.RoleIngester})

	// A principal without a grant for team-a must be rejected before any work.
	stream := &mockIngestStream{ctx: authz.ContextWithIdentity(context.Background(), authz.Identity{ClientID: "intruder"})}
	err := r.IngestRepository(&pb.IngestRequest{
		Source:        "https://github.com/example/repo",
		SourceId:      "team-a",
		IngestionType: pb.IngestionType_INGESTION_GIT,
	}, stream)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("IngestRepository: want PermissionDenied, got %v", err)
	}
}

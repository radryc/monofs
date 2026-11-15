package router

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newAuthzTestRouter(t *testing.T, enforce bool) *Router {
	t.Helper()
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	r.accessPolicy = newTestAccessPolicy(t, enforce)
	return r
}

func newTestAccessPolicy(t *testing.T, enforce bool) *accessPolicyState {
	t.Helper()
	const raw = `
version: 1
defaults:
  partitions: deny
  logs: deny
  traces: deny
  monofs: deny
clients:
  ci-bot:
    partitions: [team-a]
    role: ingester
users:
  reader@example.com:
    partitions: [doctor]
    logs: [doctor]
    role: viewer
  admin@example.com:
    partitions: ["*"]
    logs: ["*"]
    traces: ["*"]
    monofs: ["*"]
    role: admin
`
	file, err := policy.ParseFile([]byte(raw))
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	catalog := &policy.Catalog{Partitions: []policy.Partition{
		{Name: "team-a"},
		{Name: "team-b"},
		{Name: "doctor"},
	}}
	bundle, _, err := policy.Compile(file, catalog, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("compile policy: %v", err)
	}
	data, err := policy.MarshalPolicy(policy.New(*bundle))
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	state := &accessPolicyState{
		loader: policy.NewLoader(policy.FileSource{Path: path}, time.Hour, func(string, ...any) {}),
	}
	state.enforce.Store(enforce)
	if err := state.loader.Refresh(context.Background()); err != nil {
		t.Fatalf("load policy: %v", err)
	}
	return state
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
	r := newAuthzTestRouter(t, true)

	gitReq := func(partition string) *pb.IngestRequest {
		return &pb.IngestRequest{IngestionType: pb.IngestionType_INGESTION_GIT, SourceId: partition}
	}

	// No identity -> denied.
	if err := r.authorizeIngest(context.Background(), gitReq("team-a"), "team-a"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("anonymous ingest: want PermissionDenied, got %v", err)
	}

	// Ingester -> allowed.
	ciCtx := authz.ContextWithIdentity(context.Background(), authz.Identity{ClientID: "ci-bot"})
	if err := r.authorizeIngest(ciCtx, gitReq("team-a"), "team-a"); err != nil {
		t.Fatalf("granted ingest: want nil, got %v", err)
	}

	// Ingester, wrong partition -> denied.
	if err := r.authorizeIngest(ciCtx, gitReq("team-b"), "team-b"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("cross-partition ingest: want PermissionDenied, got %v", err)
	}
}

func TestAuthorizeIngestObserveModeIsNoop(t *testing.T) {
	r := newAuthzTestRouter(t, false)
	req := &pb.IngestRequest{IngestionType: pb.IngestionType_INGESTION_GIT, SourceId: "team-a"}
	if err := r.authorizeIngest(context.Background(), req, "team-a"); err != nil {
		t.Fatalf("observe mode should be a no-op, got %v", err)
	}
}

func TestAuthorizeIngestFailsClosedWithoutPolicy(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	r.accessPolicy = &accessPolicyState{}
	r.accessPolicy.enforce.Store(true)
	if err := r.authorizeIngest(context.Background(), &pb.IngestRequest{}, "team-a"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("missing policy under enforcement: want PermissionDenied, got %v", err)
	}
}

func TestIngestRepositoryDeniedByAuthz(t *testing.T) {
	r := newAuthzTestRouter(t, true)

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

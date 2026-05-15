package client

import (
	"testing"

	pb "github.com/radryc/monofs/api/proto"
)

func TestRefreshRequestForRepository(t *testing.T) {
	repo := WorkspaceRepository{
		DisplayPath: "github.com/acme/monofs",
		Source:      "git@example.com/acme/monofs.git",
		Ref:         "main",
	}

	req, err := refreshRequestForRepository(repo)
	if err != nil {
		t.Fatalf("refreshRequestForRepository() error = %v", err)
	}
	if req.GetSource() != repo.Source {
		t.Fatalf("request source = %q, want %q", req.GetSource(), repo.Source)
	}
	if req.GetRef() != repo.Ref {
		t.Fatalf("request ref = %q, want %q", req.GetRef(), repo.Ref)
	}
	if req.GetSourceId() != repo.DisplayPath {
		t.Fatalf("request source_id = %q, want %q", req.GetSourceId(), repo.DisplayPath)
	}
	if req.GetIngestionType() != pb.IngestionType_INGESTION_GIT {
		t.Fatalf("request ingestion_type = %v, want %v", req.GetIngestionType(), pb.IngestionType_INGESTION_GIT)
	}
	if req.GetFetchType() != pb.SourceType_SOURCE_TYPE_BLOB {
		t.Fatalf("request fetch_type = %v, want %v", req.GetFetchType(), pb.SourceType_SOURCE_TYPE_BLOB)
	}
}

func TestRefreshRequestForRepositoryRequiresSource(t *testing.T) {
	_, err := refreshRequestForRepository(WorkspaceRepository{DisplayPath: "github.com/acme/monofs"})
	if err == nil {
		t.Fatal("refreshRequestForRepository() error = nil, want source validation")
	}
}

package server

import (
	"context"
	"path/filepath"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
)

func TestRepairRepositoryNamespaceIndex(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, false, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if _, err := s.RegisterRepository(context.Background(), &pb.RegisterRepositoryRequest{
		StorageId:   "g1",
		DisplayPath: "guardian/genomics",
		Source:      "https://example.com/repo.git",
	}); err != nil {
		t.Fatalf("RegisterRepository: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := RepairRepositoryNamespaceIndex(dbPath); err != nil {
		t.Fatalf("RepairRepositoryNamespaceIndex: %v", err)
	}

	reopened, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, false, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if !reopened.isIntermediateDir("guardian") {
		t.Fatal("namespace index was not rebuilt by repair")
	}
}

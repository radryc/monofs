package server

import (
	"context"
	"path/filepath"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
)

func TestRegisterRepositoryUpdatesCommitMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	s, err := NewServer("test-node", "localhost:9000", filepath.Join(tmpDir, "test.db"), filepath.Join(tmpDir, "git"), false, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer s.Close()

	req := &pb.RegisterRepositoryRequest{
		StorageId:     "repo-commit-update",
		DisplayPath:   "github.com/acme/repo",
		Source:        "https://github.com/acme/repo.git",
		CommitHash:    "old-commit",
		CommitTime:    100,
		CommitMessage: "old message",
	}
	if _, err := s.RegisterRepository(context.Background(), req); err != nil {
		t.Fatalf("first RegisterRepository: %v", err)
	}
	req.CommitHash = "new-commit"
	req.CommitTime = 200
	req.CommitMessage = "new message"
	if _, err := s.RegisterRepository(context.Background(), req); err != nil {
		t.Fatalf("second RegisterRepository: %v", err)
	}
	info, err := s.GetRepositoryInfo(context.Background(), &pb.GetRepositoryInfoRequest{StorageId: req.StorageId})
	if err != nil {
		t.Fatalf("GetRepositoryInfo: %v", err)
	}
	if info.CommitHash != "new-commit" || info.CommitTime != 200 || info.CommitMessage != "new message" {
		t.Fatalf("metadata = %+v", info)
	}
}

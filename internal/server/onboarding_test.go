package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

func TestOnboardingStatusTracking(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()
	storageID := "test-storage-123"

	// Register repository
	_, err = server.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: "github.com/test/repo",
		Source:      "https://github.com/test/repo.git",
	})
	if err != nil {
		t.Fatalf("failed to register repository: %v", err)
	}

	// Ingest a file (should mark as NOT onboarded yet)
	_, err = server.IngestFile(ctx, &pb.IngestFileRequest{
		Metadata: &pb.FileMetadata{
			Path:        "README.md",
			StorageId:   storageID,
			DisplayPath: "github.com/test/repo",
			Size:        100,
			Mode:        0644,
			Mtime:       time.Now().Unix(),
			BlobHash:    "abc123",
			Ref:         "main",
			Source:      "https://github.com/test/repo.git",
		},
	})
	if err != nil {
		t.Fatalf("failed to ingest file: %v", err)
	}

	// Check status - should be false (not onboarded)
	statusResp, err := server.GetOnboardingStatus(ctx, &pb.OnboardingStatusRequest{
		NodeId: "test-node",
	})
	if err != nil {
		t.Fatalf("GetOnboardingStatus failed: %v", err)
	}

	if onboarded, exists := statusResp.Repositories[storageID]; !exists {
		t.Error("expected repository to be tracked")
	} else if onboarded {
		t.Error("expected repository to NOT be onboarded yet")
	}

	// Mark as onboarded
	_, err = server.MarkRepositoryOnboarded(ctx, &pb.MarkRepositoryOnboardedRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("MarkRepositoryOnboarded failed: %v", err)
	}

	// Check again - should be true
	statusResp, err = server.GetOnboardingStatus(ctx, &pb.OnboardingStatusRequest{
		NodeId: "test-node",
	})
	if err != nil {
		t.Fatalf("GetOnboardingStatus failed: %v", err)
	}

	if onboarded, exists := statusResp.Repositories[storageID]; !exists {
		t.Error("expected repository to be tracked")
	} else if !onboarded {
		t.Error("expected repository to be onboarded")
	}
}

func TestOnboardingStatusMultipleRepositories(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()

	repos := []struct {
		storageID   string
		displayPath string
		repoURL     string
	}{
		{"storage-1", "github.com/test/repo1", "https://github.com/test/repo1.git"},
		{"storage-2", "github.com/test/repo2", "https://github.com/test/repo2.git"},
		{"storage-3", "github.com/test/repo3", "https://github.com/test/repo3.git"},
	}

	// Register and ingest files for all repositories
	for _, repo := range repos {
		_, err = server.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
			StorageId:   repo.storageID,
			DisplayPath: repo.displayPath,
			Source:      repo.repoURL,
		})
		if err != nil {
			t.Fatalf("failed to register repository %s: %v", repo.storageID, err)
		}

		_, err = server.IngestFile(ctx, &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        "README.md",
				StorageId:   repo.storageID,
				DisplayPath: repo.displayPath,
				Size:        100,
				Mode:        0644,
				Mtime:       time.Now().Unix(),
				BlobHash:    "abc123",
				Ref:         "main",
				Source:      repo.repoURL,
			},
		})
		if err != nil {
			t.Fatalf("failed to ingest file for %s: %v", repo.storageID, err)
		}
	}

	// Check status - all should be false
	statusResp, err := server.GetOnboardingStatus(ctx, &pb.OnboardingStatusRequest{
		NodeId: "test-node",
	})
	if err != nil {
		t.Fatalf("GetOnboardingStatus failed: %v", err)
	}

	if len(statusResp.Repositories) != 3 {
		t.Errorf("expected 3 repositories, got %d", len(statusResp.Repositories))
	}

	for _, repo := range repos {
		if onboarded, exists := statusResp.Repositories[repo.storageID]; !exists {
			t.Errorf("repository %s not tracked", repo.storageID)
		} else if onboarded {
			t.Errorf("repository %s should not be onboarded yet", repo.storageID)
		}
	}

	// Mark first two as onboarded
	for i := 0; i < 2; i++ {
		_, err = server.MarkRepositoryOnboarded(ctx, &pb.MarkRepositoryOnboardedRequest{
			StorageId: repos[i].storageID,
		})
		if err != nil {
			t.Fatalf("MarkRepositoryOnboarded failed for %s: %v", repos[i].storageID, err)
		}
	}

	// Check status again
	statusResp, err = server.GetOnboardingStatus(ctx, &pb.OnboardingStatusRequest{
		NodeId: "test-node",
	})
	if err != nil {
		t.Fatalf("GetOnboardingStatus failed: %v", err)
	}

	// First two should be onboarded, third should not
	for i, repo := range repos {
		onboarded, exists := statusResp.Repositories[repo.storageID]
		if !exists {
			t.Errorf("repository %s not tracked", repo.storageID)
			continue
		}

		if i < 2 && !onboarded {
			t.Errorf("repository %s should be onboarded", repo.storageID)
		} else if i == 2 && onboarded {
			t.Errorf("repository %s should not be onboarded yet", repo.storageID)
		}
	}
}

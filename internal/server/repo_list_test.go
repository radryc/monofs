package server

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/metastore"
)

func TestListRepositoriesPrefixDetailsAndPagination(t *testing.T) {
	tmpDir := t.TempDir()
	s, err := NewServer("test-node", "localhost:9000", filepath.Join(tmpDir, "test.db"), filepath.Join(tmpDir, "git"), false, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer s.Close()

	repos := []struct {
		id      string
		display string
	}{
		{"g1", "guardian/part-1"},
		{"g2", "guardian/part-2"},
		{"o1", "github.com/acme/repo"},
		{"gs", "guardian-system"},
	}
	for _, repo := range repos {
		if _, err := s.RegisterRepository(context.Background(), &pb.RegisterRepositoryRequest{
			StorageId:   repo.id,
			DisplayPath: repo.display,
			Source:      "https://example.com/repo.git",
		}); err != nil {
			t.Fatalf("RegisterRepository(%s): %v", repo.display, err)
		}
	}

	first, err := s.ListRepositories(context.Background(), &pb.ListRepositoriesRequest{
		Prefix:         "guardian/",
		PageSize:       1,
		IncludeDetails: true,
	})
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(first.GetRepositories()) != 1 || first.GetNextPageToken() == "" || first.GetTotal() != 2 {
		t.Fatalf("first page = records=%d next=%q total=%d", len(first.GetRepositories()), first.GetNextPageToken(), first.GetTotal())
	}
	if first.GetRepositories()[0].GetDisplayPath() != "guardian/part-1" {
		t.Fatalf("first record = %q", first.GetRepositories()[0].GetDisplayPath())
	}

	second, err := s.ListRepositories(context.Background(), &pb.ListRepositoriesRequest{
		Prefix:         "guardian/",
		PageSize:       1,
		PageToken:      first.GetNextPageToken(),
		IncludeDetails: true,
	})
	if err != nil {
		t.Fatalf("ListRepositories second page: %v", err)
	}
	if len(second.GetRepositories()) != 1 || second.GetNextPageToken() != "" {
		t.Fatalf("second page = records=%d next=%q", len(second.GetRepositories()), second.GetNextPageToken())
	}
	if second.GetRepositories()[0].GetDisplayPath() != "guardian/part-2" {
		t.Fatalf("second record = %q", second.GetRepositories()[0].GetDisplayPath())
	}
}

func TestRepoNamespaceIndexRebuildsOnStartup(t *testing.T) {
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

	// Simulate an old database without the namespace index.
	if err := s.db.Update(func(tx metastore.Tx) error {
		for _, bucket := range []string{bucketRepoPrefixes, bucketRepoChildren} {
			var keys [][]byte
			if err := tx.ScanPrefix(bucket, nil, func(key, _ []byte) error {
				keys = append(keys, append([]byte(nil), key...))
				return nil
			}); err != nil {
				return err
			}
			for _, key := range keys {
				if err := tx.Delete(bucket, key); err != nil && !errors.Is(err, metastore.ErrKeyNotFound) {
					return err
				}
			}
		}
		return tx.Delete(bucketSchema, []byte(repoNamespaceIndexKey))
	}); err != nil {
		t.Fatalf("clear index: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, false, nil)
	if err != nil {
		t.Fatalf("reopen NewServer: %v", err)
	}
	defer reopened.Close()
	if !reopened.isIntermediateDir("guardian") {
		t.Fatal("namespace index was not rebuilt on startup")
	}
}

func TestListGuardianPartitionsUsesPrefixIndex(t *testing.T) {
	tmpDir := t.TempDir()
	s, err := NewServer("test-node", "localhost:9000", filepath.Join(tmpDir, "test.db"), filepath.Join(tmpDir, "git"), false, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer s.Close()

	repos := []struct {
		id      string
		display string
	}{
		{"g1", "guardian/part-1"},
		{"g2", "guardian/part-2"},
		{"o1", "github.com/acme/repo"},
		{"gs", "guardian-system"},
	}
	for _, repo := range repos {
		if _, err := s.RegisterRepository(context.Background(), &pb.RegisterRepositoryRequest{
			StorageId:   repo.id,
			DisplayPath: repo.display,
			Source:      "https://example.com/repo.git",
		}); err != nil {
			t.Fatalf("RegisterRepository(%s): %v", repo.display, err)
		}
	}

	resp, err := s.ListGuardianPartitions(context.Background(), &pb.ListGuardianPartitionsRequest{})
	if err != nil {
		t.Fatalf("ListGuardianPartitions: %v", err)
	}
	if len(resp.GetPartitions()) != 2 || resp.GetTotal() != 2 {
		t.Fatalf("partitions = %d total = %d, want 2", len(resp.GetPartitions()), resp.GetTotal())
	}

	resp, err = s.ListGuardianPartitions(context.Background(), &pb.ListGuardianPartitionsRequest{IncludeSystem: true})
	if err != nil {
		t.Fatalf("ListGuardianPartitions with system: %v", err)
	}
	if len(resp.GetPartitions()) != 3 || resp.GetTotal() != 3 {
		t.Fatalf("partitions = %d total = %d, want 3", len(resp.GetPartitions()), resp.GetTotal())
	}

	if !s.isIntermediateDir("guardian") || !s.isIntermediateDir("github.com/acme") {
		t.Fatal("repository namespace prefixes should be intermediate directories")
	}
	if s.isIntermediateDir("guardian/part-1") {
		t.Fatal("a repository leaf should not be an intermediate directory")
	}
	if s.isIntermediateDir("guardian/missing") {
		t.Fatal("missing partition should not be an intermediate directory")
	}
}

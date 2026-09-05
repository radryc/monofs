package server

import (
	"context"
	"path/filepath"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/sharding"
	"google.golang.org/grpc"
)

func TestReadDirFindsTagsInSubdirectoryViaRebuild(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, false, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer s.Close()

	// Simulate the registry client's pattern:
	// RegisterRepository with displayPath "docker-registry/doctor-query"
	_, err = s.RegisterRepository(context.Background(), &pb.RegisterRepositoryRequest{
		StorageId:   sharding.GenerateStorageID("docker-registry/doctor-query"),
		DisplayPath: "docker-registry/doctor-query",
		Source:      "monofs-registry",
	})
	if err != nil {
		t.Fatalf("RegisterRepository failed: %v", err)
	}

	// IngestFileBatch with tag data (what the registry client Write does)
	storageID := sharding.GenerateStorageID("docker-registry/doctor-query")
	_, err = s.IngestFileBatch(context.Background(), &pb.IngestFileBatchRequest{
		StorageId:   storageID,
		DisplayPath: "docker-registry/doctor-query",
		Files: []*pb.FileMetadata{
			{
				Path:          "_tags/latest",
				StorageId:     storageID,
				DisplayPath:   "docker-registry/doctor-query",
				Mode:          0644,
				Size:          64,
				Mtime:         1700000000,
				BlobHash:      "sha256:abc123",
				Source:        "monofs-registry",
				InlineContent: []byte("sha256:abc123"),
				SourceType:    pb.IngestionType_INGESTION_FILE,
				FetchType:     pb.SourceType_SOURCE_TYPE_BLOB,
			},
			{
				Path:          "_tags/v1.0",
				StorageId:     storageID,
				DisplayPath:   "docker-registry/doctor-query",
				Mode:          0644,
				Size:          64,
				Mtime:         1700000001,
				BlobHash:      "sha256:def456",
				Source:        "monofs-registry",
				InlineContent: []byte("sha256:def456"),
				SourceType:    pb.IngestionType_INGESTION_FILE,
				FetchType:     pb.SourceType_SOURCE_TYPE_BLOB,
			},
		},
	})
	if err != nil {
		t.Fatalf("IngestFileBatch failed: %v", err)
	}

	// ReadDir should find the _tags directory entries WITHOUT BuildDirectoryIndexes
	// (because the rebuild-from-canonical fallback in ReadDir handles it)
	stream := &collectingReadDirStream{}
	err = s.ReadDir(&pb.ReadDirRequest{Path: "docker-registry/doctor-query/_tags"}, stream)
	if err != nil {
		t.Fatalf("ReadDir(_tags) failed: %v", err)
	}

	found := make(map[string]bool)
	for _, e := range stream.entries {
		found[e.Name] = true
	}

	if !found["latest"] {
		t.Errorf("expected tag 'latest', got entries: %v", stream.names())
	}
	if !found["v1.0"] {
		t.Errorf("expected tag 'v1.0', got entries: %v", stream.names())
	}
	if found["_tags"] {
		t.Error("'_tags' should not appear as a child of itself")
	}
	if len(stream.entries) != 2 {
		t.Errorf("expected 2 tag entries, got %d: %v", len(stream.entries), stream.names())
	}
}

func TestReadDirFindsMultipleRepos(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, false, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer s.Close()

	repos := []string{"doctor-query", "guardian-pusher-docker", "monofs-fetcher"}
	for _, repo := range repos {
		displayPath := "docker-registry/" + repo
		storageID := sharding.GenerateStorageID(displayPath)

		_, err = s.RegisterRepository(context.Background(), &pb.RegisterRepositoryRequest{
			StorageId:   storageID,
			DisplayPath: displayPath,
			Source:      "monofs-registry",
		})
		if err != nil {
			t.Fatalf("RegisterRepository(%s) failed: %v", repo, err)
		}

		_, err = s.IngestFileBatch(context.Background(), &pb.IngestFileBatchRequest{
			StorageId:   storageID,
			DisplayPath: displayPath,
			Files: []*pb.FileMetadata{
				{
					Path:          "_tags/latest",
					StorageId:     storageID,
					DisplayPath:   displayPath,
					Mode:          0644,
					Size:          64,
					Mtime:         1700000000,
					BlobHash:      "sha256:hash_" + repo,
					Source:        "monofs-registry",
					InlineContent: []byte("sha256:hash_" + repo),
					SourceType:    pb.IngestionType_INGESTION_FILE,
					FetchType:     pb.SourceType_SOURCE_TYPE_BLOB,
				},
			},
		})
		if err != nil {
			t.Fatalf("IngestFileBatch(%s) failed: %v", repo, err)
		}
	}

	// Each repo's _tags directory should have the 'latest' tag
	for _, repo := range repos {
		path := "docker-registry/" + repo + "/_tags"
		stream := &collectingReadDirStream{}
		err = s.ReadDir(&pb.ReadDirRequest{Path: path}, stream)
		if err != nil {
			t.Fatalf("ReadDir(%s) failed: %v", path, err)
		}
		if len(stream.entries) == 0 {
			t.Errorf("ReadDir(%s) returned no entries", path)
		}
		if !stream.has("latest") {
			t.Errorf("ReadDir(%s) missing 'latest', got: %v", path, stream.names())
		}
	}

	// ReadDir of the namespace root should find repo directories
	rootStream := &collectingReadDirStream{}
	err = s.ReadDir(&pb.ReadDirRequest{Path: "docker-registry"}, rootStream)
	if err != nil {
		t.Fatalf("ReadDir(docker-registry) failed: %v", err)
	}
	for _, repo := range repos {
		if !rootStream.has(repo) {
			t.Errorf("ReadDir(docker-registry) missing repo %q, got: %v", repo, rootStream.names())
		}
	}
}

func TestForwardableTarget_Disabled(t *testing.T) {
	s := &Server{enableForwarding: false}
	if target := s.forwardableTarget("docker-registry/doctor-query/_tags"); target != nil {
		t.Error("forwardableTarget should return nil when forwarding is disabled")
	}
}

func TestForwardableTarget_NoHRW(t *testing.T) {
	s := &Server{enableForwarding: true}
	if target := s.forwardableTarget("docker-registry/doctor-query/_tags"); target != nil {
		t.Error("forwardableTarget should return nil when HRW is nil")
	}
}

func TestForwardableTarget_SingleSegment(t *testing.T) {
	s := &Server{enableForwarding: true}
	if target := s.forwardableTarget("docker-registry"); target != nil {
		t.Error("forwardableTarget should return nil for single-segment path")
	}
	if target := s.forwardableTarget(""); target != nil {
		t.Error("forwardableTarget should return nil for empty path")
	}
}

// collectingReadDirStream implements pb.MonoFS_ReadDirServer for test collection.
type collectingReadDirStream struct {
	grpc.ServerStream
	entries []*pb.DirEntry
}

func (s *collectingReadDirStream) Send(entry *pb.DirEntry) error {
	s.entries = append(s.entries, entry)
	return nil
}

func (s *collectingReadDirStream) Context() context.Context {
	return context.Background()
}

func (s *collectingReadDirStream) has(name string) bool {
	for _, e := range s.entries {
		if e.Name == name {
			return true
		}
	}
	return false
}

func (s *collectingReadDirStream) names() []string {
	var ns []string
	for _, e := range s.entries {
		ns = append(ns, e.Name)
	}
	return ns
}

package server

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nutsdb/nutsdb"
	pb "github.com/radryc/monofs/api/proto"
)

func newUsageTestServer(t *testing.T) *Server {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db")
	gitCache := filepath.Join(tmpDir, "git-cache")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return s
}

func usageTestMeta(storageID, displayPath, filePath string, size uint64) *pb.FileMetadata {
	return &pb.FileMetadata{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Path:        filePath,
		Size:        size,
		Mode:        0o644 | uint32(syscall.S_IFREG),
		Mtime:       time.Now().Unix(),
		Source:      "https://example.invalid/repo.git",
		Ref:         "main",
	}
}

func TestGetNodeInfoTracksOwnedBytesAcrossOverwriteAndDelete(t *testing.T) {
	server := newUsageTestServer(t)
	defer server.Close()

	ctx := context.Background()
	storageID := "repo-1"
	displayPath := "github.com/acme/repo"

	if _, err := server.IngestFile(ctx, &pb.IngestFileRequest{
		Metadata: usageTestMeta(storageID, displayPath, "README.md", 10),
	}); err != nil {
		t.Fatalf("IngestFile(initial) error = %v", err)
	}

	info, err := server.GetNodeInfo(ctx, &pb.NodeInfoRequest{})
	if err != nil {
		t.Fatalf("GetNodeInfo() error = %v", err)
	}
	if got, want := info.DiskUsedBytes, int64(10); got != want {
		t.Fatalf("DiskUsedBytes after initial ingest = %d, want %d", got, want)
	}
	if got, want := info.TotalFiles, int64(1); got != want {
		t.Fatalf("TotalFiles after initial ingest = %d, want %d", got, want)
	}

	if _, err := server.IngestFile(ctx, &pb.IngestFileRequest{
		Metadata: usageTestMeta(storageID, displayPath, "README.md", 25),
	}); err != nil {
		t.Fatalf("IngestFile(overwrite) error = %v", err)
	}

	info, err = server.GetNodeInfo(ctx, &pb.NodeInfoRequest{})
	if err != nil {
		t.Fatalf("GetNodeInfo() after overwrite error = %v", err)
	}
	if got, want := info.DiskUsedBytes, int64(25); got != want {
		t.Fatalf("DiskUsedBytes after overwrite = %d, want %d", got, want)
	}
	if got, want := info.TotalFiles, int64(1); got != want {
		t.Fatalf("TotalFiles after overwrite = %d, want %d", got, want)
	}

	if _, err := server.DeleteFile(ctx, &pb.DeleteFileRequest{
		StorageId: storageID,
		FilePath:  "README.md",
	}); err != nil {
		t.Fatalf("DeleteFile() error = %v", err)
	}

	info, err = server.GetNodeInfo(ctx, &pb.NodeInfoRequest{})
	if err != nil {
		t.Fatalf("GetNodeInfo() after delete error = %v", err)
	}
	if got := info.DiskUsedBytes; got != 0 {
		t.Fatalf("DiskUsedBytes after delete = %d, want 0", got)
	}
	if got := info.TotalFiles; got != 0 {
		t.Fatalf("TotalFiles after delete = %d, want 0", got)
	}
}

func TestLoadOwnedUsageRestoresBytesFromMetadata(t *testing.T) {
	server := newUsageTestServer(t)
	defer server.Close()
	ctx := context.Background()

	resp, err := server.IngestFileBatch(ctx, &pb.IngestFileBatchRequest{
		StorageId:   "repo-2",
		DisplayPath: "github.com/acme/repo2",
		Source:      "https://example.invalid/repo2.git",
		Ref:         "main",
		Files: []*pb.FileMetadata{
			usageTestMeta("repo-2", "github.com/acme/repo2", "a.txt", 7),
			usageTestMeta("repo-2", "github.com/acme/repo2", "b.txt", 5),
		},
	})
	if err != nil {
		t.Fatalf("IngestFileBatch() error = %v", err)
	}
	if got, want := resp.FilesIngested, int64(2); got != want {
		t.Fatalf("FilesIngested = %d, want %d", got, want)
	}

	var totalFiles int64
	var totalBytes int64
	if err := server.db.View(func(tx *nutsdb.Tx) error {
		var err error
		totalFiles, totalBytes, err = loadOwnedUsage(tx)
		return err
	}); err != nil {
		t.Fatalf("loadOwnedUsage() error = %v", err)
	}
	if got, want := totalBytes, int64(12); got != want {
		t.Fatalf("owned bytes from scan = %d, want %d", got, want)
	}
	if got, want := totalFiles, int64(2); got != want {
		t.Fatalf("owned files from scan = %d, want %d", got, want)
	}
}

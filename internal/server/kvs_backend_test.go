package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/rydzu/ainfra/kvs/pkg/localstore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestKVSBackedRepositoryBypassesFetcher(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "monofs-kvs-node-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	server, err := NewServer("test-node", ":9000", filepath.Join(tmpDir, "db"), filepath.Join(tmpDir, "git-cache"), nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	store, err := localstore.Open(localstore.Config{DataDir: filepath.Join(tmpDir, "kvs"), MaxHotVersions: 2})
	if err != nil {
		t.Fatalf("failed to open kvs store: %v", err)
	}
	server.SetKVSStore(store)

	ctx := context.Background()
	storageID := "guardian-system-kvs"
	_, err = server.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: "guardian-system",
		Source:      "guardian-system",
		FetchConfig: map[string]string{"storage_backend": "kvs"},
	})
	if err != nil {
		t.Fatalf("register kvs repository: %v", err)
	}

	batchResp, err := server.IngestFileBatch(ctx, &pb.IngestFileBatchRequest{
		StorageId:   storageID,
		DisplayPath: "guardian-system",
		Files: []*pb.FileMetadata{
			{
				Path:          ".queues/demo/task.json",
				DisplayPath:   "guardian-system",
				StorageId:     storageID,
				Size:          uint64(len("task-payload")),
				InlineContent: []byte("task-payload"),
				BackendMetadata: map[string]string{
					"storage_backend": "kvs",
				},
			},
			{
				Path:          ".queues/demo/nested/result.json",
				DisplayPath:   "guardian-system",
				StorageId:     storageID,
				Size:          uint64(len("nested-result")),
				InlineContent: []byte("nested-result"),
				BackendMetadata: map[string]string{
					"storage_backend": "kvs",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("ingest kvs file batch: %v", err)
	}
	if !batchResp.Success || batchResp.FilesIngested != 2 {
		t.Fatalf("unexpected ingest response: %+v", batchResp)
	}

	lookup, err := server.Lookup(ctx, &pb.LookupRequest{ParentPath: "guardian-system/.queues/demo", Name: "task.json"})
	if err != nil {
		t.Fatalf("lookup kvs file: %v", err)
	}
	if !lookup.Found {
		t.Fatal("expected kvs-backed file to be found")
	}

	attr, err := server.GetAttr(ctx, &pb.GetAttrRequest{Path: "guardian-system/.queues/demo/task.json"})
	if err != nil {
		t.Fatalf("getattr kvs file: %v", err)
	}
	if !attr.Found || attr.Size != uint64(len("task-payload")) {
		t.Fatalf("unexpected getattr response: %+v", attr)
	}

	dirStream := &testReadDirStream{}
	if err := server.ReadDir(&pb.ReadDirRequest{Path: "guardian-system/.queues/demo"}, dirStream); err != nil {
		t.Fatalf("readdir kvs dir: %v", err)
	}
	foundTask := false
	foundNested := false
	for _, entry := range dirStream.entries {
		switch entry.Name {
		case "task.json":
			foundTask = true
		case "nested":
			foundNested = true
		}
	}
	if !foundTask || !foundNested {
		t.Fatalf("unexpected kvs directory entries: %+v", dirStream.entries)
	}

	readStream := &mockReadStream{}
	if err := server.Read(&pb.ReadRequest{Path: "guardian-system/.queues/demo/task.json"}, readStream); err != nil {
		t.Fatalf("read kvs file: %v", err)
	}
	var content []byte
	for _, chunk := range readStream.chunks {
		content = append(content, chunk.Data...)
	}
	if string(content) != "task-payload" {
		t.Fatalf("unexpected kvs file content %q", string(content))
	}

	deleteResp, err := server.DeleteDirectoryRecursive(ctx, &pb.DeleteDirectoryRecursiveRequest{StorageId: storageID, DirPath: ".queues/demo"})
	if err != nil {
		t.Fatalf("delete kvs directory: %v", err)
	}
	if !deleteResp.Success || deleteResp.FilesDeleted != 2 {
		t.Fatalf("unexpected delete response: %+v", deleteResp)
	}

	readStream = &mockReadStream{}
	err = server.Read(&pb.ReadRequest{Path: "guardian-system/.queues/demo/task.json"}, readStream)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected not found after kvs delete, got %v", err)
	}
}

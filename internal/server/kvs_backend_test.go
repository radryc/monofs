package server

import (
	"context"
	"errors"
	"io/fs"
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

func TestKVSBackedRepositoryDeleteRepositoryCleansUpStore(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "monofs-kvs-delete-*")
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
	storageID := "guardian-system-kvs-delete"
	_, err = server.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: "guardian-system",
		Source:      "guardian-system",
		FetchConfig: map[string]string{"storage_backend": "kvs"},
	})
	if err != nil {
		t.Fatalf("register kvs repository: %v", err)
	}

	_, err = server.IngestFileBatch(ctx, &pb.IngestFileBatchRequest{
		StorageId:   storageID,
		DisplayPath: "guardian-system",
		Files: []*pb.FileMetadata{{
			Path:          ".queues/demo/task.json",
			DisplayPath:   "guardian-system",
			StorageId:     storageID,
			Size:          uint64(len("task-payload")),
			InlineContent: []byte("task-payload"),
			BackendMetadata: map[string]string{
				"storage_backend": "kvs",
			},
		}},
	})
	if err != nil {
		t.Fatalf("ingest kvs file batch: %v", err)
	}

	logicalPath := kvsLogicalPath("guardian-system", ".queues/demo/task.json")
	if content, err := store.ReadFile(ctx, logicalPath); err != nil || string(content) != "task-payload" {
		t.Fatalf("expected kvs file before delete, got content=%q err=%v", string(content), err)
	}

	deleteResp, err := server.DeleteRepository(ctx, &pb.DeleteRepositoryOnNodeRequest{StorageId: storageID})
	if err != nil {
		t.Fatalf("DeleteRepository() error = %v", err)
	}
	if !deleteResp.Success || deleteResp.FilesDeleted != 1 {
		t.Fatalf("unexpected delete repository response: %+v", deleteResp)
	}

	_, err = store.ReadFile(ctx, logicalPath)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected kvs content to be removed from store, got %v", err)
	}
}

func TestKVSBackedRepositoriesStayIsolatedAcrossPartitions(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "monofs-kvs-multi-*")
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
	repos := []struct {
		storageID   string
		displayPath string
		filePath    string
		fullPath    string
		content     string
	}{
		{
			storageID:   "guardian-genomics-kvs",
			displayPath: "guardian/genomics",
			filePath:    "intents/api.yaml",
			fullPath:    "guardian/genomics/intents/api.yaml",
			content:     "kind: Intent\nmetadata:\n  name: api\n",
		},
		{
			storageID:   "guardian-payments-kvs",
			displayPath: "guardian/payments",
			filePath:    "intents/worker.yaml",
			fullPath:    "guardian/payments/intents/worker.yaml",
			content:     "kind: Intent\nmetadata:\n  name: worker\n",
		},
		{
			storageID:   "guardian-system-kvs-multi",
			displayPath: "guardian-system",
			filePath:    ".queues/local/tasks/task-42.json",
			fullPath:    "guardian-system/.queues/local/tasks/task-42.json",
			content:     "{\"id\":\"task-42\"}",
		},
	}

	for _, repo := range repos {
		_, err := server.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
			StorageId:   repo.storageID,
			DisplayPath: repo.displayPath,
			Source:      repo.displayPath,
			FetchConfig: map[string]string{"storage_backend": "kvs"},
		})
		if err != nil {
			t.Fatalf("register kvs repository %q: %v", repo.displayPath, err)
		}

		batchResp, err := server.IngestFileBatch(ctx, &pb.IngestFileBatchRequest{
			StorageId:   repo.storageID,
			DisplayPath: repo.displayPath,
			Files: []*pb.FileMetadata{{
				Path:          repo.filePath,
				DisplayPath:   repo.displayPath,
				StorageId:     repo.storageID,
				Size:          uint64(len(repo.content)),
				InlineContent: []byte(repo.content),
				BackendMetadata: map[string]string{
					"storage_backend": "kvs",
				},
			}},
		})
		if err != nil {
			t.Fatalf("ingest kvs file batch for %q: %v", repo.displayPath, err)
		}
		if !batchResp.Success || batchResp.FilesIngested != 1 {
			t.Fatalf("unexpected ingest response for %q: %+v", repo.displayPath, batchResp)
		}
	}

	guardianStream := &testReadDirStream{}
	if err := server.ReadDir(&pb.ReadDirRequest{Path: "guardian"}, guardianStream); err != nil {
		t.Fatalf("readdir guardian root: %v", err)
	}
	foundGenomics := false
	foundPayments := false
	for _, entry := range guardianStream.entries {
		switch entry.Name {
		case "genomics":
			foundGenomics = true
		case "payments":
			foundPayments = true
		}
	}
	if !foundGenomics || !foundPayments {
		t.Fatalf("unexpected guardian root entries: %+v", guardianStream.entries)
	}

	for _, repo := range repos {
		attr, err := server.GetAttr(ctx, &pb.GetAttrRequest{Path: repo.fullPath})
		if err != nil {
			t.Fatalf("getattr %q: %v", repo.fullPath, err)
		}
		if !attr.Found || attr.Size != uint64(len(repo.content)) {
			t.Fatalf("unexpected getattr for %q: %+v", repo.fullPath, attr)
		}

		readStream := &mockReadStream{}
		if err := server.Read(&pb.ReadRequest{Path: repo.fullPath}, readStream); err != nil {
			t.Fatalf("read %q: %v", repo.fullPath, err)
		}
		var content []byte
		for _, chunk := range readStream.chunks {
			content = append(content, chunk.Data...)
		}
		if string(content) != repo.content {
			t.Fatalf("unexpected content for %q: %q", repo.fullPath, string(content))
		}

		logicalPath := kvsLogicalPath(repo.displayPath, repo.filePath)
		storedContent, err := store.ReadFile(ctx, logicalPath)
		if err != nil {
			t.Fatalf("store.ReadFile(%q): %v", logicalPath, err)
		}
		if string(storedContent) != repo.content {
			t.Fatalf("unexpected store content for %q: %q", logicalPath, string(storedContent))
		}
	}

	deleteResp, err := server.DeleteRepository(ctx, &pb.DeleteRepositoryOnNodeRequest{StorageId: repos[0].storageID})
	if err != nil {
		t.Fatalf("DeleteRepository(genomics) error = %v", err)
	}
	if !deleteResp.Success || deleteResp.FilesDeleted != 1 {
		t.Fatalf("unexpected delete response: %+v", deleteResp)
	}

	deletedLogicalPath := kvsLogicalPath(repos[0].displayPath, repos[0].filePath)
	_, err = store.ReadFile(ctx, deletedLogicalPath)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected deleted repo content to be removed from store, got %v", err)
	}

	deletedReadStream := &mockReadStream{}
	err = server.Read(&pb.ReadRequest{Path: repos[0].fullPath}, deletedReadStream)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected deleted repo read to return not found, got %v", err)
	}

	guardianStream = &testReadDirStream{}
	if err := server.ReadDir(&pb.ReadDirRequest{Path: "guardian"}, guardianStream); err != nil {
		t.Fatalf("readdir guardian root after delete: %v", err)
	}
	foundGenomics = false
	foundPayments = false
	for _, entry := range guardianStream.entries {
		switch entry.Name {
		case "genomics":
			foundGenomics = true
		case "payments":
			foundPayments = true
		}
	}
	if foundGenomics || !foundPayments {
		t.Fatalf("unexpected guardian root entries after delete: %+v", guardianStream.entries)
	}

	for _, repo := range repos[1:] {
		readStream := &mockReadStream{}
		if err := server.Read(&pb.ReadRequest{Path: repo.fullPath}, readStream); err != nil {
			t.Fatalf("expected %q to survive unrelated repo delete, got %v", repo.fullPath, err)
		}
		logicalPath := kvsLogicalPath(repo.displayPath, repo.filePath)
		storedContent, err := store.ReadFile(ctx, logicalPath)
		if err != nil {
			t.Fatalf("expected %q to remain in store, got %v", logicalPath, err)
		}
		if string(storedContent) != repo.content {
			t.Fatalf("unexpected surviving store content for %q: %q", logicalPath, string(storedContent))
		}
	}
}

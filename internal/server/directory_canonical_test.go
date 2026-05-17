package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/nutsdb/nutsdb"
	pb "github.com/radryc/monofs/api/proto"
)

func TestCanonicalDirectorySurvivesIndexLoss(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-storage-canonical"
	displayPath := "repo"

	_, err = s.IngestFileBatch(context.Background(), &pb.IngestFileBatchRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      "test",
		Files: []*pb.FileMetadata{
			{
				Path:            "docs/empty",
				Mode:            0555,
				Mtime:           1000,
				BackendMetadata: map[string]string{"file_type": "1"},
			},
			{
				Path:     "docs/empty/file.txt",
				Mode:     0644,
				Size:     12,
				Mtime:    1001,
				BlobHash: "abc",
			},
		},
	})
	if err != nil {
		t.Fatalf("IngestFileBatch failed: %v", err)
	}

	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	_, err = s.DeleteFile(context.Background(), &pb.DeleteFileRequest{
		StorageId: storageID,
		FilePath:  "docs/empty/file.txt",
	})
	if err != nil {
		t.Fatalf("DeleteFile failed: %v", err)
	}

	err = s.db.Update(func(tx *nutsdb.Tx) error {
		for _, dirPath := range []string{"", "docs", "docs/empty"} {
			if err := tx.Delete(bucketDirIndex, makeDirIndexKey(storageID, dirPath)); err != nil && err != nutsdb.ErrKeyNotFound {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to delete dir indexes: %v", err)
	}

	attrResp, err := s.GetAttr(context.Background(), &pb.GetAttrRequest{
		Path: displayPath + "/docs/empty",
	})
	if err != nil {
		t.Fatalf("GetAttr failed: %v", err)
	}
	if !attrResp.Found {
		t.Fatal("expected explicit empty directory to survive file delete")
	}

	rebuiltRoot, foundRoot, err := s.rebuildDirectoryIndexFromCanonical(storageID, "")
	if err != nil {
		t.Fatalf("rebuildDirectoryIndexFromCanonical(root) failed: %v", err)
	}
	if !foundRoot {
		t.Fatal("expected canonical rebuild to find repo root")
	}
	if len(rebuiltRoot) != 1 || rebuiltRoot[0].Name != "docs" {
		t.Fatalf("expected canonical rebuild for root to contain docs, got %+v", rebuiltRoot)
	}

	rootStream := &mockReadDirStream{}
	if err := s.ReadDir(&pb.ReadDirRequest{Path: displayPath}, rootStream); err != nil {
		t.Fatalf("ReadDir(repo) failed: %v", err)
	}
	if len(rootStream.entries) != 1 || rootStream.entries[0].Name != "docs" {
		t.Fatalf("expected repo root to rebuild listing with docs, got %+v", rootStream.entries)
	}

	docsStream := &mockReadDirStream{}
	if err := s.ReadDir(&pb.ReadDirRequest{Path: displayPath + "/docs"}, docsStream); err != nil {
		t.Fatalf("ReadDir(repo/docs) failed: %v", err)
	}
	if len(docsStream.entries) != 1 || docsStream.entries[0].Name != "empty" {
		t.Fatalf("expected docs listing to rebuild with empty dir, got %+v", docsStream.entries)
	}

	emptyStream := &mockReadDirStream{}
	if err := s.ReadDir(&pb.ReadDirRequest{Path: displayPath + "/docs/empty"}, emptyStream); err != nil {
		t.Fatalf("ReadDir(repo/docs/empty) failed: %v", err)
	}
	if len(emptyStream.entries) != 0 {
		t.Fatalf("expected explicit empty directory listing to be empty, got %+v", emptyStream.entries)
	}
}

func TestDirHintRebuildWithoutDirIndex(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-storage-dirhint-rebuild"
	displayPath := "dependency"

	_, err = s.RegisterRepository(context.Background(), &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      "test",
	})
	if err != nil {
		t.Fatalf("RegisterRepository failed: %v", err)
	}

	_, err = s.IngestFileBatch(context.Background(), &pb.IngestFileBatchRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      "test",
		Files: []*pb.FileMetadata{
			{Path: "pkg/enry.go", Mode: 0444, Size: 100, Mtime: 1000, BlobHash: "aaa"},
		},
	})
	if err != nil {
		t.Fatalf("IngestFileBatch real file failed: %v", err)
	}

	_, err = s.IngestFileBatch(context.Background(), &pb.IngestFileBatchRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      "dir-hint",
		Files: []*pb.FileMetadata{
			{Path: "pkg/classifier.go", Mode: 0444, Size: 2777, Mtime: 1000, BackendMetadata: map[string]string{"dir_hint": "true"}},
			{Path: "pkg/common.go", Mode: 0444, Size: 500, Mtime: 1000, BackendMetadata: map[string]string{"dir_hint": "true"}},
		},
	})
	if err != nil {
		t.Fatalf("IngestFileBatch dir hints failed: %v", err)
	}

	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	err = s.db.Update(func(tx *nutsdb.Tx) error {
		for _, dirPath := range []string{"", "pkg"} {
			if err := tx.Delete(bucketDirIndex, makeDirIndexKey(storageID, dirPath)); err != nil && err != nutsdb.ErrKeyNotFound {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to delete dir indexes: %v", err)
	}

	stream := &mockReadDirStream{}
	if err := s.ReadDir(&pb.ReadDirRequest{Path: displayPath + "/pkg"}, stream); err != nil {
		t.Fatalf("ReadDir(pkg) failed: %v", err)
	}
	if len(stream.entries) != 3 {
		t.Fatalf("expected 3 entries rebuilt from summaries, got %+v", stream.entries)
	}

	attrResp, err := s.GetAttr(context.Background(), &pb.GetAttrRequest{
		Path: displayPath + "/pkg/common.go",
	})
	if err != nil {
		t.Fatalf("GetAttr(common.go) failed: %v", err)
	}
	if !attrResp.Found || attrResp.Size != 500 {
		t.Fatalf("expected summary-backed file attrs, got %+v", attrResp)
	}

	lookupResp, err := s.Lookup(context.Background(), &pb.LookupRequest{
		ParentPath: displayPath + "/pkg",
		Name:       "classifier.go",
	})
	if err != nil {
		t.Fatalf("Lookup(classifier.go) failed: %v", err)
	}
	if !lookupResp.Found || lookupResp.Size != 2777 {
		t.Fatalf("expected summary-backed lookup, got %+v", lookupResp)
	}
}

func TestBuildDirectoryIndexesBackfillsCanonicalState(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "legacy-storage"
	displayPath := "legacy-repo"
	filePath := "docs/readme.txt"
	metaKey := makeStorageKey(storageID, filePath)
	meta := storedMetadata{
		Path:        displayPath + "/" + filePath,
		StorageID:   storageID,
		DisplayPath: displayPath,
		FilePath:    filePath,
		Size:        11,
		Mode:        0644,
		Mtime:       2000,
		BlobHash:    "legacy",
		RepoURL:     "test",
		Branch:      "main",
	}
	metaValue, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal legacy metadata: %v", err)
	}

	err = s.db.Update(func(tx *nutsdb.Tx) error {
		repoValue, err := json.Marshal(&repoInfo{
			StorageID:   storageID,
			DisplayPath: displayPath,
			RepoURL:     "test",
			Branch:      "main",
		})
		if err != nil {
			return err
		}
		if err := tx.Put(bucketRepos, []byte(storageID), repoValue, 0); err != nil {
			return err
		}
		if err := tx.Put(bucketRepoLookup, []byte(displayPath), []byte(storageID), 0); err != nil {
			return err
		}
		if err := tx.Put(bucketOnboardingStatus, []byte(storageID), []byte("true"), 0); err != nil {
			return err
		}
		if err := tx.Put(bucketMetadata, metaKey, metaValue, 0); err != nil {
			return err
		}
		if err := tx.Put(bucketPathIndex, []byte(storageID+":"+filePath), metaKey, 0); err != nil {
			return err
		}
		return tx.Put(bucketOwnedFiles, []byte(storageID+":"+filePath), []byte("1"), 0)
	})
	if err != nil {
		t.Fatalf("seed legacy repo failed: %v", err)
	}

	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	err = s.db.Update(func(tx *nutsdb.Tx) error {
		for _, dirPath := range []string{"", "docs"} {
			if err := tx.Delete(bucketDirIndex, makeDirIndexKey(storageID, dirPath)); err != nil && err != nutsdb.ErrKeyNotFound {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("delete rebuilt dir indexes failed: %v", err)
	}

	attrResp, err := s.GetAttr(context.Background(), &pb.GetAttrRequest{
		Path: displayPath + "/docs",
	})
	if err != nil {
		t.Fatalf("GetAttr(docs) failed: %v", err)
	}
	if !attrResp.Found {
		t.Fatalf("expected docs directory after backfill, got %+v", attrResp)
	}

	stream := &mockReadDirStream{}
	if err := s.ReadDir(&pb.ReadDirRequest{Path: displayPath + "/docs"}, stream); err != nil {
		t.Fatalf("ReadDir(docs) failed: %v", err)
	}
	if len(stream.entries) != 1 || stream.entries[0].Name != "readme.txt" {
		t.Fatalf("expected backfilled docs listing, got %+v", stream.entries)
	}
}

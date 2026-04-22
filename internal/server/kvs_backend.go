package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"time"

	"github.com/nutsdb/nutsdb"
	pb "github.com/radryc/monofs/api/proto"
	"github.com/rydzu/ainfra/kvs/pkg/kvsapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const storageBackendKVS = "kvs"

type KVSStore interface {
	kvsapi.Store
	Close() error
}

type kvsResolvedPath struct {
	logicalPath string
	isDir       bool
	size        int64
	modTime     time.Time
	entries     []kvsapi.DirEntry
}

func (s *Server) SetKVSStore(store KVSStore) {
	s.kvsStore = store
}

func (s *Server) repositoryStorageBackend(storageID, fallback string) string {
	if backend := normalizeStorageBackend(fallback); backend != "" {
		return backend
	}
	if repo, ok := s.repoInfoByStorageID(storageID); ok {
		return normalizeStorageBackend(repo.StorageBackend)
	}
	return ""
}

func registerRepositoryStorageBackend(req *pb.RegisterRepositoryRequest) string {
	if req == nil {
		return ""
	}
	if backend := normalizeStorageBackend(req.GetFetchConfig()["storage_backend"]); backend != "" {
		return backend
	}
	if backend := normalizeStorageBackend(req.GetIngestionConfig()["storage_backend"]); backend != "" {
		return backend
	}
	return ""
}

func normalizeStorageBackend(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func (s *Server) repoInfoByStorageID(storageID string) (repoInfo, bool) {
	if storageID == "" {
		return repoInfo{}, false
	}
	var info repoInfo
	err := s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketRepos, []byte(storageID))
		if err != nil {
			return err
		}
		return json.Unmarshal(value, &info)
	})
	if err != nil {
		return repoInfo{}, false
	}
	return info, true
}

func (s *Server) ensureRepositoryRegistration(storageID, displayPath, repoURL, branch, storageBackend string) error {
	if storageID == "" || displayPath == "" {
		return fmt.Errorf("storage id and display path are required")
	}
	return s.db.Update(func(tx *nutsdb.Tx) error {
		repoKey := []byte(storageID)
		existingRepoData, existsErr := tx.Get(bucketRepos, repoKey)
		isNewRepo := existsErr == nutsdb.ErrKeyNotFound

		info := &repoInfo{
			StorageID:      storageID,
			DisplayPath:    displayPath,
			Branch:         branch,
			RepoURL:        repoURL,
			StorageBackend: storageBackend,
		}

		if !isNewRepo && existsErr == nil {
			var existing repoInfo
			if json.Unmarshal(existingRepoData, &existing) == nil {
				info.CommitHash = existing.CommitHash
				info.CommitTime = existing.CommitTime
				info.CommitMessage = existing.CommitMessage
				info.FetchType = existing.FetchType
				info.GuardianURL = existing.GuardianURL
				if info.Branch == "" {
					info.Branch = existing.Branch
				}
				if info.RepoURL == "" {
					info.RepoURL = existing.RepoURL
				}
				if info.StorageBackend == "" {
					info.StorageBackend = existing.StorageBackend
				}
			}
		}

		repoValue, err := json.Marshal(info)
		if err != nil {
			return err
		}
		if err := tx.Put(bucketRepos, repoKey, repoValue, 0); err != nil {
			return err
		}

		if isNewRepo {
			lookupKey := []byte(displayPath)
			if err := tx.Put(bucketRepoLookup, lookupKey, []byte(storageID), 0); err != nil {
				return err
			}
			onboardKey := []byte(storageID)
			if err := tx.Put(bucketOnboardingStatus, onboardKey, []byte("false"), 0); err != nil {
				return err
			}
		}
		return nil
	})
}

func (info repoInfo) usesKVSBackend() bool {
	return normalizeStorageBackend(info.StorageBackend) == storageBackendKVS
}

func clampNonNegativeInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func kvsMode(isDir bool) uint32 {
	if isDir {
		return 0o755 | uint32(syscall.S_IFDIR)
	}
	return 0o644 | uint32(syscall.S_IFREG)
}

func kvsLookupResponse(path string, resolved *kvsResolvedPath) *pb.LookupResponse {
	if resolved == nil {
		return &pb.LookupResponse{Found: false}
	}
	return &pb.LookupResponse{
		Ino:   hashPath(path),
		Mode:  kvsMode(resolved.isDir),
		Size:  uint64(clampNonNegativeInt64(resolved.size)),
		Mtime: resolved.modTime.Unix(),
		Found: true,
	}
}

func kvsGetAttrResponse(path string, resolved *kvsResolvedPath) *pb.GetAttrResponse {
	if resolved == nil {
		return &pb.GetAttrResponse{Found: false}
	}
	nlink := uint32(1)
	if resolved.isDir {
		nlink = 2
	}
	mtime := resolved.modTime.Unix()
	return &pb.GetAttrResponse{
		Ino:   hashPath(path),
		Mode:  kvsMode(resolved.isDir),
		Size:  uint64(clampNonNegativeInt64(resolved.size)),
		Mtime: mtime,
		Atime: mtime,
		Ctime: mtime,
		Nlink: nlink,
		Uid:   uint32(1000),
		Gid:   uint32(1000),
		Found: true,
	}
}

func kvsLogicalPath(displayPath, filePath string) string {
	trimmedDisplayPath := strings.Trim(displayPath, "/")
	trimmedFilePath := strings.Trim(filePath, "/")
	if trimmedFilePath == "" {
		return "/" + trimmedDisplayPath
	}
	return "/" + trimmedDisplayPath + "/" + trimmedFilePath
}

func kvsChildLogicalPath(parentLogicalPath, childName string) string {
	if parentLogicalPath == "/" {
		return "/" + strings.TrimPrefix(childName, "/")
	}
	return parentLogicalPath + "/" + strings.TrimPrefix(childName, "/")
}

func (s *Server) resolveKVSPath(ctx context.Context, storageID, filePath string) (*kvsResolvedPath, bool, error) {
	repo, ok := s.repoInfoByStorageID(storageID)
	if !ok || !repo.usesKVSBackend() {
		return nil, false, nil
	}
	if s.kvsStore == nil {
		return nil, true, status.Error(codes.FailedPrecondition, "kvs-backed repository registered but no kvs store is configured")
	}

	logicalPath := kvsLogicalPath(repo.DisplayPath, filePath)
	if info, err := s.kvsStore.Stat(ctx, logicalPath); err == nil {
		return &kvsResolvedPath{logicalPath: logicalPath, isDir: false, size: info.Size, modTime: info.ModTime}, true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, true, status.Errorf(codes.Internal, "kvs stat %s: %v", logicalPath, err)
	}

	entries, err := s.kvsStore.ListDir(ctx, logicalPath)
	if err != nil {
		return nil, true, status.Errorf(codes.Internal, "kvs listdir %s: %v", logicalPath, err)
	}
	if len(entries) == 0 {
		return nil, true, nil
	}
	return &kvsResolvedPath{logicalPath: logicalPath, isDir: true, entries: entries, modTime: time.Now().UTC()}, true, nil
}

func (s *Server) ingestKVSFiles(ctx context.Context, displayPath string, files []*pb.FileMetadata) (int64, int64, error) {
	if s.kvsStore == nil {
		return 0, int64(len(files)), status.Error(codes.FailedPrecondition, "kvs-backed ingest requested but kvs store is not configured")
	}

	writes := make([]kvsapi.PathWrite, 0, len(files))
	for _, meta := range files {
		if meta == nil {
			continue
		}
		if meta.GetBackendMetadata()["dir_hint"] == "true" || meta.GetBackendMetadata()["file_type"] == "1" {
			continue
		}
		content := append([]byte(nil), meta.GetInlineContent()...)
		if len(content) == 0 && meta.GetSize() > 0 {
			return 0, int64(len(files)), status.Errorf(codes.InvalidArgument, "kvs-backed ingest requires inline content for %s", meta.GetPath())
		}
		writes = append(writes, kvsapi.PathWrite{
			LogicalPath: kvsLogicalPath(displayPath, meta.GetPath()),
			Content:     content,
		})
	}
	if len(writes) == 0 {
		return 0, 0, nil
	}
	if _, err := s.kvsStore.UpsertFiles(ctx, kvsapi.MutationBatch{
		Writes: writes,
		Context: kvsapi.MutationContext{
			PrincipalID: "monofs-node",
			Reason:      "monofs kvs ingest",
		},
	}); err != nil {
		return 0, int64(len(writes)), status.Errorf(codes.Internal, "kvs ingest failed: %v", err)
	}
	return int64(len(writes)), 0, nil
}

func (s *Server) deleteKVSFile(ctx context.Context, storageID, filePath string) error {
	resolved, handled, err := s.resolveKVSPath(ctx, storageID, filePath)
	if err != nil {
		return err
	}
	if !handled {
		return nil
	}
	if resolved == nil || resolved.isDir {
		return nil
	}
	_, err = s.kvsStore.DeletePaths(ctx, kvsapi.DeleteBatch{
		Deletes: []kvsapi.PathDelete{{LogicalPath: resolved.logicalPath}},
		Context: kvsapi.MutationContext{PrincipalID: "monofs-node", Reason: "monofs kvs file delete"},
	})
	if err != nil {
		return status.Errorf(codes.Internal, "kvs delete failed: %v", err)
	}
	return nil
}

func (s *Server) deleteKVSDirectory(ctx context.Context, storageID, filePath string) (int64, int64, error) {
	resolved, handled, err := s.resolveKVSPath(ctx, storageID, filePath)
	if err != nil {
		return 0, 0, err
	}
	if !handled {
		return 0, 0, nil
	}
	if resolved == nil || !resolved.isDir {
		return 0, 0, nil
	}
	files, dirsDeleted, err := s.collectKVSFiles(ctx, resolved.logicalPath)
	if err != nil {
		return 0, 0, status.Errorf(codes.Internal, "kvs directory walk failed: %v", err)
	}
	if len(files) == 0 {
		return 0, dirsDeleted, nil
	}
	deletes := make([]kvsapi.PathDelete, 0, len(files))
	for _, logicalPath := range files {
		deletes = append(deletes, kvsapi.PathDelete{LogicalPath: logicalPath})
	}
	if _, err := s.kvsStore.DeletePaths(ctx, kvsapi.DeleteBatch{
		Deletes: deletes,
		Context: kvsapi.MutationContext{PrincipalID: "monofs-node", Reason: "monofs kvs directory delete"},
	}); err != nil {
		return 0, 0, status.Errorf(codes.Internal, "kvs directory delete failed: %v", err)
	}
	return int64(len(deletes)), dirsDeleted, nil
}

func (s *Server) collectKVSFiles(ctx context.Context, logicalDir string) ([]string, int64, error) {
	entries, err := s.kvsStore.ListDir(ctx, logicalDir)
	if err != nil {
		return nil, 0, err
	}
	files := make([]string, 0, len(entries))
	dirsDeleted := int64(1)
	for _, entry := range entries {
		childLogicalPath := kvsChildLogicalPath(logicalDir, entry.Name)
		if entry.IsDir {
			nestedFiles, nestedDirs, err := s.collectKVSFiles(ctx, childLogicalPath)
			if err != nil {
				return nil, 0, err
			}
			files = append(files, nestedFiles...)
			dirsDeleted += nestedDirs
			continue
		}
		files = append(files, childLogicalPath)
	}
	return files, dirsDeleted, nil
}

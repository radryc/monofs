package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/rydzu/ainfra/cfg"
	"github.com/rydzu/ainfra/kvs/pkg/kvsapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const storageBackendCfg = "cfg"

// CfgBackendStore is the interface that CfgStore must satisfy for use in the
// MonoFS server. It exposes the read and watch interfaces of kvsapi.Store plus
// the cfg-specific management methods the server needs for ingestion.
type CfgBackendStore interface {
	// kvsapi.ReadStore
	ReadFile(ctx context.Context, logicalPath string) ([]byte, error)
	ListDir(ctx context.Context, logicalDir string) ([]kvsapi.DirEntry, error)
	Stat(ctx context.Context, logicalPath string) (kvsapi.FileInfo, error)

	// kvsapi.WatchStore
	Watch(ctx context.Context, prefixes []string) (<-chan kvsapi.ChangeEvent, error)

	// Config management
	PushVersion(ctx context.Context, product, sha string, files map[string][]byte) error
	SetCurrent(ctx context.Context, product, sha string) error
	GetCurrent(ctx context.Context, product string) (string, error)
	ListProductVersions(ctx context.Context, product string) ([]cfg.VersionRecord, error)
	Compact(ctx context.Context, product string) error
}

// SetCfgStore wires a CfgBackendStore into the server.
// Call this before serving requests if any repository uses the "cfg" backend.
func (s *Server) SetCfgStore(store CfgBackendStore) {
	s.cfgStore = store
}

// usesCfgBackend reports whether the repository uses the cfg storage backend.
func (info repoInfo) usesCfgBackend() bool {
	return normalizeStorageBackend(info.StorageBackend) == storageBackendCfg
}

// resolveCfgPath resolves a logical path within a cfg-backed repository.
// It returns (resolved, true, nil) when the path belongs to this store,
// (nil, true, err) when an error occurs, and (nil, false, nil) when the
// repository does not use the cfg backend.
func (s *Server) resolveCfgPath(ctx context.Context, storageID, filePath string) (*kvsResolvedPath, bool, error) {
	repo, ok := s.repoInfoByStorageID(storageID)
	if !ok || !repo.usesCfgBackend() {
		return nil, false, nil
	}
	if s.cfgStore == nil {
		return nil, true, status.Error(codes.FailedPrecondition, "cfg-backed repository registered but no cfg store is configured")
	}

	logicalPath := kvsLogicalPath(repo.DisplayPath, filePath)

	info, err := s.cfgStore.Stat(ctx, logicalPath)
	if err == nil {
		return &kvsResolvedPath{logicalPath: logicalPath, isDir: false, size: info.Size, modTime: info.ModTime}, true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, true, status.Errorf(codes.Internal, "cfg stat %s: %v", logicalPath, err)
	}

	// Check if it's a directory.
	entries, err := s.cfgStore.ListDir(ctx, logicalPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, true, nil
		}
		return nil, true, status.Errorf(codes.Internal, "cfg listdir %s: %v", logicalPath, err)
	}
	if len(entries) == 0 {
		return nil, true, nil
	}
	return &kvsResolvedPath{logicalPath: logicalPath, isDir: true, entries: entries, modTime: time.Now().UTC()}, true, nil
}

// ingestCfgFiles handles IngestFileBatch for cfg-backed repositories.
//
// Ingestion protocol:
//   - Each file's path must be of the form "{sha}/{relative-file-path}".
//   - Files with the same sha are grouped and pushed as one version via PushVersion.
//   - A file with BackendMetadata["cfg_set_current"] == "true" triggers SetCurrent
//     for its sha after PushVersion succeeds.
//
// Returns (filesIngested, filesFailed, error).
func (s *Server) ingestCfgFiles(ctx context.Context, displayPath string, files []*pb.FileMetadata) (int64, int64, error) {
	if s.cfgStore == nil {
		return 0, int64(len(files)), status.Error(codes.FailedPrecondition, "cfg-backed ingest requested but cfg store is not configured")
	}

	product := cfgProductFromDisplayPath(displayPath)
	if product == "" {
		return 0, int64(len(files)), status.Errorf(codes.InvalidArgument, "cfg: could not determine product from display path %q", displayPath)
	}

	// Group files by SHA (first path component).
	type versionFiles struct {
		files      map[string][]byte
		setCurrent bool
	}
	byVersion := make(map[string]*versionFiles)
	// Preserve insertion order so we can process in the order received.
	var versionOrder []string
	seenVersion := make(map[string]bool)

	for _, meta := range files {
		if meta == nil {
			continue
		}
		if meta.GetBackendMetadata()["dir_hint"] == "true" || meta.GetBackendMetadata()["file_type"] == "1" {
			continue
		}

		sha, relPath, ok := splitSHAFromPath(meta.GetPath())
		if !ok {
			s.logger.Warn("cfg ingest: skipping file with no sha prefix", "path", meta.GetPath())
			continue
		}
		if relPath == "" {
			s.logger.Warn("cfg ingest: skipping file with empty relative path after sha", "path", meta.GetPath())
			continue
		}

		if _, exists := byVersion[sha]; !exists {
			byVersion[sha] = &versionFiles{files: make(map[string][]byte)}
		}
		if !seenVersion[sha] {
			seenVersion[sha] = true
			versionOrder = append(versionOrder, sha)
		}

		content := append([]byte(nil), meta.GetInlineContent()...)
		byVersion[sha].files[relPath] = content

		if meta.GetBackendMetadata()["cfg_set_current"] == "true" {
			byVersion[sha].setCurrent = true
		}
	}

	var ingested, failed int64
	for _, sha := range versionOrder {
		vf := byVersion[sha]
		if err := s.cfgStore.PushVersion(ctx, product, sha, vf.files); err != nil {
			s.logger.Error("cfg ingest: PushVersion failed",
				"product", product, "sha", sha, "error", err)
			failed += int64(len(vf.files))
			continue
		}
		if vf.setCurrent {
			if err := s.cfgStore.SetCurrent(ctx, product, sha); err != nil {
				s.logger.Warn("cfg ingest: SetCurrent failed",
					"product", product, "sha", sha, "error", err)
			}
		}
		ingested += int64(len(vf.files))
	}

	if failed > 0 {
		return ingested, failed, fmt.Errorf("cfg ingest: %d file(s) failed to push", failed)
	}
	return ingested, 0, nil
}

// cfgProductFromDisplayPath extracts the product name from the display path.
// The product is the last non-empty path component.
// e.g. "/cfg/my-app" → "my-app"
func cfgProductFromDisplayPath(displayPath string) string {
	trimmed := strings.Trim(displayPath, "/")
	if trimmed == "" {
		return ""
	}
	parts := strings.Split(trimmed, "/")
	return parts[len(parts)-1]
}

// splitSHAFromPath splits a path of the form "{sha}/{relative...}" into its
// SHA prefix and the remainder.
// Returns ok=false if the path has no "/" separator.
func splitSHAFromPath(path string) (sha, relPath string, ok bool) {
	idx := strings.Index(path, "/")
	if idx < 0 {
		return "", "", false
	}
	return path[:idx], path[idx+1:], true
}

// cfgMode returns the POSIX mode bits for a cfg entry.
func cfgMode(isDir bool) uint32 {
	if isDir {
		return 0o755 | uint32(syscall.S_IFDIR)
	}
	return 0o644 | uint32(syscall.S_IFREG)
}

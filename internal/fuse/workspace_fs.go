package fuse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	monoclient "github.com/radryc/monofs/internal/client"
)

// Headless workspace client errors. The session socket maps these to plain
// messages; callers that need programmatic handling can use errors.Is.
var (
	ErrFSNotFound    = errors.New("path not found")
	ErrFSIsDir       = errors.New("path is a directory")
	ErrFSNotDir      = errors.New("path is not a directory")
	ErrFSReadOnly    = errors.New("path is read-only")
	ErrFSInvalidPath = errors.New("invalid path")
	ErrFSIsSymlink   = errors.New("path is a symlink")
)

// FileStat describes one file or directory in the MonoFS workspace.
type FileStat struct {
	Path          string `json:"path"`
	Name          string `json:"name,omitempty"`
	Mode          uint32 `json:"mode"`
	Size          int64  `json:"size"`
	Mtime         int64  `json:"mtime,omitempty"`
	IsDir         bool   `json:"is_dir,omitempty"`
	SymlinkTarget string `json:"symlink_target,omitempty"`
}

// WorkspaceFS implements filesystem operations against a MonoFS cluster and
// the local write overlay without requiring a FUSE mount. It is the read/write
// backend for the headless monofs-client mode and for the extended session
// socket verbs (list, read, stat, write, mkdir).
//
// Semantics mirror the FUSE layer: overlay files shadow backend content,
// deletions are respected, user-root directories are local-only, and virtual
// monorepo filters hide system namespaces.
type WorkspaceFS struct {
	client     monoclient.MonoFSClient
	sessionMgr *SessionManager
	workspace  *WorkspaceManifest
	logger     *slog.Logger
}

// NewWorkspaceFS creates a headless workspace filesystem. sessionMgr may be
// nil for a read-only client; workspace may be nil when virtual monorepo mode
// is disabled.
func NewWorkspaceFS(client monoclient.MonoFSClient, sessionMgr *SessionManager, workspace *WorkspaceManifest, logger *slog.Logger) *WorkspaceFS {
	if logger == nil {
		logger = slog.Default()
	}
	return &WorkspaceFS{
		client:     client,
		sessionMgr: sessionMgr,
		workspace:  workspace,
		logger:     logger.With("component", "workspace-fs"),
	}
}

// cleanFSPath normalises a workspace-relative path and rejects traversal.
func cleanFSPath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "." || trimmed == "/" {
		return "", nil
	}
	trimmed = strings.Trim(trimmed, "/")
	cleaned := path.Clean(trimmed)
	if cleaned == "." {
		return "", nil
	}
	for _, part := range strings.Split(cleaned, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: %q escapes the workspace root", ErrFSInvalidPath, raw)
		}
	}
	return cleaned, nil
}

func (fs *WorkspaceFS) backendPath(monofsPath string) string {
	if mapped, ok := backendPathForSystemView(monofsPath); ok {
		return mapped
	}
	return monofsPath
}

func (fs *WorkspaceFS) isWorkspaceReadOnly(monofsPath string) bool {
	if isWorkspaceReadOnlyPath(monofsPath) {
		return true
	}
	return fs.workspace != nil && fs.workspace.ShouldHidePath(monofsPath)
}

func (fs *WorkspaceFS) syntheticContent(ctx context.Context, monofsPath string) ([]byte, bool, error) {
	if fs.workspace == nil {
		return nil, false, nil
	}
	switch monofsPath {
	case syntheticGitignoreName:
		return fs.workspace.GitignoreContent(), true, nil
	case syntheticWorkspaceManifestPath:
		content, err := fs.workspace.JSONContent(ctx)
		if err != nil {
			return nil, true, err
		}
		return content, true, nil
	}
	return nil, false, nil
}

func (fs *WorkspaceFS) syntheticDir(monofsPath string) (bool, uint32) {
	if fs.workspace == nil {
		return false, 0
	}
	switch monofsPath {
	case syntheticWorkspaceControlPath, syntheticWorkspaceSystemPath:
		return true, 0555 | uint32(syscall.S_IFDIR)
	}
	return false, 0
}

// Stat returns metadata for a workspace path, merging overlay state with
// backend metadata.
func (fs *WorkspaceFS) Stat(ctx context.Context, rawPath string) (*FileStat, error) {
	monofsPath, err := cleanFSPath(rawPath)
	if err != nil {
		return nil, err
	}
	stat, found, err := fs.statInternal(ctx, monofsPath)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrFSNotFound, displayPath(monofsPath))
	}
	return stat, nil
}

func (fs *WorkspaceFS) statInternal(ctx context.Context, monofsPath string) (*FileStat, bool, error) {
	if monofsPath == "" {
		return &FileStat{Path: "", Mode: 0755 | uint32(syscall.S_IFDIR), IsDir: true}, true, nil
	}
	if dir, mode := fs.syntheticDir(monofsPath); dir {
		return &FileStat{Path: monofsPath, Name: path.Base(monofsPath), Mode: mode, IsDir: true}, true, nil
	}
	if content, synthetic, err := fs.syntheticContent(ctx, monofsPath); synthetic {
		if err != nil {
			return nil, false, err
		}
		return &FileStat{
			Path:  monofsPath,
			Name:  path.Base(monofsPath),
			Mode:  0444 | uint32(syscall.S_IFREG),
			Size:  int64(len(content)),
			Mtime: time.Now().Unix(),
		}, true, nil
	}
	if fs.workspace != nil && fs.workspace.ShouldHidePath(monofsPath) {
		return nil, false, nil
	}

	if isWorkspaceSystemPath(monofsPath) {
		// System view bypasses the overlay and workspace filters, exactly like
		// the FUSE layer.
		return fs.statBackend(ctx, monofsPath)
	}

	if fs.sessionMgr != nil {
		state := fs.sessionMgr.GetPathState(monofsPath)
		if state.IsDeleted {
			return nil, false, nil
		}
		if state.IsSymlink {
			target := state.SymlinkTarget
			return &FileStat{
				Path:          monofsPath,
				Name:          path.Base(monofsPath),
				Mode:          0777 | uint32(syscall.S_IFLNK),
				Size:          int64(len(target)),
				SymlinkTarget: target,
				Mtime:         time.Now().Unix(),
			}, true, nil
		}
		if state.HasOverride {
			return statLocal(monofsPath, state.LocalPath)
		}
		parts := splitPath(monofsPath)
		if len(parts) > 1 && fs.sessionMgr.IsUserRootDir(parts[0]) {
			return statLocal(monofsPath, state.LocalPath)
		}
	}

	if fs.client == nil {
		return nil, false, fmt.Errorf("no backend client configured")
	}
	return fs.statBackend(ctx, monofsPath)
}

func (fs *WorkspaceFS) statBackend(ctx context.Context, monofsPath string) (*FileStat, bool, error) {
	resp, err := fs.client.GetAttr(ctx, fs.backendPath(monofsPath))
	if err != nil {
		if isNotFoundStatus(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get attrs for %q: %w", monofsPath, err)
	}
	if resp == nil || !resp.Found {
		return nil, false, nil
	}
	return &FileStat{
		Path:  monofsPath,
		Name:  path.Base(monofsPath),
		Mode:  resp.Mode,
		Size:  int64(resp.Size),
		Mtime: resp.Mtime,
		IsDir: resp.Mode&uint32(syscall.S_IFDIR) != 0,
	}, true, nil
}

func statLocal(monofsPath, localPath string) (*FileStat, bool, error) {
	info, err := os.Lstat(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat %q: %w", monofsPath, err)
	}
	mode := uint32(info.Mode() & 0777)
	stat := &FileStat{
		Path:  monofsPath,
		Name:  path.Base(monofsPath),
		Size:  info.Size(),
		Mtime: info.ModTime().Unix(),
		IsDir: info.IsDir(),
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		stat.Mode = mode | uint32(syscall.S_IFLNK)
		if target, err := os.Readlink(localPath); err == nil {
			stat.SymlinkTarget = target
		}
	case info.IsDir():
		stat.Mode = mode | uint32(syscall.S_IFDIR)
	default:
		stat.Mode = mode | uint32(syscall.S_IFREG)
	}
	return stat, true, nil
}

// List returns the merged directory listing for a workspace path.
func (fs *WorkspaceFS) List(ctx context.Context, rawPath string) ([]FileStat, error) {
	monofsPath, err := cleanFSPath(rawPath)
	if err != nil {
		return nil, err
	}
	if fs.workspace != nil && fs.workspace.ShouldHidePath(monofsPath) {
		return nil, fmt.Errorf("%w: %s", ErrFSNotFound, displayPath(monofsPath))
	}
	if monofsPath == syntheticWorkspaceControlPath {
		return []FileStat{
			{Path: syntheticWorkspaceSystemPath, Name: syntheticWorkspaceSystemDirName, Mode: 0555 | uint32(syscall.S_IFDIR), IsDir: true},
			{Path: syntheticWorkspaceManifestPath, Name: syntheticWorkspaceManifestName, Mode: 0444 | uint32(syscall.S_IFREG)},
		}, nil
	}
	// .monofs/system projects the backend root. Keep the original path for
	// child names while listing the mapped backend path unfiltered.
	systemView := isWorkspaceSystemPath(monofsPath)
	dirPath := monofsPath
	if systemView {
		dirPath = fs.backendPath(monofsPath)
	}

	userDirList := false
	if !systemView && dirPath != "" && fs.sessionMgr != nil {
		state := fs.sessionMgr.GetPathState(dirPath)
		if state.IsDeleted {
			return nil, fmt.Errorf("%w: %s", ErrFSNotFound, displayPath(dirPath))
		}
		if state.HasOverride && !fs.isLocalDir(state.LocalPath) {
			return nil, fmt.Errorf("%w: %s", ErrFSNotDir, displayPath(dirPath))
		}
		parts := splitPath(dirPath)
		userDirList = fs.sessionMgr.IsUserRootDir(parts[0])
		if userDirList {
			localPath, err := fs.sessionMgr.GetLocalPath(dirPath)
			if err != nil {
				return nil, fmt.Errorf("%w: %s", ErrFSNotFound, displayPath(dirPath))
			}
			info, err := os.Lstat(localPath)
			if err != nil {
				return nil, fmt.Errorf("%w: %s", ErrFSNotFound, displayPath(dirPath))
			}
			if !info.IsDir() {
				return nil, fmt.Errorf("%w: %s", ErrFSNotDir, displayPath(dirPath))
			}
		}
	}

	var backendEntries []fuse.DirEntry
	if !userDirList {
		if fs.client == nil {
			return nil, fmt.Errorf("no backend client configured")
		}
		entries, err := fs.client.ReadDir(ctx, fs.backendPath(dirPath))
		if err != nil {
			if isNotFoundStatus(err) {
				return nil, fmt.Errorf("%w: %s", ErrFSNotFound, displayPath(dirPath))
			}
			return nil, fmt.Errorf("list %q: %w", dirPath, err)
		}
		backendEntries = make([]fuse.DirEntry, 0, len(entries))
		for _, entry := range entries {
			backendEntries = append(backendEntries, fuse.DirEntry{
				Name: entry.Name,
				Mode: entry.Mode,
				Ino:  entry.Ino,
			})
		}
	}

	merged := backendEntries
	if fs.sessionMgr != nil && !systemView {
		// A nil slice makes MergeReadDir treat the directory as user-owned and
		// scan the overlay disk tree (authoritative for user-root dirs).
		if userDirList {
			merged = nil
		}
		merged = NewOverlayManager(fs.sessionMgr).MergeReadDir(merged, dirPath)
	}
	if fs.workspace != nil && !systemView {
		merged = fs.workspace.FilterDirEntries(dirPath, merged)
	} else {
		sort.Slice(merged, func(i, j int) bool { return merged[i].Name < merged[j].Name })
	}

	out := make([]FileStat, 0, len(merged))
	db := fs.overlayDB()
	for _, entry := range merged {
		childPath := joinWorkspacePath(monofsPath, entry.Name)
		stat := FileStat{
			Path:  childPath,
			Name:  entry.Name,
			Mode:  entry.Mode,
			IsDir: entry.Mode&uint32(syscall.S_IFDIR) != 0,
		}
		if !stat.IsDir {
			if userDirList {
				if local, err := fs.sessionMgr.GetLocalPath(childPath); err == nil {
					if info, err := os.Lstat(local); err == nil {
						stat.Size = info.Size()
					}
				}
			} else if db != nil {
				if fileEntry, found, err := db.GetFile(childPath); err == nil && found {
					stat.Size = int64(fileEntry.Size)
				}
			}
		}
		out = append(out, stat)
	}
	return out, nil
}

// Read reads a byte range. size <= 0 reads from offset to EOF.
func (fs *WorkspaceFS) Read(ctx context.Context, rawPath string, offset, size int64) ([]byte, error) {
	monofsPath, err := cleanFSPath(rawPath)
	if err != nil {
		return nil, err
	}
	if offset < 0 || size < 0 {
		return nil, fmt.Errorf("%w: negative offset or size", ErrFSInvalidPath)
	}
	if monofsPath == "" {
		return nil, fmt.Errorf("%w: workspace root", ErrFSIsDir)
	}
	if dir, _ := fs.syntheticDir(monofsPath); dir {
		return nil, fmt.Errorf("%w: %s", ErrFSIsDir, monofsPath)
	}
	if content, synthetic, err := fs.syntheticContent(ctx, monofsPath); synthetic {
		if err != nil {
			return nil, err
		}
		return sliceRange(content, offset, size), nil
	}
	if fs.workspace != nil && fs.workspace.ShouldHidePath(monofsPath) {
		return nil, fmt.Errorf("%w: %s", ErrFSNotFound, monofsPath)
	}
	if isWorkspaceSystemPath(monofsPath) {
		return fs.readBackend(ctx, monofsPath, offset, size)
	}

	if fs.sessionMgr != nil {
		state := fs.sessionMgr.GetPathState(monofsPath)
		if state.IsDeleted {
			return nil, fmt.Errorf("%w: %s", ErrFSNotFound, monofsPath)
		}
		if state.IsSymlink {
			return nil, fmt.Errorf("%w: %s -> %s", ErrFSIsSymlink, monofsPath, state.SymlinkTarget)
		}
		if state.HasOverride {
			return readLocalRange(monofsPath, state.LocalPath, offset, size)
		}
		parts := splitPath(monofsPath)
		if len(parts) > 1 && fs.sessionMgr.IsUserRootDir(parts[0]) {
			return readLocalRange(monofsPath, state.LocalPath, offset, size)
		}
	}

	if fs.client == nil {
		return nil, fmt.Errorf("no backend client configured")
	}
	return fs.readBackend(ctx, monofsPath, offset, size)
}

func (fs *WorkspaceFS) readBackend(ctx context.Context, monofsPath string, offset, size int64) ([]byte, error) {
	content, err := fs.client.Read(ctx, fs.backendPath(monofsPath), offset, size)
	if err != nil {
		if isNotFoundStatus(err) {
			return nil, fmt.Errorf("%w: %s", ErrFSNotFound, monofsPath)
		}
		return nil, fmt.Errorf("read %q: %w", monofsPath, err)
	}
	return content, nil
}

// Write stores content in the local overlay and records the change.
func (fs *WorkspaceFS) Write(ctx context.Context, rawPath string, content []byte, mode uint32) (*FileStat, error) {
	return fs.WriteWithParents(ctx, rawPath, content, mode, false)
}

// WriteWithParents is Write with optional creation of a missing top-level
// user directory (the headless equivalent of `mkdir -p` at the workspace root).
func (fs *WorkspaceFS) WriteWithParents(ctx context.Context, rawPath string, content []byte, mode uint32, createParents bool) (*FileStat, error) {
	monofsPath, err := cleanFSPath(rawPath)
	if err != nil {
		return nil, err
	}
	if monofsPath == "" {
		return nil, fmt.Errorf("%w: cannot write to the workspace root", ErrFSReadOnly)
	}
	if fs.sessionMgr == nil {
		return nil, fmt.Errorf("%w: no write session manager", ErrFSReadOnly)
	}
	if isDependencyPath(monofsPath) {
		return nil, fmt.Errorf("%w: %q is a dependency path; use push-blobs for dependency changes", ErrFSReadOnly, monofsPath)
	}
	if fs.isWorkspaceReadOnly(monofsPath) {
		return nil, fmt.Errorf("%w: %s", ErrFSReadOnly, monofsPath)
	}

	if _, err := fs.sessionMgr.StartSession(); err != nil {
		return nil, fmt.Errorf("start write session: %w", err)
	}
	if createParents {
		if err := fs.ensureRootParent(ctx, monofsPath); err != nil {
			return nil, err
		}
	} else if parent := path.Dir(monofsPath); parent != "." && parent != "" {
		stat, found, err := fs.statInternal(ctx, parent)
		if err != nil {
			return nil, err
		}
		if !found || !stat.IsDir {
			return nil, fmt.Errorf("%w: parent directory %q does not exist", ErrFSNotFound, parent)
		}
	}

	existing, found, err := fs.statInternal(ctx, monofsPath)
	if err != nil {
		return nil, err
	}
	if found && existing.IsDir {
		return nil, fmt.Errorf("%w: %s", ErrFSIsDir, monofsPath)
	}
	if found && existing.SymlinkTarget != "" {
		return nil, fmt.Errorf("%w: %s -> %s", ErrFSIsSymlink, monofsPath, existing.SymlinkTarget)
	}
	if !found && !strings.Contains(monofsPath, "/") {
		return nil, fmt.Errorf("%w: cannot create files at the workspace root; use mkdir first", ErrFSReadOnly)
	}

	localPath, err := fs.sessionMgr.GetLocalPath(monofsPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return nil, fmt.Errorf("create parent directories for %q: %w", monofsPath, err)
	}

	fileMode := os.FileMode(mode & 0777)
	if fileMode == 0 {
		if found {
			fileMode = os.FileMode(existing.Mode & 0777)
		} else {
			fileMode = 0644
		}
	}
	if err := os.WriteFile(localPath, content, fileMode); err != nil {
		return nil, fmt.Errorf("write %q: %w", monofsPath, err)
	}
	// WriteFile only applies the mode when it creates the file; preserve the
	// requested overlay mode when replacing an existing local copy.
	if err := os.Chmod(localPath, fileMode); err != nil {
		return nil, fmt.Errorf("chmod %q: %w", monofsPath, err)
	}

	changeType := ChangeCreate
	parts := splitPath(monofsPath)
	if len(parts) > 0 && !fs.sessionMgr.IsUserRootDir(parts[0]) && found {
		changeType = ChangeModify
	}
	if err := fs.sessionMgr.TrackChange(changeType, monofsPath, ""); err != nil {
		return nil, fmt.Errorf("track write for %q: %w", monofsPath, err)
	}
	fs.logger.Debug("headless write", "path", monofsPath, "bytes", len(content), "change", changeType)

	state := fs.sessionMgr.GetPathState(monofsPath)
	stat, ok, err := statLocal(monofsPath, state.LocalPath)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s after write", ErrFSNotFound, monofsPath)
	}
	return stat, nil
}

// ensureRootParent creates the top-level user directory for a path when the
// backend does not know it (e.g. `.deps` for `.deps/lib.go`). Repository
// namespaces already exist on the backend and are left alone, so local
// directories never shadow them.
func (fs *WorkspaceFS) ensureRootParent(ctx context.Context, monofsPath string) error {
	parts := splitPath(monofsPath)
	if len(parts) < 2 {
		return nil
	}
	top := parts[0]
	if fs.sessionMgr.IsUserRootDir(top) {
		return nil
	}
	// Dot-prefixed roots are workspace-local by convention (e.g. `.deps`);
	// create them without an RPC so cache writes work during backend blips.
	if strings.HasPrefix(top, ".") {
		return fs.sessionMgr.CreateUserRootDir(top)
	}
	_, found, err := fs.statInternal(ctx, top)
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	return fs.sessionMgr.CreateUserRootDir(top)
}

// Mkdir creates a directory in the overlay. Root-level directories become
// user root directories, matching the FUSE create semantics.
func (fs *WorkspaceFS) Mkdir(ctx context.Context, rawPath string, mode uint32) (*FileStat, error) {
	monofsPath, err := cleanFSPath(rawPath)
	if err != nil {
		return nil, err
	}
	if monofsPath == "" {
		return nil, fmt.Errorf("%w: cannot create the workspace root", ErrFSReadOnly)
	}
	if fs.sessionMgr == nil {
		return nil, fmt.Errorf("%w: no write session manager", ErrFSReadOnly)
	}
	if isDependencyPath(monofsPath) || fs.isWorkspaceReadOnly(monofsPath) {
		return nil, fmt.Errorf("%w: %s", ErrFSReadOnly, monofsPath)
	}

	if _, err := fs.sessionMgr.StartSession(); err != nil {
		return nil, fmt.Errorf("start write session: %w", err)
	}

	if !strings.Contains(monofsPath, "/") {
		// Root-level directories are user-owned and never probed on the
		// backend, matching the FUSE Mkdir path.
		if !fs.sessionMgr.IsUserRootDir(monofsPath) {
			if err := fs.sessionMgr.CreateUserRootDir(monofsPath); err != nil {
				return nil, err
			}
			if mode&0777 != 0 {
				if local, err := fs.sessionMgr.GetLocalPath(monofsPath); err == nil {
					_ = os.Chmod(local, os.FileMode(mode&0777))
				}
			}
		}
	} else {
		localPath, err := fs.sessionMgr.GetLocalPath(monofsPath)
		if err != nil {
			return nil, err
		}
		if info, err := os.Lstat(localPath); err == nil {
			if !info.IsDir() {
				return nil, fmt.Errorf("%w: %s", ErrFSNotDir, monofsPath)
			}
			stat, _, err := statLocal(monofsPath, localPath)
			return stat, err
		}
		dirMode := os.FileMode(mode & 0777)
		if dirMode == 0 {
			dirMode = 0755
		}
		if err := os.MkdirAll(localPath, dirMode); err != nil {
			return nil, fmt.Errorf("create directory %q: %w", monofsPath, err)
		}
		if err := fs.sessionMgr.TrackChange(ChangeMkdir, monofsPath, ""); err != nil {
			return nil, fmt.Errorf("track directory %q: %w", monofsPath, err)
		}
	}

	state := fs.sessionMgr.GetPathState(monofsPath)
	stat, ok, err := statLocal(monofsPath, state.LocalPath)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s after mkdir", ErrFSNotFound, monofsPath)
	}
	return stat, nil
}

func (fs *WorkspaceFS) overlayDB() *OverlayDB {
	if fs.sessionMgr == nil {
		return nil
	}
	return fs.sessionMgr.GetOverlayDB()
}

func (fs *WorkspaceFS) isLocalDir(localPath string) bool {
	info, err := os.Lstat(localPath)
	return err == nil && info.IsDir()
}

func displayPath(monofsPath string) string {
	if monofsPath == "" {
		return "/"
	}
	return monofsPath
}

func sliceRange(content []byte, offset, size int64) []byte {
	if offset >= int64(len(content)) {
		return nil
	}
	end := int64(len(content))
	if size > 0 && offset+size < end {
		end = offset + size
	}
	return content[offset:end]
}

func readLocalRange(monofsPath, localPath string, offset, size int64) ([]byte, error) {
	file, err := os.Open(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrFSNotFound, monofsPath)
		}
		return nil, fmt.Errorf("open %q: %w", monofsPath, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", monofsPath, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrFSIsDir, monofsPath)
	}
	if offset >= info.Size() {
		return nil, nil
	}
	length := info.Size() - offset
	if size > 0 && size < length {
		length = size
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(io.NewSectionReader(file, offset, length), buf); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read %q: %w", monofsPath, err)
	}
	return buf, nil
}

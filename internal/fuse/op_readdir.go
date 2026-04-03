package fuse

import (
	"context"
	"fmt"
	"sort"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/radryc/monofs/internal/cache"
)

// Readdir reads directory entries.
func (n *MonoNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	defer n.recoverPanic("Readdir")
	n.client.RecordOperation()
	n.logger.Debug("readdir", "path", n.path)

	// Check if this is a user-created root directory (or subdirectory of one)
	if n.sessionMgr != nil && n.path != "" {
		parts := splitPath(n.path)
		if len(parts) >= 1 && n.sessionMgr.IsUserRootDir(parts[0]) {
			// This is a user directory - only return local overlay entries
			n.logger.Debug("readdir: user directory", "path", n.path)
			overlay := NewOverlayManager(n.sessionMgr)
			dirEntries := overlay.MergeReadDir(nil, n.path)
			return fs.NewListDirStream(dirEntries), 0
		}
	}

	// Try cache first if available
	if n.cache != nil {
		if entries, err := n.cache.GetDir(n.path); err == nil {
			n.logger.Debug("readdir cache hit", "path", n.path, "count", len(entries))
			// Convert cache entries to fuse entries
			dirEntries := make([]fuse.DirEntry, len(entries))
			for i, e := range entries {
				dirEntries[i] = fuse.DirEntry{
					Name: e.Name,
					Mode: e.Mode,
					Ino:  e.Ino,
				}
			}
			// Add FS_ERROR.txt if this is root and there's a catastrophic error
			if n.path == "" {
				if rootNode := n.getRootNode(); rootNode != nil {
					rootNode.catErrorMu.RLock()
					hasCatError := rootNode.catastrophicError != ""
					rootNode.catErrorMu.RUnlock()
					if hasCatError {
						dirEntries = append(dirEntries, fuse.DirEntry{
							Name: "FS_ERROR.txt",
							Mode: 0444 | uint32(syscall.S_IFREG),
							Ino:  0xFFFFFFFF,
						})
					}
				}
			}
			// Still need to merge overlay even for cached results
			if n.sessionMgr != nil {
				overlay := NewOverlayManager(n.sessionMgr)
				dirEntries = overlay.MergeReadDir(dirEntries, n.path)
			}
			return fs.NewListDirStream(dirEntries), 0
		}
		n.logger.Debug("readdir cache miss", "path", n.path)
	}

	// Check for catastrophic error file in root directory
	if n.path == "" {
		if rootNode := n.getRootNode(); rootNode != nil {
			rootNode.catErrorMu.RLock()
			hasCatastrophicError := rootNode.catastrophicError != ""
			rootNode.catErrorMu.RUnlock()

			if hasCatastrophicError {
				n.logger.Debug("catastrophic error exists, including FS_ERROR.txt")
			}
		}
	}

	// Query backend with retry on transient failures.
	// A single transient network/timeout error should not immediately return
	// EIO — retry once for all paths. For dependency paths this is critical:
	// go mod verify computes directory hashes and an incomplete listing
	// produces a "dir has been modified" error.
	n.logger.Debug("readdir calling backend rpc", "path", n.path)
	backendEntries, err := n.client.ReadDir(ctx, n.path)
	if err != nil {
		// Invalidate the dir cache for this path so we don't serve stale data.
		if n.cache != nil {
			n.cache.Invalidate(n.path)
		}
		n.logger.Info("readdir: retrying after error",
			"path", n.path, "first_error", err)

		// If the FUSE request context is still live, wait before retrying.
		// If it has been cancelled (FUSE_INTERRUPT), skip the wait — the
		// select would return EINTR immediately anyway.
		if ctx.Err() == nil {
			select {
			case <-ctx.Done():
				// Context cancelled while waiting; fall through to retry with
				// a fresh context rather than abandoning the listing.
			case <-time.After(retryDelay(0)):
			}
		}

		// Use a fresh context for the retry: the FUSE request context may have
		// been cancelled (FUSE_INTERRUPT) because the kernel thought the
		// operation was taking too long, but callers like go mod verify need
		// the complete listing to produce a correct directory hash. Passing the
		// cancelled ctx would cause an immediate second failure.
		retryCtx, retryCancel := context.WithTimeout(context.Background(), readdirTimeout)
		defer retryCancel()
		backendEntries, err = n.client.ReadDir(retryCtx, n.path)
	}
	if err != nil {
		n.logger.Debug("readdir failed", "path", n.path, "error", err)
		n.updateBackendError(err)
		// For root directory, return FS_ERROR.txt instead of failing
		if n.path == "" {
			n.logger.Debug("readdir backend error, returning error file", "error", err)

			// Store error in root node
			if rootNode := n.getRootNode(); rootNode != nil {
				rootNode.catErrorMu.Lock()
				rootNode.catastrophicError = fmt.Sprintf("Backend error in Readdir: %v", err)
				rootNode.catErrorMu.Unlock()
			}

			errorEntry := []fuse.DirEntry{
				{
					Name: "FS_ERROR.txt",
					Mode: 0444 | uint32(syscall.S_IFREG),
					Ino:  0xFFFFFFFF,
				},
			}
			return fs.NewListDirStream(errorEntry), 0
		}
		return nil, n.recordAndConvertError(err)
	}
	// Clear backend error on success
	n.updateBackendError(nil)
	// Clear catastrophic error on success (backend has recovered)
	if n.path == "" {
		if rootNode := n.getRootNode(); rootNode != nil {
			rootNode.catErrorMu.Lock()
			if rootNode.catastrophicError != "" {
				n.logger.Info("backend recovered, clearing catastrophic error")
				rootNode.catastrophicError = ""
			}
			rootNode.catErrorMu.Unlock()
		}
	}
	n.logger.Debug("readdir backend returned", "path", n.path, "entries", len(backendEntries))

	// Convert to FUSE entries and cache entries
	dirEntries := make([]fuse.DirEntry, len(backendEntries))
	cacheEntries := make([]cache.DirEntry, len(backendEntries))

	for i, e := range backendEntries {
		n.logger.Debug("readdir entry",
			"path", n.path,
			"name", e.Name,
			"mode", fmt.Sprintf("0%o", e.Mode),
			"ino", e.Ino)
		dirEntries[i] = fuse.DirEntry{
			Name: e.Name,
			Mode: e.Mode,
			Ino:  e.Ino,
		}
		cacheEntries[i] = cache.DirEntry{
			Name: e.Name,
			Mode: e.Mode,
			Ino:  e.Ino,
		}
	}

	// Add FS_ERROR.txt if this is root and there's a catastrophic error or backend error
	if n.path == "" {
		showErrorFile := false
		if rootNode := n.getRootNode(); rootNode != nil {
			rootNode.catErrorMu.RLock()
			showErrorFile = rootNode.catastrophicError != ""
			rootNode.catErrorMu.RUnlock()
		}
		if !showErrorFile {
			showErrorFile = n.hasBackendError()
		}
		if showErrorFile {
			dirEntries = append(dirEntries, fuse.DirEntry{
				Name: "FS_ERROR.txt",
				Mode: 0444 | uint32(syscall.S_IFREG),
				Ino:  0xFFFFFFFF,
			})
		}
	}

	// Update cache if available
	if n.cache != nil {
		_ = n.cache.PutDir(n.path, cacheEntries)
	}

	// Merge local overlay changes if session manager is available
	if n.sessionMgr != nil {
		overlay := NewOverlayManager(n.sessionMgr)
		dirEntries = overlay.MergeReadDir(dirEntries, n.path)
	} else {
		// No overlay - still need to sort for deterministic ordering
		sort.Slice(dirEntries, func(i, j int) bool {
			return dirEntries[i].Name < dirEntries[j].Name
		})
	}

	n.logger.Debug("readdir complete", "path", n.path, "count", len(dirEntries))

	// Debug logging for go mod verify issues
	for i, e := range dirEntries {
		n.logger.Debug("readdir entry final",
			"path", n.path,
			"index", i,
			"name", e.Name,
			"mode", fmt.Sprintf("0%o", e.Mode))
	}

	return fs.NewListDirStream(dirEntries), 0
}

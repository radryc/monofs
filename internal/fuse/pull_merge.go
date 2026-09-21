package fuse

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pullMergeSummary reports the outcome of merging pending session
// changes across a workspace refresh.
type pullMergeSummary struct {
	Merged     int // files auto-merged with upstream changes
	Dropped    int // vacuous local changes replaced by upstream content
	Kept       int // local-only changes left untouched
	Conflicted int // conflicts recorded for manual resolution
	Conflicts  []SessionConflict
}

func (s *pullMergeSummary) message() string {
	if s.total() == 0 {
		return ""
	}
	msg := fmt.Sprintf("merge: %d merged, %d kept, %d dropped", s.Merged, s.Kept, s.Dropped)
	if s.Conflicted > 0 {
		msg += fmt.Sprintf(", %d CONFLICTED (run 'monofs-session conflicts')", s.Conflicted)
	}
	return msg
}

func (s *pullMergeSummary) total() int {
	return s.Merged + s.Dropped + s.Kept + s.Conflicted
}

// pullCapture snapshots one pending session change before a refresh so
// it can be 3-way merged against the refreshed upstream content.
type pullCapture struct {
	change Change
	// base is the pre-refresh backend content (nil when absent).
	base []byte
	// hasBase records whether the file existed in the backend before
	// the refresh.
	hasBase bool
	// ours is the local overlay content (nil for local deletions).
	ours []byte
}

// capturePullMergeState snapshots the workspace-scope session changes
// before a refresh. Blob (dependency) and excluded changes are skipped
// by the caller.
func (h *SessionSocketHandler) capturePullMergeState(ctx context.Context, changes []Change) (map[string]pullCapture, error) {
	captures := make(map[string]pullCapture, len(changes))

	for _, change := range changes {
		switch change.Type {
		case ChangeCreate, ChangeModify, ChangeDelete:
		default:
			// user root dirs and other session-local metadata are not
			// mergeable file content.
			continue
		}

		capture := pullCapture{change: change}

		if change.Type != ChangeCreate {
			base, err := h.readBackendContent(ctx, change.Path)
			if err == nil {
				capture.base = base
				capture.hasBase = true
			} else if !isNotFoundStatus(err) {
				return nil, fmt.Errorf("read base content for %s: %w", change.Path, err)
			}
			// A missing base means the file never existed in the
			// backend; the change behaves like a creation.
		}

		if change.Type != ChangeDelete {
			localPath := change.LocalPath
			if localPath == "" {
				resolved, err := h.sessionMgr.GetLocalPath(change.Path)
				if err != nil {
					return nil, fmt.Errorf("resolve local path for %s: %w", change.Path, err)
				}
				localPath = resolved
			}
			ours, err := os.ReadFile(localPath)
			if err != nil {
				return nil, fmt.Errorf("read local content for %s: %w", change.Path, err)
			}
			capture.ours = ours
			capture.change.LocalPath = localPath
		}

		captures[change.Path] = capture
	}

	return captures, nil
}

// applyPullMerge reconciles captured pre-refresh changes against the
// refreshed backend content using 3-way merges, writing merged content
// back into the overlay and recording conflicts.
func (h *SessionSocketHandler) applyPullMerge(ctx context.Context, captures map[string]pullCapture) *pullMergeSummary {
	summary := &pullMergeSummary{}

	for _, capture := range captures {
		switch capture.change.Type {
		case ChangeCreate:
			h.mergeCreate(ctx, capture, summary)
		case ChangeModify:
			h.mergeModify(ctx, capture, summary)
		case ChangeDelete:
			h.mergeDelete(ctx, capture, summary)
		}
	}

	if h.attrCache != nil {
		h.attrCache.Invalidate("")
	}
	return summary
}

func (h *SessionSocketHandler) mergeCreate(ctx context.Context, capture pullCapture, summary *pullMergeSummary) {
	theirs, theirsErr := h.readBackendContent(ctx, capture.change.Path)
	if theirsErr != nil {
		// File still absent upstream: the local creation stands.
		summary.Kept++
		return
	}

	if bytes.Equal(capture.ours, theirs) {
		// Both sides created identical content: drop the overlay entry.
		h.dropLocalChange(capture.change.Path)
		summary.Dropped++
		return
	}

	// Add/add: merge with an empty base. This will usually conflict.
	merged, conflicts := Merge3(nil, capture.ours, theirs)
	h.writeMergedContent(capture.change.Path, capture.change.LocalPath, merged)
	if conflicts > 0 {
		h.recordConflict(capture.change.Path, "both sides created the file", summary)
		return
	}
	summary.Merged++
}

func (h *SessionSocketHandler) mergeModify(ctx context.Context, capture pullCapture, summary *pullMergeSummary) {
	if !capture.hasBase {
		// No base existed pre-refresh; behave like a creation.
		capture.change.Type = ChangeCreate
		h.mergeCreate(ctx, capture, summary)
		return
	}

	theirs, theirsErr := h.readBackendContent(ctx, capture.change.Path)
	if theirsErr != nil {
		// Upstream deleted the file while it was modified locally.
		// Keep the local modification and flag the conflict.
		h.recordConflict(capture.change.Path, "local modify vs upstream delete", summary)
		return
	}

	switch {
	case bytes.Equal(capture.base, theirs):
		// Upstream unchanged: local modification stands.
		summary.Kept++
	case bytes.Equal(capture.ours, capture.base), bytes.Equal(capture.ours, theirs):
		// Local change is vacuous or matches upstream: take theirs.
		h.dropLocalChange(capture.change.Path)
		summary.Dropped++
	default:
		merged, conflicts := Merge3(capture.base, capture.ours, theirs)
		h.writeMergedContent(capture.change.Path, capture.change.LocalPath, merged)
		if conflicts > 0 {
			h.recordConflict(capture.change.Path, "both sides modified", summary)
			return
		}
		summary.Merged++
	}
}

func (h *SessionSocketHandler) mergeDelete(ctx context.Context, capture pullCapture, summary *pullMergeSummary) {
	theirs, theirsErr := h.readBackendContent(ctx, capture.change.Path)
	if theirsErr != nil {
		// Deleted on both sides: the deletion stands.
		summary.Kept++
		return
	}
	if bytes.Equal(capture.base, theirs) {
		// Upstream unchanged: the local deletion stands.
		summary.Kept++
		return
	}

	// Upstream modified the file the session deleted. Restore the
	// upstream content into the overlay and flag the conflict so the
	// user can decide to keep or re-delete it.
	localPath := capture.change.LocalPath
	if localPath == "" {
		resolved, err := h.sessionMgr.GetLocalPath(capture.change.Path)
		if err != nil {
			h.logger.Error("pull merge: resolve local path failed", "path", capture.change.Path, "error", err)
			h.recordConflict(capture.change.Path, "local delete vs upstream modify", summary)
			return
		}
		localPath = resolved
	}
	if err := h.sessionMgr.GetOverlayDB().UnmarkDeleted(capture.change.Path); err != nil {
		h.logger.Error("pull merge: unmark deleted failed", "path", capture.change.Path, "error", err)
	}
	h.writeMergedContent(capture.change.Path, localPath, theirs)
	h.recordConflict(capture.change.Path, "local delete vs upstream modify", summary)
}

// readBackendContent reads original content from the cluster backend.
// A nil error means the content exists; an error means it is absent or
// unreadable.
func (h *SessionSocketHandler) readBackendContent(ctx context.Context, path string) ([]byte, error) {
	if h.diffReader == nil {
		return nil, fmt.Errorf("diff reader not configured")
	}
	return h.diffReader.ReadOriginal(ctx, path)
}

// dropLocalChange removes a vacuous overlay change so the mount serves
// the fresh upstream content again.
func (h *SessionSocketHandler) dropLocalChange(monofsPath string) {
	db := h.sessionMgr.GetOverlayDB()
	if db == nil {
		return
	}
	if err := db.DeleteFile(monofsPath); err != nil {
		h.logger.Warn("pull merge: drop overlay entry failed", "path", monofsPath, "error", err)
		return
	}
	if localPath, err := h.sessionMgr.GetLocalPath(monofsPath); err == nil {
		if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
			h.logger.Warn("pull merge: remove local file failed", "path", localPath, "error", err)
		}
	}
	if err := h.sessionMgr.DeleteStagedEntry(monofsPath); err != nil {
		h.logger.Warn("pull merge: drop staged entry failed", "path", monofsPath, "error", err)
	}
}

// writeMergedContent writes merged content into the overlay and
// refreshes the tracked change metadata.
func (h *SessionSocketHandler) writeMergedContent(monofsPath, localPath string, content []byte) {
	if err := os.MkdirAll(dirOf(localPath), 0o755); err != nil {
		h.logger.Error("pull merge: mkdir failed", "path", localPath, "error", err)
		return
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(localPath); err == nil {
		mode = info.Mode()
	}
	if err := os.WriteFile(localPath, content, mode); err != nil {
		h.logger.Error("pull merge: write merged content failed", "path", localPath, "error", err)
		return
	}
	if err := h.sessionMgr.TrackChangeWithMeta(ChangeModify, monofsPath, "", int64(len(content))); err != nil {
		h.logger.Warn("pull merge: retrack change failed", "path", monofsPath, "error", err)
	}
}

func (h *SessionSocketHandler) recordConflict(path, reason string, summary *pullMergeSummary) {
	conflict := SessionConflict{
		Path:      path,
		Reason:    reason,
		CreatedAt: time.Now().UTC(),
	}
	summary.Conflicted++
	summary.Conflicts = append(summary.Conflicts, conflict)
	if err := h.sessionMgr.PutSessionConflict(conflict); err != nil {
		h.logger.Error("pull merge: record conflict failed", "path", path, "error", err)
	}
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

// isNotFoundStatus reports whether an error is a gRPC NotFound status.
func isNotFoundStatus(err error) bool {
	if st, ok := status.FromError(err); ok {
		return st.Code() == codes.NotFound
	}
	return false
}

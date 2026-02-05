// Package fuse implements the FUSE filesystem layer for MonoFS.
package fuse

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/radryc/monofs/internal/client"
)

// CommitManager handles pushing local changes to the backend
type CommitManager struct {
	sessionMgr *SessionManager
	client     client.MonoFSClient
	logger     *slog.Logger
	mu         sync.Mutex
}

// NewCommitManager creates a new commit manager
func NewCommitManager(sessionMgr *SessionManager, c client.MonoFSClient, logger *slog.Logger) *CommitManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &CommitManager{
		sessionMgr: sessionMgr,
		client:     c,
		logger:     logger.With("component", "commit"),
	}
}

// CommitResult represents the result of a commit operation
type CommitResult struct {
	Success        bool              // Overall success
	FilesProcessed int               // Number of files processed
	FilesUploaded  int               // Number of files successfully uploaded
	FilesFailed    int               // Number of files that failed
	Errors         map[string]string // Path -> error message
	SessionID      string            // Committed session ID
}

// CommitChanges pushes all local changes to the backend
func (cm *CommitManager) CommitChanges(ctx context.Context) (*CommitResult, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	session := cm.sessionMgr.GetCurrentSession()
	if session == nil {
		return nil, fmt.Errorf("no active session")
	}

	result := &CommitResult{
		Success:   true,
		Errors:    make(map[string]string),
		SessionID: session.ID,
	}

	changes := cm.sessionMgr.GetChanges()
	if len(changes) == 0 {
		cm.logger.Info("no changes to commit")
		// Archive empty session
		if err := cm.sessionMgr.CommitSession(); err != nil {
			return nil, fmt.Errorf("archive session: %w", err)
		}
		return result, nil
	}

	// Group changes by repository
	repoChanges := make(map[string][]Change)
	for _, c := range changes {
		// Extract repo from path: github.com/user/repo/file.txt -> github.com/user/repo
		parts := strings.SplitN(c.Path, "/", 4)
		if len(parts) >= 3 {
			repoPath := strings.Join(parts[:3], "/")
			repoChanges[repoPath] = append(repoChanges[repoPath], c)
		} else {
			// Handle root-level changes
			repoChanges["_root"] = append(repoChanges["_root"], c)
		}
	}

	// Process each repository
	for repoPath, repoChangelist := range repoChanges {
		cm.logger.Info("processing repository changes",
			"repo", repoPath,
			"changes", len(repoChangelist))

		for _, change := range repoChangelist {
			result.FilesProcessed++

			if err := cm.processChange(ctx, change); err != nil {
				cm.logger.Error("failed to process change",
					"path", change.Path,
					"type", change.Type,
					"error", err)
				result.FilesFailed++
				result.Errors[change.Path] = err.Error()
				result.Success = false
			} else {
				result.FilesUploaded++
			}
		}
	}

	// Archive the session regardless of errors
	if err := cm.sessionMgr.CommitSession(); err != nil {
		cm.logger.Warn("failed to archive session", "error", err)
	}

	return result, nil
}

// processChange handles a single file change
func (cm *CommitManager) processChange(ctx context.Context, change Change) error {
	switch change.Type {
	case ChangeCreate, ChangeModify:
		return cm.uploadFile(ctx, change)
	case ChangeDelete:
		return cm.deleteFile(ctx, change)
	case ChangeMkdir:
		// Directories are implicit in Git, no action needed
		cm.logger.Debug("mkdir is implicit in git", "path", change.Path)
		return nil
	case ChangeRmdir:
		// Directories are implicit in Git, handled by file deletions
		cm.logger.Debug("rmdir is implicit in git", "path", change.Path)
		return nil
	default:
		return fmt.Errorf("unknown change type: %s", change.Type)
	}
}

// uploadFile reads the local file and uploads to backend
func (cm *CommitManager) uploadFile(_ context.Context, change Change) error {
	// Read local file content
	content, err := os.ReadFile(change.LocalPath)
	if err != nil {
		return fmt.Errorf("read local file: %w", err)
	}

	info, err := os.Stat(change.LocalPath)
	if err != nil {
		return fmt.Errorf("stat local file: %w", err)
	}

	cm.logger.Info("uploading file",
		"path", change.Path,
		"size", len(content),
		"mode", info.Mode())

	// TODO: Implement actual upload via IngestFile RPC
	// This would require extending the MonoFSClient interface
	// For now, log the intent
	cm.logger.Warn("upload not yet implemented - file would be uploaded",
		"path", change.Path,
		"size", len(content))

	return nil
}

// deleteFile removes a file from the backend
func (cm *CommitManager) deleteFile(_ context.Context, change Change) error {
	cm.logger.Info("deleting file", "path", change.Path)

	// TODO: Implement actual delete via DeleteFile RPC
	// This would require extending the MonoFSClient interface
	cm.logger.Warn("delete not yet implemented - file would be deleted",
		"path", change.Path)

	return nil
}

// DryRun returns what would be committed without actually committing
func (cm *CommitManager) DryRun() (*CommitResult, error) {
	session := cm.sessionMgr.GetCurrentSession()
	if session == nil {
		return nil, fmt.Errorf("no active session")
	}

	changes := cm.sessionMgr.GetChanges()

	result := &CommitResult{
		Success:        true,
		FilesProcessed: len(changes),
		SessionID:      session.ID,
		Errors:         make(map[string]string),
	}

	for _, change := range changes {
		// Check if local file still exists for create/modify
		if change.Type == ChangeCreate || change.Type == ChangeModify {
			if _, err := os.Stat(change.LocalPath); err != nil {
				result.Errors[change.Path] = fmt.Sprintf("file not found: %v", err)
				result.FilesFailed++
			}
		}
	}

	return result, nil
}

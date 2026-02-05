// Package fuse implements the FUSE filesystem layer for MonoFS.
package fuse

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// SessionSocketHandler handles session management requests via Unix socket
type SessionSocketHandler struct {
	socketPath string
	sessionMgr *SessionManager
	commitMgr  *CommitManager
	listener   net.Listener
	logger     *slog.Logger
	wg         sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
}

// SessionRequest is received from CLI
type SessionRequest struct {
	Action string `json:"action"` // start, status, commit, discard
}

// SessionResponse is sent to CLI
type SessionResponse struct {
	Success    bool         `json:"success"`
	SessionID  string       `json:"session_id,omitempty"`
	CreatedAt  string       `json:"created_at,omitempty"`
	Changes    int          `json:"changes,omitempty"`
	Message    string       `json:"message,omitempty"`
	Error      string       `json:"error,omitempty"`
	ChangeList []ChangeInfo `json:"change_list,omitempty"`
}

// ChangeInfo represents a single change for display
type ChangeInfo struct {
	Type      string `json:"type"`
	Path      string `json:"path"`
	Timestamp string `json:"timestamp"`
}

// NewSessionSocketHandler creates a new socket handler
func NewSessionSocketHandler(overlayDir string, sessionMgr *SessionManager, commitMgr *CommitManager, logger *slog.Logger) (*SessionSocketHandler, error) {
	if logger == nil {
		logger = slog.Default()
	}

	socketPath := filepath.Join(overlayDir, "session.sock")

	// Remove existing socket
	os.Remove(socketPath)

	// Create socket
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}

	// Set permissions
	if err := os.Chmod(socketPath, 0600); err != nil {
		listener.Close()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	handler := &SessionSocketHandler{
		socketPath: socketPath,
		sessionMgr: sessionMgr,
		commitMgr:  commitMgr,
		listener:   listener,
		logger:     logger.With("component", "session-socket"),
		ctx:        ctx,
		cancel:     cancel,
	}

	return handler, nil
}

// Start begins accepting connections
func (h *SessionSocketHandler) Start() {
	h.wg.Add(1)
	go h.acceptLoop()
	h.logger.Info("session socket started", "path", h.socketPath)
}

// Stop closes the socket and stops accepting connections
func (h *SessionSocketHandler) Stop() {
	h.cancel()
	h.listener.Close()
	h.wg.Wait()
	os.Remove(h.socketPath)
	h.logger.Info("session socket stopped")
}

func (h *SessionSocketHandler) acceptLoop() {
	defer h.wg.Done()

	for {
		conn, err := h.listener.Accept()
		if err != nil {
			select {
			case <-h.ctx.Done():
				return
			default:
				h.logger.Warn("accept error", "error", err)
				continue
			}
		}

		h.wg.Add(1)
		go h.handleConnection(conn)
	}
}

func (h *SessionSocketHandler) handleConnection(conn net.Conn) {
	defer h.wg.Done()
	defer conn.Close()

	// Read request
	var req SessionRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		h.logger.Warn("failed to decode request", "error", err)
		h.sendError(conn, "invalid request")
		return
	}

	h.logger.Debug("session request", "action", req.Action)

	// Handle action
	var resp SessionResponse
	switch req.Action {
	case "start":
		resp = h.handleStart()
	case "status":
		resp = h.handleStatus()
	case "commit":
		resp = h.handleCommit()
	case "discard":
		resp = h.handleDiscard()
	default:
		resp = SessionResponse{
			Success: false,
			Error:   "unknown action: " + req.Action,
		}
	}

	// Send response
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		h.logger.Warn("failed to encode response", "error", err)
	}
}

func (h *SessionSocketHandler) handleStart() SessionResponse {
	session, err := h.sessionMgr.StartSession()
	if err != nil {
		return SessionResponse{
			Success: false,
			Error:   err.Error(),
		}
	}

	session.mu.RLock()
	changeCount := len(session.Changes)
	session.mu.RUnlock()

	return SessionResponse{
		Success:   true,
		SessionID: session.ID,
		CreatedAt: session.CreatedAt.Format("2006-01-02 15:04:05"),
		Changes:   changeCount,
	}
}

func (h *SessionSocketHandler) handleStatus() SessionResponse {
	id, createdAt, changeCount, ok := h.sessionMgr.GetSessionInfo()
	if !ok {
		return SessionResponse{
			Success: false,
			Error:   "no active session",
		}
	}

	// Get change list
	changes := h.sessionMgr.GetChanges()
	changeList := make([]ChangeInfo, len(changes))
	for i, c := range changes {
		changeList[i] = ChangeInfo{
			Type:      string(c.Type),
			Path:      c.Path,
			Timestamp: c.Timestamp.Format("15:04:05"),
		}
	}

	return SessionResponse{
		Success:    true,
		SessionID:  id,
		CreatedAt:  createdAt.Format("2006-01-02 15:04:05"),
		Changes:    changeCount,
		ChangeList: changeList,
	}
}

func (h *SessionSocketHandler) handleCommit() SessionResponse {
	if h.commitMgr == nil {
		return SessionResponse{
			Success: false,
			Error:   "commit manager not available",
		}
	}

	result, err := h.commitMgr.CommitChanges(h.ctx)
	if err != nil {
		return SessionResponse{
			Success: false,
			Error:   err.Error(),
		}
	}

	message := "No changes to commit"
	if result.FilesProcessed > 0 {
		message = formatCommitMessage(result)
	}

	return SessionResponse{
		Success:   result.Success,
		SessionID: result.SessionID,
		Message:   message,
	}
}

func (h *SessionSocketHandler) handleDiscard() SessionResponse {
	err := h.sessionMgr.DiscardSession()
	if err != nil {
		return SessionResponse{
			Success: false,
			Error:   err.Error(),
		}
	}

	return SessionResponse{
		Success: true,
		Message: "Session discarded successfully",
	}
}

func (h *SessionSocketHandler) sendError(conn net.Conn, msg string) {
	resp := SessionResponse{
		Success: false,
		Error:   msg,
	}
	json.NewEncoder(conn).Encode(resp)
}

func formatCommitMessage(result *CommitResult) string {
	if result.FilesFailed > 0 {
		return fmt.Sprintf("Processed %d files: %d uploaded, %d failed",
			result.FilesProcessed, result.FilesUploaded, result.FilesFailed)
	}
	return fmt.Sprintf("Successfully processed %d files", result.FilesProcessed)
}

package fuse

import (
	"context"
	"fmt"
)

func (h *SessionSocketHandler) fsContext() context.Context {
	if h.ctx != nil {
		return h.ctx
	}
	return context.Background()
}

func (h *SessionSocketHandler) fsUnavailable() SessionResponse {
	return SessionResponse{
		Success: false,
		Error:   "filesystem API is not available (start monofs-client with --writable)",
	}
}

func (h *SessionSocketHandler) handleFSList(path string) SessionResponse {
	if h.fs == nil {
		return h.fsUnavailable()
	}
	entries, err := h.fs.List(h.fsContext(), path)
	if err != nil {
		return SessionResponse{Success: false, Error: err.Error()}
	}
	return SessionResponse{
		Success: true,
		Entries: entries,
		Message: fmt.Sprintf("%d entries", len(entries)),
	}
}

func (h *SessionSocketHandler) handleFSRead(path string, offset, size int64) SessionResponse {
	if h.fs == nil {
		return h.fsUnavailable()
	}
	content, err := h.fs.Read(h.fsContext(), path, offset, size)
	if err != nil {
		return SessionResponse{Success: false, Error: err.Error()}
	}
	return SessionResponse{
		Success: true,
		Content: content,
		Message: fmt.Sprintf("read %d bytes", len(content)),
	}
}

func (h *SessionSocketHandler) handleFSStat(path string) SessionResponse {
	if h.fs == nil {
		return h.fsUnavailable()
	}
	stat, err := h.fs.Stat(h.fsContext(), path)
	if err != nil {
		return SessionResponse{Success: false, Error: err.Error()}
	}
	return SessionResponse{
		Success: true,
		Stat:    stat,
	}
}

func (h *SessionSocketHandler) handleFSWrite(path string, content []byte, mode uint32, createParents bool) SessionResponse {
	if h.fs == nil {
		return h.fsUnavailable()
	}
	stat, err := h.fs.WriteWithParents(h.fsContext(), path, content, mode, createParents)
	if err != nil {
		return SessionResponse{Success: false, Error: err.Error()}
	}
	return SessionResponse{
		Success: true,
		Stat:    stat,
		Message: fmt.Sprintf("wrote %d bytes to %s", len(content), stat.Path),
	}
}

func (h *SessionSocketHandler) handleFSMkdir(path string, mode uint32) SessionResponse {
	if h.fs == nil {
		return h.fsUnavailable()
	}
	stat, err := h.fs.Mkdir(h.fsContext(), path, mode)
	if err != nil {
		return SessionResponse{Success: false, Error: err.Error()}
	}
	return SessionResponse{
		Success: true,
		Stat:    stat,
		Message: fmt.Sprintf("created directory %s", stat.Path),
	}
}

package router

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandleAutoRefreshWorkerDisabled(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	rec := httptest.NewRecorder()
	r.handleAutoRefreshWorker(rec, httptest.NewRequest(http.MethodGet, "/api/workers/auto-refresh", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["enabled"] != false {
		t.Fatalf("enabled = %v, want false", got["enabled"])
	}
}

func TestHandleAutoRefreshWorkerEnabled(t *testing.T) {
	cfg := DefaultRouterConfig()
	cfg.RouterName = "router-a"
	cfg.AutoRefreshOwner = "router-a"
	r := NewRouter(cfg, slog.New(slog.DiscardHandler))
	r.autoRefreshWorker = newAutoRefreshWorker(r, time.Minute, 2, slog.New(slog.DiscardHandler))
	r.mu.Lock()
	r.ingestedRepos["repo-1"] = &ingestedRepo{repoID: "src/repo", repoURL: "https://example.com/repo.git", branch: "main"}
	r.mu.Unlock()

	rec := httptest.NewRecorder()
	r.handleAutoRefreshWorker(rec, httptest.NewRequest(http.MethodGet, "/api/workers/auto-refresh", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["enabled"] != true {
		t.Fatalf("enabled = %v, want true", got["enabled"])
	}
	if got["owner"] != "router-a" || got["router"] != "router-a" {
		t.Fatalf("owner/router = %v/%v", got["owner"], got["router"])
	}
}

func TestHandleRepositoryRefreshValidation(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))

	rec := httptest.NewRecorder()
	r.handleRepositoryRefresh(rec, httptest.NewRequest(http.MethodPost, "/api/repositories/refresh", strings.NewReader("")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing storage_id status = %d, want 400", rec.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/repositories/refresh", strings.NewReader("storage_id=repo-1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	r.handleRepositoryRefresh(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled worker status = %d, want 503", rec.Code)
	}
}

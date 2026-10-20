package router

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleAutoRefreshWorker serves the repository watcher status used by the
// MonoFS worker UI.
func (r *Router) handleAutoRefreshWorker(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	worker := r.autoRefreshWorker
	if worker == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enabled":     false,
			"interval":    "",
			"concurrency": 0,
			"owner":       strings.TrimSpace(r.config.AutoRefreshOwner),
			"router":      r.config.RouterName,
			"tracked":     0,
			"active":      0,
			"last_scan":   0,
			"next_scan":   0,
			"probes":      map[string]int64{},
			"reingests":   0,
			"backoffs":    0,
		})
		return
	}
	stats := worker.Stats()
	stats["owner"] = strings.TrimSpace(r.config.AutoRefreshOwner)
	stats["router"] = r.config.RouterName
	_ = json.NewEncoder(w).Encode(stats)
}

// handleRepositoryRefresh queues an immediate auto-refresh probe for one
// repository. It is used by the repository page's "Check now" action.
func (r *Router) handleRepositoryRefresh(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := req.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}
	storageID := strings.TrimSpace(req.FormValue("storage_id"))
	if storageID == "" {
		http.Error(w, "storage_id is required", http.StatusBadRequest)
		return
	}
	worker := r.autoRefreshWorker
	if worker == nil {
		http.Error(w, "auto-refresh is disabled on this router", http.StatusServiceUnavailable)
		return
	}

	r.mu.RLock()
	_, exists := r.ingestedRepos[storageID]
	r.mu.RUnlock()
	if !exists {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}

	queued := worker.probeStorageID(storageID)
	status := "queued"
	if !queued {
		status = "already_running"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     status,
		"storage_id": storageID,
	})
}

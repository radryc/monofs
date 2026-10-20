package router

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// handleAutoRefreshWorker serves the repository watcher status used by the
// MonoFS worker UI. When the router runs one worker per shard, the response is
// aggregated across all shards.
func (r *Router) handleAutoRefreshWorker(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	workers := r.autoRefreshWorkers
	if len(workers) == 0 && r.autoRefreshWorker != nil {
		workers = []*autoRefreshWorker{r.autoRefreshWorker}
	}
	if len(workers) == 0 {
		empty := map[string]any{
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
		}
		empty = r.proxyAutoRefreshStats(empty)
		_ = json.NewEncoder(w).Encode(empty)
		return
	}
	if len(workers) == 1 {
		stats := workers[0].Stats()
		stats["owner"] = strings.TrimSpace(r.config.AutoRefreshOwner)
		stats["router"] = r.config.RouterName
		stats = r.proxyAutoRefreshStats(stats)
		_ = json.NewEncoder(w).Encode(stats)
		return
	}

	aggregate := map[string]any{
		"enabled":         true,
		"owner":           strings.TrimSpace(r.config.AutoRefreshOwner),
		"router":          r.config.RouterName,
		"shard":           -1,
		"probes":          map[string]int64{},
		"reingests":       int64(0),
		"backoffs":        int64(0),
		"tracked":         0,
		"active":          0,
		"scheduled":       0,
		"last_scan":       int64(0),
		"next_scan":       int64(0),
		"lease_held":      false,
		"lease_bypass":    false,
		"trace_limit":     autoRefreshTraceLimit,
		"trace_truncated": false,
		"repositories":    []map[string]any{},
	}
	probes := map[string]int64{}
	repos := make([]map[string]any, 0)
	for _, worker := range workers {
		stats := worker.Stats()
		if aggregate["interval"] == nil {
			aggregate["interval"] = stats["interval"]
			aggregate["max_interval"] = stats["max_interval"]
			aggregate["concurrency"] = stats["concurrency"]
			aggregate["batch_size"] = stats["batch_size"]
			aggregate["shards"] = stats["shards"]
		}
		aggregate["tracked"] = aggregate["tracked"].(int) + intValue(stats["tracked"])
		aggregate["active"] = aggregate["active"].(int) + intValue(stats["active"])
		aggregate["scheduled"] = aggregate["scheduled"].(int) + intValue(stats["scheduled"])
		aggregate["reingests"] = aggregate["reingests"].(int64) + int64Value(stats["reingests"])
		aggregate["backoffs"] = aggregate["backoffs"].(int64) + int64Value(stats["backoffs"])
		if held, _ := stats["lease_held"].(bool); held {
			aggregate["lease_held"] = true
		}
		if bypass, _ := stats["lease_bypass"].(bool); bypass {
			aggregate["lease_bypass"] = true
		}
		if stats["last_scan"] != nil {
			if aggregate["last_scan"] == nil || int64Value(stats["last_scan"]) > int64Value(aggregate["last_scan"]) {
				aggregate["last_scan"] = stats["last_scan"]
			}
		}
		if stats["next_scan"] != nil {
			current := int64Value(aggregate["next_scan"])
			next := int64Value(stats["next_scan"])
			if next > 0 && (current == 0 || next < current) {
				aggregate["next_scan"] = next
			}
		}
		if values, ok := stats["probes"].(map[string]int64); ok {
			for key, value := range values {
				probes[key] += value
			}
		}
		if values, ok := stats["repositories"].([]map[string]any); ok {
			repos = append(repos, values...)
		}
		if truncated, _ := stats["trace_truncated"].(bool); truncated {
			aggregate["trace_truncated"] = true
		}
	}
	if len(repos) > autoRefreshTraceLimit {
		repos = repos[:autoRefreshTraceLimit]
		aggregate["trace_truncated"] = true
	}
	aggregate["probes"] = probes
	aggregate["repositories"] = repos
	aggregate = r.proxyAutoRefreshStats(aggregate)
	_ = json.NewEncoder(w).Encode(aggregate)
}

// proxyAutoRefreshStats returns the local watcher stats when this router owns
// a shard. A standby router proxies the active peer's stats so the UI does not
// show "not checked" just because the request landed on the standby.
func (r *Router) proxyAutoRefreshStats(local map[string]any) map[string]any {
	if held, _ := local["lease_held"].(bool); held {
		return local
	}
	if len(r.config.PeerRouters) == 0 {
		return local
	}
	r.mu.RLock()
	token := strings.TrimSpace(r.peerRouterToken)
	r.mu.RUnlock()
	client := &http.Client{Timeout: 2 * time.Second}
	for _, peer := range r.config.PeerRouters {
		baseURL, err := normalizeRouterURL(peer.URL)
		if err != nil {
			continue
		}
		req, err := http.NewRequest(http.MethodGet, baseURL+"/api/workers/auto-refresh", nil)
		if err != nil {
			continue
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var stats map[string]any
		decodeErr := json.NewDecoder(resp.Body).Decode(&stats)
		resp.Body.Close()
		if decodeErr != nil {
			continue
		}
		held, _ := stats["lease_held"].(bool)
		scheduled, _ := stats["scheduled"].(float64)
		probes, _ := stats["probes"].(map[string]any)
		if held || scheduled > 0 || len(probes) > 0 {
			stats["router"] = peer.Name
			stats["proxied_from"] = peer.Name
			return stats
		}
	}
	return local
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func int64Value(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	default:
		return 0
	}
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
	if worker == nil && len(r.autoRefreshWorkers) > 0 {
		worker = r.autoRefreshWorkers[0]
	}
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

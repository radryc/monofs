package router

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func (r *Router) handleAuthzIngestToggleAPI(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Enforce bool `json:"enforce"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "invalid request body",
		})
		return
	}

	r.SetAuthzEnforceIngest(body.Enforce)

	state := "disabled"
	if body.Enforce {
		state = "enabled"
	}
	r.logger.Info("authz ingest enforcement toggled", "enforce", body.Enforce)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("partition ingest authorization %s", state),
	})
}

func (r *Router) handleAuthzIngestStatusAPI(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"enforce": r.AuthzEnforceIngest(),
	})
}

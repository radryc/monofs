package router

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/radryc/monofs/pkg/authz"
)

func (r *Router) handleAccessEnforceToggleAPI(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, _ := authz.IdentityFromContext(req.Context())
	if !r.accessAdmin(id) {
		http.Error(w, "Forbidden", http.StatusForbidden)
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

	r.SetAccessEnforce(body.Enforce)

	state := "observe"
	if body.Enforce {
		state = "enforce"
	}
	r.logger.Info("access policy enforcement toggled", "enforce", body.Enforce)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("access policy %s mode", state),
	})
}

func (r *Router) handleAccessEnforceStatusAPI(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"configured": r.accessPolicySnapshot() != nil,
		"enforce":    r.AccessEnforce(),
	})
}

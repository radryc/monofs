package router

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/radryc/monofs/internal/router/mergerequest"
	"github.com/radryc/monofs/pkg/authz"
)

var errAnonymousIdentity = errors.New("authentication required")

// handleMergeRequestsAPI serves the native merge-request (proposal) endpoints
// for guardian-managed partitions. It mirrors the workspace-sync API routing
// pattern: the bare path handles list/create, and the /{id}/{action} path
// handles approve/merge/reject.
func (r *Router) handleMergeRequestsAPI(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	store := r.mergeRequestStore()
	if store == nil {
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "ownership gate not configured"})
		return
	}

	path := strings.TrimPrefix(req.URL.Path, "/api/merge-requests")
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		r.handleMergeRequestsCollection(w, req, store)
		return
	}

	parts := strings.Split(path, "/")
	id := parts[0]
	if len(parts) == 1 {
		if req.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "method not allowed"})
			return
		}
		p, ok := store.Get(id)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "proposal not found"})
			return
		}
		_ = json.NewEncoder(w).Encode(p)
		return
	}

	if len(parts) != 2 {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "not found"})
		return
	}
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "method not allowed"})
		return
	}

	idn, err := r.httpIdentity(req)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}

	var (
		out *mergerequest.Proposal
	)
	switch parts[1] {
	case "approve":
		out, err = store.Approve(req.Context(), id, idn)
	case "merge":
		out, err = store.Merge(req.Context(), id, idn)
	case "reject":
		out, err = store.Reject(req.Context(), id, idn)
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "unknown action"})
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (r *Router) handleMergeRequestsCollection(w http.ResponseWriter, req *http.Request, store *mergerequest.Store) {
	switch req.Method {
	case http.MethodGet:
		partition := req.URL.Query().Get("partition")
		status := req.URL.Query().Get("status")
		all := store.List(partition)
		out := make([]*mergerequest.Proposal, 0, len(all))
		for _, p := range all {
			if status != "" && string(p.State) != status {
				continue
			}
			out = append(out, p)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"proposals": out})
	case http.MethodPost:
		var body struct {
			Partition   string   `json:"partition"`
			Workspace   string   `json:"workspace"`
			Paths       []string `json:"paths"`
			Title       string   `json:"title"`
			Description string   `json:"description"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid request body"})
			return
		}
		idn, err := r.httpIdentity(req)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		partition := body.Partition
		if partition == "" {
			partition = body.Workspace
		}
		p, err := store.Create(idn.PrincipalID(), partition, body.Paths, body.Title, body.Description)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(p)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "method not allowed"})
	}
}

// httpIdentity extracts the authenticated identity from an HTTP request
// context, mirroring the gRPC identity interceptor for the HTTP surface.
func (r *Router) httpIdentity(req *http.Request) (authz.Identity, error) {
	id, ok := authz.IdentityFromContext(req.Context())
	if !ok || id.IsAnonymous() {
		return authz.Identity{}, errAnonymousIdentity
	}
	return id, nil
}

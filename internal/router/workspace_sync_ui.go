package router

import (
	"encoding/json"
	"net/http"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/pkg/authz"
)

func (r *Router) handleWorkspaceSyncJobsAPI(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(req.URL.Path, "/api/workspace-sync/jobs")
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		if req.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "method not allowed"})
			return
		}
		resp, err := r.ListWorkspaceSyncJobs(req.Context(), &pb.ListWorkspaceSyncJobsRequest{Limit: 100})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		resp = r.filterWorkspaceJobs(req, resp)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	parts := strings.Split(path, "/")
	jobID := parts[0]
	if len(parts) == 1 {
		if req.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "method not allowed"})
			return
		}
		job, err := r.GetWorkspaceSyncJob(req.Context(), &pb.GetWorkspaceSyncJobRequest{JobId: jobID})
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		if r.accessPolicySnapshot() != nil {
			id, _ := authz.IdentityFromContext(req.Context())
			paths := make([]string, 0, len(job.Repositories))
			for _, repo := range job.Repositories {
				paths = append(paths, repo.DisplayPath)
			}
			if !r.canReadWorkspaceJob(id, paths) {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "job not found"})
				return
			}
		}
		_ = json.NewEncoder(w).Encode(job)
		return
	}

	if len(parts) == 2 && parts[1] == "cancel" {
		if req.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "method not allowed"})
			return
		}
		resp, err := r.CancelWorkspaceSyncJob(req.Context(), &pb.CancelWorkspaceSyncJobRequest{JobId: jobID})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": "not found"})
}

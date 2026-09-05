package router

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"github.com/radryc/monofs/internal/bazel"
	"github.com/radryc/monofs/internal/bazel/migration"
)

// handleBazelStatusAPI serves GET /api/bazel/status returning the
// Bazel adoption status for all repos in the workspace.
func (r *Router) handleBazelStatusAPI(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// Read workspace manifest from the default mount point.
	manifestPath := "/mnt/monofs/.monofs/workspace.json"
	// Also try the router's workspace state directory if configured.
	if r.config.WorkspaceStateDir != "" {
		candidate := filepath.Join(r.config.WorkspaceStateDir, "workspace.json")
		if _, err := os.Stat(candidate); err == nil {
			manifestPath = candidate
		}
	}

	manifest, err := bazel.LoadManifest(manifestPath)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":             true,
			"message":        "workspace not mounted or no manifest available",
			"total_repos":    0,
			"active_count":   0,
			"hermetic_count": 0,
			"adoption_pct":   0.0,
			"repos":          []interface{}{},
		})
		return
	}

	type repoInfo struct {
		DisplayPath string `json:"display_path"`
		State       string `json:"state"`
		BuildSystem string `json:"build_system"`
		Source      string `json:"source,omitempty"`
		CommitHash  string `json:"commit_hash,omitempty"`
	}
	var repos []repoInfo
	activeCount := 0
	hermeticCount := 0

	for _, repo := range manifest.IncludedRepos() {
		repoDir := filepath.Join("/mnt/monofs", repo.DisplayPath)
		cfg, err := migration.LoadRepoConfig(repoDir)
		state := "native"
		buildSystem := "unknown"
		if err == nil {
			state = string(cfg.State)
			buildSystem = migration.BuildSystemLabel(repoDir)
		} else if fi, serr := os.Stat(repoDir); serr == nil && fi.IsDir() {
			buildSystem = migration.BuildSystemLabel(repoDir)
		}

		if state == "active" {
			activeCount++
		}
		if state == "hermetic" {
			hermeticCount++
		}

		repos = append(repos, repoInfo{
			DisplayPath: repo.DisplayPath,
			State:       state,
			BuildSystem: buildSystem,
			Source:      repo.Source,
			CommitHash:  repo.CommitHash,
		})
	}

	totalRepos := len(repos)
	adoptionPct := 0.0
	if totalRepos > 0 {
		adoptionPct = float64(activeCount+hermeticCount) / float64(totalRepos) * 100
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":             true,
		"total_repos":    totalRepos,
		"active_count":   activeCount,
		"hermetic_count": hermeticCount,
		"adoption_pct":   adoptionPct,
		"repos":          repos,
	})
}

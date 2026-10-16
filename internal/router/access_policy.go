package router

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
)

// accessPolicyState holds the router's compiled access policy. It is nil when
// the feature is disabled, in which case every access helper returns allow and
// behavior is identical to a cluster without access policies.
type accessPolicyState struct {
	loader     *policy.Loader
	enforce    bool
	serveToken string

	mu       sync.Mutex
	loggedAt map[string]time.Time
}

// StartAccessPolicy starts the policy loader when configured. Safe to call
// unconditionally; it is a no-op when neither --access-policy-url nor
// --access-policy-file is set.
func (r *Router) StartAccessPolicy(ctx context.Context) {
	r.mu.RLock()
	cfg := r.config
	r.mu.RUnlock()

	url := strings.TrimSpace(cfg.AccessPolicyURL)
	file := strings.TrimSpace(cfg.AccessPolicyFile)
	if url == "" && file == "" {
		return
	}
	var source policy.Source
	if url != "" {
		source = policy.HTTPSource{URL: url, Token: strings.TrimSpace(cfg.AccessPolicyToken)}
	} else {
		source = policy.FileSource{Path: file}
	}
	logf := func(format string, args ...any) {
		r.logger.Info(fmt.Sprintf(format, args...))
	}
	state := &accessPolicyState{
		loader:     policy.NewLoader(source, cfg.AccessPolicyInterval, logf),
		enforce:    cfg.AccessEnforce,
		serveToken: strings.TrimSpace(cfg.AccessPolicyServeToken),
		loggedAt:   map[string]time.Time{},
	}
	r.accessPolicyMu.Lock()
	r.accessPolicy = state
	r.accessPolicyMu.Unlock()

	go state.loader.Run(ctx)
	r.logger.Info("access policy enabled",
		"source", source.Describe(),
		"enforce", cfg.AccessEnforce,
		"serve_token", state.serveToken != "")
}

func (r *Router) accessPolicySnapshot() *accessPolicyState {
	r.accessPolicyMu.RLock()
	defer r.accessPolicyMu.RUnlock()
	return r.accessPolicy
}

func (r *Router) policyInstance() *policy.Policy {
	st := r.accessPolicySnapshot()
	if st == nil {
		return nil
	}
	return st.loader.Get()
}

// accessAdmin reports whether the identity bypasses policy filtering.
func (r *Router) accessAdmin(id authz.Identity) bool {
	if id.Subject == "break-glass-admin" {
		return true
	}
	p := r.policyInstance()
	return p != nil && p.IsAdmin(id)
}

// canReadDisplayPath decides whether the identity may read a repository
// display path (and therefore its files in the UI/API).
func (r *Router) canReadDisplayPath(id authz.Identity, displayPath string) bool {
	st := r.accessPolicySnapshot()
	if st == nil {
		return true
	}
	if r.accessAdmin(id) {
		return true
	}
	allowed := false
	if p := st.loader.Get(); p != nil {
		allowed = p.CanReadPath(id, displayPath)
	}
	if !allowed {
		r.logAccessDenied(st, id, "monofs", displayPath)
	}
	if st.enforce {
		return allowed
	}
	return true
}

// canSeePartition decides whether the identity may see a Guardian partition.
func (r *Router) canSeePartition(id authz.Identity, partition string) bool {
	st := r.accessPolicySnapshot()
	if st == nil {
		return true
	}
	if r.accessAdmin(id) {
		return true
	}
	allowed := false
	if p := st.loader.Get(); p != nil {
		allowed = p.CanSeePartition(id, partition)
	}
	if !allowed {
		r.logAccessDenied(st, id, "partitions", partition)
	}
	if st.enforce {
		return allowed
	}
	return true
}

// canReadGuardianRepo routes guardian namespace display paths through the
// partition scope, keeping guardian-system admin-only.
func (r *Router) canReadGuardianRepo(id authz.Identity, displayPath string) bool {
	if displayPath == "guardian-system" {
		st := r.accessPolicySnapshot()
		if st == nil {
			return true
		}
		return r.accessAdmin(id)
	}
	if partition, ok := strings.CutPrefix(displayPath, "guardian/"); ok {
		return r.canSeePartition(id, partition)
	}
	return r.canReadDisplayPath(id, displayPath)
}

func (r *Router) logAccessDenied(st *accessPolicyState, id authz.Identity, scope, target string) {
	key := scope + "|" + target + "|" + id.PrincipalID()
	st.mu.Lock()
	now := time.Now()
	if last, ok := st.loggedAt[key]; ok && now.Sub(last) < time.Minute {
		st.mu.Unlock()
		return
	}
	st.loggedAt[key] = now
	st.mu.Unlock()
	mode := "observe"
	if st.enforce {
		mode = "enforce"
	}
	r.logger.Info("access policy decision",
		"mode", mode,
		"principal", id.PrincipalID(),
		"scope", scope,
		"target", target,
		"allowed", false)
}

// handleAccessPolicy serves the compiled access policy bundle to internal
// consumers (storage nodes, search service, Doctor).
func (r *Router) handleAccessPolicy(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	st := r.accessPolicySnapshot()
	if st == nil {
		http.Error(w, "access policy is not enabled on this router", http.StatusNotFound)
		return
	}
	if st.serveToken != "" {
		token := strings.TrimSpace(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		if alt := strings.TrimSpace(req.Header.Get("X-Monofs-Access-Token")); alt != "" {
			token = alt
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(st.serveToken)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	p := st.loader.Get()
	if p == nil {
		http.Error(w, "access policy not loaded yet", http.StatusServiceUnavailable)
		return
	}
	version := p.Version()
	w.Header().Set("ETag", strconv.Quote(version))
	w.Header().Set("X-Policy-Version", version)
	w.Header().Set("Cache-Control", "no-store")
	if match := req.Header.Get("If-None-Match"); match != "" && strings.Contains(match, version) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	data, err := policy.MarshalPolicy(p)
	if err != nil {
		http.Error(w, "failed to marshal access policy", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// filterRepositoriesData returns a copy of the repository list containing only
// repositories the caller may read. The cached payload is never mutated.
func (r *Router) filterRepositoriesData(req *http.Request, data *RepositoriesData) *RepositoriesData {
	if data == nil || r.accessPolicySnapshot() == nil {
		return data
	}
	id, _ := authz.IdentityFromContext(req.Context())
	out := &RepositoriesData{
		CurrentTopologyVersion: data.CurrentTopologyVersion,
		Repositories:           make([]map[string]interface{}, 0, len(data.Repositories)),
	}
	for _, repo := range data.Repositories {
		repoID, _ := repo["repo_id"].(string)
		if repoID == "" {
			out.Repositories = append(out.Repositories, repo)
			continue
		}
		var visible bool
		if strings.HasPrefix(repoID, "guardian/") || repoID == "guardian-system" {
			visible = r.canReadGuardianRepo(id, repoID)
		} else {
			visible = r.canReadDisplayPath(id, repoID)
		}
		if visible {
			out.Repositories = append(out.Repositories, repo)
		}
	}
	return out
}

// canReadWorkspaceJob reports whether the identity may see a workspace sync
// job, based on the display paths of the repositories it touched.
func (r *Router) canReadWorkspaceJob(id authz.Identity, displayPaths []string) bool {
	if r.accessPolicySnapshot() == nil {
		return true
	}
	if len(displayPaths) == 0 {
		return r.accessAdmin(id)
	}
	for _, displayPath := range displayPaths {
		if strings.HasPrefix(displayPath, "guardian/") || displayPath == "guardian-system" {
			if r.canReadGuardianRepo(id, displayPath) {
				return true
			}
			continue
		}
		if r.canReadDisplayPath(id, displayPath) {
			return true
		}
	}
	return false
}

// filterWorkspaceJobs returns a copy of the job list containing only jobs the
// caller may see.
func (r *Router) filterWorkspaceJobs(req *http.Request, resp *pb.ListWorkspaceSyncJobsResponse) *pb.ListWorkspaceSyncJobsResponse {
	if resp == nil || r.accessPolicySnapshot() == nil {
		return resp
	}
	id, _ := authz.IdentityFromContext(req.Context())
	if r.accessAdmin(id) {
		return resp
	}
	out := &pb.ListWorkspaceSyncJobsResponse{
		Jobs: make([]*pb.WorkspaceSyncJob, 0, len(resp.Jobs)),
	}
	for _, job := range resp.Jobs {
		paths := make([]string, 0, len(job.Repositories))
		for _, repo := range job.Repositories {
			paths = append(paths, repo.DisplayPath)
		}
		if r.canReadWorkspaceJob(id, paths) {
			out.Jobs = append(out.Jobs, job)
		}
	}
	return out
}

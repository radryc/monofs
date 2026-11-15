package router

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/radryc/monofs/pkg/authz"
)

func newUITestRouter() *Router {
	return &Router{
		config:        DefaultRouterConfig(),
		logger:        slog.New(slog.DiscardHandler),
		whitelist:     newWhitelistStore(),
		nodes:         map[string]*nodeState{},
		ingestedRepos: map[string]*ingestedRepo{},
	}
}

func TestAPIRoutesTableIsWellFormed(t *testing.T) {
	r := newUITestRouter()
	routes := r.apiRoutes()

	required := []string{
		"/api/status", "/api/repositories", "/api/fetchers", "/api/whitelist",
		"/api/access/enforce", "/api/access/policy", "/api/bazel/status",
		"/api/pipelines/help", "/api/file/content", "/api/guardian/partitions",
		"/api/search", "/health",
	}
	for _, pattern := range required {
		if _, ok := routes[pattern]; !ok {
			t.Errorf("required route %q is missing", pattern)
		}
	}
	for pattern, handler := range routes {
		if !strings.HasPrefix(pattern, "/") {
			t.Errorf("route %q is not rooted", pattern)
		}
		if handler == nil {
			t.Errorf("route %q has a nil handler", pattern)
		}
	}
	if len(routes) < 40 {
		t.Fatalf("route table has %d entries; expected the full API surface", len(routes))
	}
}

func doRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestHealthAndPipelineHelpRoutes(t *testing.T) {
	handler := newUITestRouter().ServeHTTP()

	rec := doRequest(t, handler, http.MethodGet, "/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/health = %d", rec.Code)
	}
	var health map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("/health body: %v", err)
	}
	if health["status"] != "healthy" {
		t.Fatalf("/health = %+v", health)
	}

	rec = doRequest(t, handler, http.MethodGet, "/api/pipelines/help", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/pipelines/help = %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/markdown") {
		t.Fatalf("content type = %q", rec.Header().Get("Content-Type"))
	}
	if rec.Body.Len() == 0 {
		t.Fatal("pipeline help body is empty")
	}
}

func TestWhitelistRoutes(t *testing.T) {
	handler := newUITestRouter().ServeHTTP()

	rec := doRequest(t, handler, http.MethodGet, "/api/whitelist", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/whitelist = %d", rec.Code)
	}

	rec = doRequest(t, handler, http.MethodPost, "/api/whitelist", "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST invalid whitelist body = %d", rec.Code)
	}

	rec = doRequest(t, handler, http.MethodPost, "/api/whitelist", `{"client_id":"c1","label":"test"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/whitelist = %d", rec.Code)
	}

	rec = doRequest(t, handler, http.MethodDelete, "/api/whitelist", `{"client_id":"c1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/whitelist = %d", rec.Code)
	}

	rec = doRequest(t, handler, http.MethodPut, "/api/whitelist", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/whitelist = %d", rec.Code)
	}
}

func TestAccessEnforceToggleRoutes(t *testing.T) {
	r := newUITestRouter()
	r.accessPolicy = newTestAccessPolicy(t, false)
	handler := r.ServeHTTP()

	rec := doRequest(t, handler, http.MethodGet, "/api/access/enforce", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/access/enforce = %d", rec.Code)
	}
	var status map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["enforce"] != false {
		t.Fatalf("initial enforce = %v", status["enforce"])
	}

	req := httptest.NewRequest(http.MethodPost, "/api/access/enforce/toggle", strings.NewReader(`{"enforce":true}`))
	req = req.WithContext(authz.ContextWithIdentity(req.Context(), authz.Identity{Subject: "admin@example.com", Email: "admin@example.com"}))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST toggle = %d", rec.Code)
	}
	rec = doRequest(t, handler, http.MethodGet, "/api/access/enforce", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["enforce"] != true {
		t.Fatalf("enforce after toggle = %v", status["enforce"])
	}

	badReq := httptest.NewRequest(http.MethodPost, "/api/access/enforce/toggle", strings.NewReader("{bad"))
	badReq = badReq.WithContext(authz.ContextWithIdentity(badReq.Context(), authz.Identity{Subject: "admin@example.com", Email: "admin@example.com"}))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, badReq)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid toggle body = %d", rec.Code)
	}
	rec = doRequest(t, handler, http.MethodGet, "/api/access/enforce/toggle", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET toggle = %d", rec.Code)
	}
}

func TestAccessPolicyAndBazelRoutes(t *testing.T) {
	handler := newUITestRouter().ServeHTTP()

	rec := doRequest(t, handler, http.MethodGet, "/api/access/policy", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled access policy = %d, want 404", rec.Code)
	}
	rec = doRequest(t, handler, http.MethodPost, "/api/access/policy", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST access policy = %d", rec.Code)
	}

	rec = doRequest(t, handler, http.MethodPost, "/api/bazel/status", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST bazel status = %d", rec.Code)
	}
	rec = doRequest(t, handler, http.MethodGet, "/api/bazel/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET bazel status = %d", rec.Code)
	}
	var bazel map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &bazel); err != nil {
		t.Fatalf("bazel body: %v", err)
	}
	if bazel["ok"] != true {
		t.Fatalf("bazel body = %+v", bazel)
	}
}

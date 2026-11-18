package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/radryc/monofs/internal/diagnostics"
)

func serve(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	res := httptest.NewRecorder()
	newMetricsHandler().ServeHTTP(res, req)
	return res
}

func TestNewMetricsHandlerServesMetricsByDefault(t *testing.T) {
	t.Setenv(diagnostics.PprofEnv, "")
	if res := serve(t, "/metrics"); res.Code != http.StatusOK || res.Body.Len() == 0 {
		t.Fatalf("GET /metrics status = %d, body = %d bytes", res.Code, res.Body.Len())
	}
	if res := serve(t, "/debug/pprof/heap?debug=1"); res.Code != http.StatusNotFound {
		t.Fatalf("GET /debug/pprof/heap status = %d, want %d", res.Code, http.StatusNotFound)
	}
}

func TestNewMetricsHandlerServesPprofWhenEnabled(t *testing.T) {
	t.Setenv(diagnostics.PprofEnv, "true")
	for _, path := range []string{"/metrics", "/debug/pprof/heap?debug=1"} {
		res := serve(t, path)
		if res.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want %d", path, res.Code, http.StatusOK)
		}
		if res.Body.Len() == 0 {
			t.Fatalf("GET %s returned empty body", path)
		}
	}
}

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewMetricsHandlerServesMetricsAndPprof(t *testing.T) {
	handler := newMetricsHandler()

	for _, path := range []string{"/metrics", "/debug/pprof/heap?debug=1"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		res := httptest.NewRecorder()

		handler.ServeHTTP(res, req)

		if res.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want %d", path, res.Code, http.StatusOK)
		}
		if res.Body.Len() == 0 {
			t.Fatalf("GET %s returned empty body", path)
		}
	}
}

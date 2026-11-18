package diagnostics

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func get(t *testing.T, mux *http.ServeMux, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func TestNewHandlerPprofDisabledByDefault(t *testing.T) {
	t.Setenv(PprofEnv, "")

	mux := NewHandler()
	if code := get(t, mux, "/metrics"); code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want %d", code, http.StatusOK)
	}
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap?debug=1", "/debug/pprof/goroutine"} {
		if code := get(t, mux, path); code != http.StatusNotFound {
			t.Errorf("%s status = %d, want %d (pprof must be opt-in)", path, code, http.StatusNotFound)
		}
	}
}

func TestNewHandlerPprofEnabled(t *testing.T) {
	t.Setenv(PprofEnv, "true")

	mux := NewHandler()
	if code := get(t, mux, "/metrics"); code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want %d", code, http.StatusOK)
	}
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap?debug=1", "/debug/pprof/goroutine"} {
		if code := get(t, mux, path); code != http.StatusOK {
			t.Errorf("%s status = %d, want %d", path, code, http.StatusOK)
		}
	}
}

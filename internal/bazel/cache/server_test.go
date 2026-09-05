package cache

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func setupTestServer(t *testing.T) (*Server, *Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(DefaultStoreOptions(dir))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return NewServer(store, nil), store
}

func TestHandlerGetACNotFound(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/ac/nonexistent/0", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandlerPutGetAC(t *testing.T) {
	srv, _ := setupTestServer(t)

	// PUT
	putBody := strings.NewReader("action-result-data")
	req := httptest.NewRequest(http.MethodPut, "/ac/test-hash/18", putBody)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT AC: got %d, want 200", rec.Code)
	}

	// GET
	req = httptest.NewRequest(http.MethodGet, "/ac/test-hash/18", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET AC: got %d, want 200", rec.Code)
	}
	if rec.Body.String() != "action-result-data" {
		t.Errorf("GET AC body: got %q, want action-result-data", rec.Body.String())
	}
}

func TestHandlerPutGetCAS(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := t.Context()

	raw := []byte("build-output-bytes")
	digest := DigestString(raw)
	// Put with correct digest so validation passes.
	store.PutCAS(ctx, digest, raw)

	// GET
	req := httptest.NewRequest(http.MethodGet, "/cas/"+digest, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET CAS: got %d, want 200", rec.Code)
	}
	if rec.Body.String() != string(raw) {
		t.Errorf("GET CAS body: got %q", rec.Body.String())
	}
}

func TestHandlerPutCAS(t *testing.T) {
	srv, _ := setupTestServer(t)

	raw := []byte("cached-output")
	digest := DigestString(raw)

	// PUT
	req := httptest.NewRequest(http.MethodPut, "/cas/"+digest, strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT CAS: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// GET verify
	req = httptest.NewRequest(http.MethodGet, "/cas/"+digest, nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET CAS after PUT: got %d", rec.Code)
	}
	if rec.Body.String() != string(raw) {
		t.Errorf("body mismatch: got %q", rec.Body.String())
	}
}

func TestHandlerGetCASNotFound(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/cas/nonexistent/0", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandlerStatus(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("status missing ok: %s", rec.Body.String())
	}
}

func TestHandlerNotFound(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/unknown", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for /unknown, got %d", rec.Code)
	}
}

func TestHandlerHeadAC(t *testing.T) {
	srv, _ := setupTestServer(t)

	// HEAD before put → 404
	req := httptest.NewRequest(http.MethodHead, "/ac/some-digest/5", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("HEAD before put: got %d, want 404", rec.Code)
	}

	// PUT
	putReq := httptest.NewRequest(http.MethodPut, "/ac/some-digest/5", strings.NewReader("hello"))
	putRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(putRec, putReq)

	// HEAD after put → 200
	req = httptest.NewRequest(http.MethodHead, "/ac/some-digest/5", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("HEAD after put: got %d, want 200", rec.Code)
	}
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/ac/some/5", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /ac/: got %d, want 405", rec.Code)
	}
}

func TestEviction(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := t.Context()

	// Put a CAS blob.
	raw := []byte("evict-me")
	digest := DigestString(raw)
	store.PutCAS(ctx, digest, raw)

	// Evict with a MaxAge of 0 (everything is too old).
	cfg := DefaultEvictionConfig()
	cfg.MaxAge = 0
	cfg.Interval = time.Hour // won't fire automatically
	srv.evict(ctx, cfg)

	// Blob should be gone.
	_, _, err := store.GetCAS(ctx, digest)
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound after eviction, got %v", err)
	}
}

func TestEvictionRecentBlob(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := t.Context()

	raw := []byte("keep-me")
	digest := DigestString(raw)
	store.PutCAS(ctx, digest, raw)

	// Evict with a large MaxAge → blob survives.
	cfg := DefaultEvictionConfig()
	cfg.MaxAge = 365 * 24 * time.Hour
	srv.evict(ctx, cfg)

	_, _, err := store.GetCAS(ctx, digest)
	if err != nil {
		t.Errorf("recent blob should survive eviction: %v", err)
	}
}

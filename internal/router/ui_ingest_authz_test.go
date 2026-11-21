package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/radryc/monofs/pkg/authz"
)

// TestHandleIngestReportsAuthorizationDenial guards the HTTP contract: a caller
// who is not allowed to ingest must see the denial in the response instead of
// an optimistic "Ingestion started" while the goroutine fails silently.
func TestHandleIngestReportsAuthorizationDenial(t *testing.T) {
	r := newAuthzTestRouter(t, true)
	handler := r.ServeHTTP()

	form := url.Values{
		"source":         {"https://github.com/radryc/packager"},
		"ingestion_type": {"git"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/ingest", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(authz.ContextWithIdentity(req.Context(), authz.Identity{
		Subject: "usera",
		Email:   "usera@example.com",
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusForbidden, rec.Body.String())
	}

	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Success {
		t.Fatal("denied ingest must not report success")
	}
	if !strings.Contains(resp.Message, "not authorized") {
		t.Fatalf("message = %q, want authorization failure", resp.Message)
	}
}

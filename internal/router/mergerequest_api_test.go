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

func mergeRequestAPIRequest(t *testing.T, r *Router, method, target, body string, identity authz.Identity) (*httptest.ResponseRecorder, http.Header) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req = req.WithContext(authz.ContextWithIdentity(req.Context(), identity))
	w := httptest.NewRecorder()
	r.handleMergeRequestsAPI(w, req)
	return w, w.Header()
}

func TestMergeRequestAPINotConfigured(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	req := httptest.NewRequest(http.MethodGet, "/api/merge-requests", nil)
	w := httptest.NewRecorder()
	r.handleMergeRequestsAPI(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestMergeRequestAPILifecycle(t *testing.T) {
	r := gateTestRouter(t, map[string]*authz.OwnersFile{
		"guardian/doctor": mustOwners(t, "owner-1"),
	})

	contributor := authz.Identity{Subject: "contributor"}
	owner := authz.Identity{Subject: "owner-1"}

	// Create a proposal authored by a non-owner.
	w, _ := mergeRequestAPIRequest(t, r, http.MethodPost, "/api/merge-requests",
		`{"partition":"doctor","paths":["guardian/doctor/a/x.txt"],"title":"fix x"}`, contributor)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", w.Code, w.Body.String())
	}
	var proposal map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &proposal); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	id, _ := proposal["ID"].(string)
	if id == "" {
		t.Fatalf("could not extract proposal id from %s", w.Body.String())
	}

	// Non-owner cannot approve.
	w, _ = mergeRequestAPIRequest(t, r, http.MethodPost, "/api/merge-requests/"+id+"/approve", "", contributor)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("non-owner approve status = %d, want 400", w.Code)
	}

	// Owner approves.
	w, _ = mergeRequestAPIRequest(t, r, http.MethodPost, "/api/merge-requests/"+id+"/approve", "", owner)
	if w.Code != http.StatusOK {
		t.Fatalf("owner approve status = %d, body %s", w.Code, w.Body.String())
	}

	// Owner merges.
	w, _ = mergeRequestAPIRequest(t, r, http.MethodPost, "/api/merge-requests/"+id+"/merge", "", owner)
	if w.Code != http.StatusOK {
		t.Fatalf("merge status = %d, body %s", w.Code, w.Body.String())
	}

	// List reflects the merged proposal.
	w, _ = mergeRequestAPIRequest(t, r, http.MethodGet, "/api/merge-requests", "", owner)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	var list struct {
		Proposals []map[string]any `json:"proposals"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Proposals) != 1 {
		t.Fatalf("expected 1 proposal, got %d", len(list.Proposals))
	}
	state, _ := list.Proposals[0]["State"].(string)
	if state != "merged" {
		t.Fatalf("expected merged state, got %q", state)
	}
}

func TestMergeRequestAPIAnonymousRejected(t *testing.T) {
	r := gateTestRouter(t, map[string]*authz.OwnersFile{
		"guardian/doctor": mustOwners(t, "owner-1"),
	})
	w, _ := mergeRequestAPIRequest(t, r, http.MethodPost, "/api/merge-requests",
		`{"partition":"doctor","paths":["guardian/doctor/a"],"title":"t"}`, authz.Identity{})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous create status = %d, want 401", w.Code)
	}
}

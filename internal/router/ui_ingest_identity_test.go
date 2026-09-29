package router

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/radryc/monofs/pkg/authz"
)

func TestDetachedIngestContextPreservesIdentity(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "/api/ingest", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	want := authz.Identity{Subject: "alice@example.com"}
	req = req.WithContext(authz.ContextWithIdentity(req.Context(), want))

	ctx := detachedIngestContext(req)
	got, ok := authz.IdentityFromContext(ctx)
	if !ok || got.Subject != want.Subject {
		t.Fatalf("identity not preserved: got %+v (ok=%v), want %+v", got, ok, want)
	}
	// The detached context must outlive the request that produced it.
	if err := ctx.Err(); err != nil {
		t.Fatalf("detached context should not be cancelled: %v", err)
	}
}

func TestIngestContextServiceIdentityFallback(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	r.AddBreakGlassAdmin("break-glass-admin")

	// Anonymous caller falls back to the router service identity.
	id, ok := authz.IdentityFromContext(r.ingestContext(context.Background()))
	if !ok || !r.isBreakGlassAdmin(id) {
		t.Fatalf("expected break-glass service identity, got %+v (ok=%v)", id, ok)
	}

	// A real caller identity is preserved rather than overwritten.
	caller := authz.Identity{Subject: "alice@example.com"}
	ctx := authz.ContextWithIdentity(context.Background(), caller)
	got, _ := authz.IdentityFromContext(r.ingestContext(ctx))
	if got.Subject != caller.Subject {
		t.Fatalf("caller identity overwritten: got %+v, want %+v", got, caller)
	}
}

func TestInternalIdentityContextCarriesServiceIdentity(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	r.AddBreakGlassAdmin("break-glass-admin")

	id, ok := authz.IdentityFromContext(r.internalIdentityContext())
	if !ok || !r.isBreakGlassAdmin(id) {
		t.Fatalf("internal context should carry break-glass admin, got %+v (ok=%v)", id, ok)
	}
}

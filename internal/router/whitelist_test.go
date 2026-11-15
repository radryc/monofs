package router

import (
	"context"
	"log/slog"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
)

func newWhitelistTestRouter() *Router {
	return &Router{
		whitelist: newWhitelistStore(),
		logger:    slog.New(slog.DiscardHandler),
	}
}

func TestWhitelistStoreDisabledAllowsAll(t *testing.T) {
	store := newWhitelistStore()
	if store.Enabled() {
		t.Fatal("whitelist should start disabled")
	}
	if !store.IsAllowed("any-client") {
		t.Fatal("disabled whitelist rejected a client")
	}
	if len(store.List()) != 0 {
		t.Fatalf("entries = %d, want 0", len(store.List()))
	}
}

func TestWhitelistStoreEnableAndMembership(t *testing.T) {
	store := newWhitelistStore()
	store.Add("client-1", "first")
	store.Add("client-1", "updated")
	store.Add("client-2", "second")
	store.SetEnabled(true)

	if !store.IsAllowed("client-1") {
		t.Fatal("whitelisted client rejected")
	}
	if store.IsAllowed("client-3") {
		t.Fatal("unknown client allowed while enabled")
	}
	if !store.Remove("client-1") {
		t.Fatal("Remove reported missing client")
	}
	if store.Remove("client-1") {
		t.Fatal("second Remove succeeded")
	}
	if store.IsAllowed("client-1") {
		t.Fatal("removed client still allowed")
	}

	entries := store.List()
	if len(entries) != 1 || entries[0].clientID != "client-2" || entries[0].label != "second" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestWhitelistRPCAddRemoveListStatus(t *testing.T) {
	r := newWhitelistTestRouter()
	ctx := context.Background()

	// Empty client IDs are rejected without touching the store.
	if resp, err := r.AddWhitelistedClient(ctx, &pb.AddWhitelistedClientRequest{}); err != nil || resp.GetSuccess() {
		t.Fatalf("empty add = %+v, err=%v", resp, err)
	}
	if resp, err := r.RemoveWhitelistedClient(ctx, &pb.RemoveWhitelistedClientRequest{}); err != nil || resp.GetSuccess() {
		t.Fatalf("empty remove = %+v, err=%v", resp, err)
	}

	if resp, err := r.AddWhitelistedClient(ctx, &pb.AddWhitelistedClientRequest{ClientId: "client-1", Label: "first"}); err != nil || !resp.GetSuccess() {
		t.Fatalf("add = %+v, err=%v", resp, err)
	}
	if _, err := r.SetWhitelistEnabled(ctx, &pb.SetWhitelistEnabledRequest{Enabled: true}); err != nil {
		t.Fatalf("set enabled: %v", err)
	}

	list, err := r.ListWhitelistedClients(ctx, &pb.ListWhitelistedClientsRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !list.GetWhitelistEnabled() || len(list.GetClients()) != 1 {
		t.Fatalf("list = %+v", list)
	}
	if list.GetClients()[0].GetClientId() != "client-1" || list.GetClients()[0].GetLabel() != "first" {
		t.Fatalf("client = %+v", list.GetClients()[0])
	}
	if list.GetClients()[0].GetAddedAt() == 0 {
		t.Fatal("added_at was not set")
	}

	status, err := r.GetWhitelistStatus(ctx, &pb.GetWhitelistStatusRequest{})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !status.GetEnabled() || status.GetWhitelistedCount() != 1 {
		t.Fatalf("status = %+v", status)
	}

	// Removing an unknown client fails without an RPC error.
	if resp, err := r.RemoveWhitelistedClient(ctx, &pb.RemoveWhitelistedClientRequest{ClientId: "missing"}); err != nil || resp.GetSuccess() {
		t.Fatalf("remove missing = %+v, err=%v", resp, err)
	}
	if resp, err := r.RemoveWhitelistedClient(ctx, &pb.RemoveWhitelistedClientRequest{ClientId: "client-1"}); err != nil || !resp.GetSuccess() {
		t.Fatalf("remove = %+v, err=%v", resp, err)
	}

	if _, err := r.SetWhitelistEnabled(ctx, &pb.SetWhitelistEnabledRequest{Enabled: false}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if r.whitelist.Enabled() {
		t.Fatal("whitelist still enabled")
	}
}

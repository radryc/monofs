package repowatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeLeaseAPI struct {
	mu        sync.Mutex
	lease     *leaseObject
	create    int
	update    int
	lastPath  string
	lastToken string
}

func (f *fakeLeaseAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPath = r.URL.Path
	f.lastToken = r.Header.Get("Authorization")
	switch r.Method {
	case http.MethodGet:
		if f.lease == nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(f.lease)
	case http.MethodPost:
		var obj leaseObject
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.create++
		obj.Metadata.ResourceVersion = fmt.Sprintf("%d", f.create+f.update)
		f.lease = &obj
		_ = json.NewEncoder(w).Encode(obj)
	case http.MethodPut:
		if f.lease == nil {
			http.NotFound(w, r)
			return
		}
		var obj leaseObject
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.update++
		obj.Metadata.ResourceVersion = fmt.Sprintf("%d", f.create+f.update)
		f.lease = &obj
		_ = json.NewEncoder(w).Encode(obj)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func newTestElector(t *testing.T, server *httptest.Server, identity string, duration time.Duration) *Elector {
	t.Helper()
	e, err := NewElector(LeaseConfig{
		Namespace: "default",
		Name:      "test-lease",
		Identity:  identity,
		Duration:  duration,
		Renew:     10 * time.Millisecond,
		APIServer: server.URL,
		Client:    server.Client(),
	})
	if err != nil {
		t.Fatalf("NewElector: %v", err)
	}
	return e
}

func TestElectorAcquireAndRenew(t *testing.T) {
	api := &fakeLeaseAPI{}
	server := httptest.NewServer(api)
	defer server.Close()
	e := newTestElector(t, server, "router-a", 2*time.Second)

	ok, err := e.tryAcquire(context.Background())
	if err != nil || !ok {
		t.Fatalf("tryAcquire = %v, %v", ok, err)
	}
	if api.lease == nil || api.lease.Spec.HolderIdentity != "router-a" {
		t.Fatalf("lease = %+v", api.lease)
	}
	firstRenew := api.lease.Spec.RenewTime

	time.Sleep(2 * time.Millisecond)
	if err := e.renew(context.Background()); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if api.lease.Spec.RenewTime == firstRenew {
		t.Fatal("renewTime did not change")
	}
	if api.create != 1 || api.update < 1 {
		t.Fatalf("create/update = %d/%d", api.create, api.update)
	}
	if api.lastPath != "/apis/coordination.k8s.io/v1/namespaces/default/leases/test-lease" {
		t.Fatalf("last path = %q", api.lastPath)
	}
}

func TestElectorRespectsActiveLease(t *testing.T) {
	api := &fakeLeaseAPI{lease: &leaseObject{
		APIVersion: leaseAPIVersion,
		Kind:       leaseKind,
		Metadata:   leaseMetadata{Name: "test-lease", Namespace: "default", ResourceVersion: "1"},
		Spec: leaseSpec{
			HolderIdentity:       "router-b",
			LeaseDurationSeconds: 15,
			RenewTime:            time.Now().UTC().Format(time.RFC3339Nano),
		},
	}}
	server := httptest.NewServer(api)
	defer server.Close()
	e := newTestElector(t, server, "router-a", 15*time.Second)

	ok, err := e.tryAcquire(context.Background())
	if err != nil {
		t.Fatalf("tryAcquire: %v", err)
	}
	if ok {
		t.Fatal("acquired an active lease held by another identity")
	}
	if api.lease.Spec.HolderIdentity != "router-b" {
		t.Fatalf("holder = %q", api.lease.Spec.HolderIdentity)
	}
}

func TestElectorTakesExpiredLease(t *testing.T) {
	api := &fakeLeaseAPI{lease: &leaseObject{
		APIVersion: leaseAPIVersion,
		Kind:       leaseKind,
		Metadata:   leaseMetadata{Name: "test-lease", Namespace: "default", ResourceVersion: "1"},
		Spec: leaseSpec{
			HolderIdentity:       "router-b",
			LeaseDurationSeconds: 1,
			AcquireTime:          time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
			RenewTime:            time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
			LeaseTransitions:     2,
		},
	}}
	server := httptest.NewServer(api)
	defer server.Close()
	e := newTestElector(t, server, "router-a", time.Second)

	ok, err := e.tryAcquire(context.Background())
	if err != nil {
		t.Fatalf("tryAcquire: %v", err)
	}
	if !ok {
		t.Fatal("expected to take an expired lease")
	}
	if api.lease.Spec.HolderIdentity != "router-a" {
		t.Fatalf("holder = %q", api.lease.Spec.HolderIdentity)
	}
	if api.lease.Spec.LeaseTransitions != 3 {
		t.Fatalf("transitions = %d, want 3", api.lease.Spec.LeaseTransitions)
	}
}

func TestShardFor(t *testing.T) {
	if got := ShardFor("repo-1", 1); got != 0 {
		t.Fatalf("shard for one shard = %d", got)
	}
	for i := 0; i < 100; i++ {
		got := ShardFor(fmt.Sprintf("repo-%d", i), 8)
		if got < 0 || got >= 8 {
			t.Fatalf("shard = %d, want 0..7", got)
		}
	}
}

package router

import (
	"errors"
	"net"
	"path/filepath"
	"testing"

	"github.com/radryc/kvs/pkg/grpcserver"
	"github.com/radryc/kvs/pkg/localstore"
	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
)

func startEmbeddedKVS(t *testing.T) (string, func()) {
	t.Helper()
	store, err := localstore.Open(localstore.Config{
		DataDir:        filepath.Join(t.TempDir(), "kvs"),
		MaxHotVersions: 4,
	})
	if err != nil {
		t.Fatalf("open embedded kvs: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	grpcserver.Register(server, store, grpcserver.Config{})
	go func() { _ = server.Serve(listener) }()
	return listener.Addr().String(), func() {
		server.Stop()
		_ = store.Close()
	}
}

func newKVSBackedStore(t *testing.T, address string) (*guardianVersionStore, *Router) {
	t.Helper()
	store, err := newGuardianVersionStore(t.TempDir())
	if err != nil {
		t.Fatalf("newGuardianVersionStore: %v", err)
	}
	router := &Router{
		nodes: map[string]*nodeState{
			"node-1": {
				info:      &pb.NodeInfo{NodeId: "node-1", Address: address, Healthy: true},
				kvsStatus: &pb.KVSNodeStatus{Enabled: true, Healthy: true, Role: "leader", LeaderAddress: address},
			},
		},
	}
	store.kvs = newKVSGuardianVersionBackend(router, store)
	return store, router
}

func TestKVSGuardianVersionBackendCASAcrossStores(t *testing.T) {
	address, stop := startEmbeddedKVS(t)
	defer stop()

	storeA, _ := newKVSBackedStore(t, address)
	defer storeA.close()
	storeB, _ := newKVSBackedStore(t, address)
	defer storeB.close()

	path := "/partitions/demo/intents/app.yaml"
	first, err := storeA.commit(guardianVersionCommit{
		LogicalPath:       path,
		DisplayPath:       "guardian/demo",
		StorageID:         "storage-demo",
		Content:           []byte("v1"),
		ExpectedVersionID: "absent",
		CommittedAt:       1,
	})
	if err != nil {
		t.Fatalf("storeA absent create: %v", err)
	}

	// The other "router" sees the same authoritative record.
	currentB, ok := storeB.currentVersion(path)
	if !ok || currentB.VersionID != first.GetVersionId() {
		t.Fatalf("storeB current = %+v, ok=%v, want %s", currentB, ok, first.GetVersionId())
	}

	if _, err := storeB.commit(guardianVersionCommit{
		LogicalPath:       path,
		Content:           []byte("v2"),
		ExpectedVersionID: "absent",
		CommittedAt:       2,
	}); !errors.Is(err, errGuardianVersionExists) {
		t.Fatalf("storeB second absent create = %v, want exists", err)
	}

	second, err := storeB.commit(guardianVersionCommit{
		LogicalPath:       path,
		Content:           []byte("v2"),
		ExpectedVersionID: first.GetVersionId(),
		CommittedAt:       3,
	})
	if err != nil {
		t.Fatalf("storeB CAS update: %v", err)
	}

	// StoreA's stale expected version must now conflict.
	if _, err := storeA.commit(guardianVersionCommit{
		LogicalPath:       path,
		Content:           []byte("stale"),
		ExpectedVersionID: first.GetVersionId(),
		CommittedAt:       4,
	}); !errors.Is(err, errGuardianVersionConflict) {
		t.Fatalf("storeA stale CAS = %v, want conflict", err)
	}

	versions, _, err := storeA.list(path, 10, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(versions) != 2 || versions[0].GetVersionId() != second.GetVersionId() {
		t.Fatalf("versions = %+v, want newest %s", versions, second.GetVersionId())
	}

	version, content, available, found, err := storeB.get(path, second.GetVersionId())
	if err != nil || !found || !available {
		t.Fatalf("get = version=%v available=%v found=%v err=%v", version, available, found, err)
	}
	if string(content) != "v2" {
		t.Fatalf("content = %q, want v2", string(content))
	}
	if _, _, availableA, foundA, err := storeA.get(path, second.GetVersionId()); err != nil || !foundA || availableA {
		t.Fatalf("storeA get = available=%v found=%v err=%v, want metadata-only", availableA, foundA, err)
	}
}

func TestGuardianCoordinatorLeaseGating(t *testing.T) {
	r := &Router{config: RouterConfig{GuardianVersionBackend: "kvs", GuardianCoordinatorLease: "required"}}
	r.guardianLeaseMode = "required"

	if err := r.requireGuardianCoordinator(); err == nil {
		t.Fatal("expected non-leader mutation to be rejected")
	}
	r.guardianLeader.Store(true)
	if err := r.requireGuardianCoordinator(); err != nil {
		t.Fatalf("leader mutation rejected: %v", err)
	}
	r.guardianLeaseUnavailable.Store(true)
	if err := r.requireGuardianCoordinator(); err == nil {
		t.Fatal("expected unavailable lease to reject mutation")
	}
	r.guardianLeaseUnavailable.Store(false)
	r.config.GuardianAllowMultiWriter = true
	r.guardianLeader.Store(false)
	if err := r.requireGuardianCoordinator(); err != nil {
		t.Fatalf("multi-writer override rejected: %v", err)
	}

	// KVS backend with the lease explicitly off must still fail closed unless
	// the multi-writer override is set.
	r2 := &Router{config: RouterConfig{GuardianVersionBackend: "kvs", GuardianCoordinatorLease: "off"}}
	r2.guardianLeaseMode = "off"
	if err := r2.requireGuardianCoordinator(); err == nil {
		t.Fatal("expected kvs+lease-off to be rejected")
	}
}

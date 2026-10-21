package router

import (
	"context"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

func TestGuardianVersionStoreEphemeralTombstoneEvictsMemory(t *testing.T) {
	store, err := newGuardianVersionStore(t.TempDir())
	if err != nil {
		t.Fatalf("newGuardianVersionStore: %v", err)
	}
	defer store.close()

	path := "/.queues/aws-1/tasks/task-1.json"
	nowUnix := time.Now().UTC().Unix()
	v1, err := store.commit(guardianVersionCommit{
		LogicalPath:     path,
		BatchRevisionID: "rev-1",
		Content:         []byte(`{"id":"task-1"}`),
		CommittedAt:     nowUnix,
	})
	if err != nil {
		t.Fatalf("commit create: %v", err)
	}
	if _, ok := store.currentVersion(path); !ok {
		t.Fatalf("expected ephemeral entry to exist before delete")
	}

	// Sub-second overwrite with identical content and CommittedAt must produce a distinct VersionID.
	v2, err := store.commit(guardianVersionCommit{
		LogicalPath:     path,
		BatchRevisionID: "rev-2",
		Content:         []byte(`{"id":"task-1"}`),
		CommittedAt:     nowUnix,
	})
	if err != nil {
		t.Fatalf("commit update: %v", err)
	}
	if v1.GetVersionId() == v2.GetVersionId() {
		t.Fatalf("expected distinct VersionIDs on sub-second commit, got %q", v1.GetVersionId())
	}

	// Tombstone commit on machinery path must evict from s.ephemeral so 50K pushers never leak RAM.
	if _, err := store.commit(guardianVersionCommit{
		LogicalPath:     path,
		BatchRevisionID: "rev-3",
		Tombstone:       true,
		CommittedAt:     nowUnix,
	}); err != nil {
		t.Fatalf("commit tombstone: %v", err)
	}
	store.mu.RLock()
	_, exists := store.ephemeral[path]
	store.mu.RUnlock()
	if exists {
		t.Fatalf("expected tombstoned machinery path %s to be evicted from s.ephemeral", path)
	}
}

func TestGuardianPrincipalStoreIndexedTokenLookupAndCoalescedSave(t *testing.T) {
	store, err := newGuardianPrincipalStore(t.TempDir())
	if err != nil {
		t.Fatalf("newGuardianPrincipalStore: %v", err)
	}
	defer store.close()

	rec, err := store.upsertConnectedClient("guardian-pusher:aws-1", "secret-token-1", "pusher", "aws-1", "http://aws-1:19090")
	if err != nil {
		t.Fatalf("upsertConnectedClient: %v", err)
	}
	validated, ok := store.validateToken("secret-token-1")
	if !ok || validated == nil {
		t.Fatalf("validateToken failed for registered token")
	}
	if validated.PrincipalID != rec.PrincipalID {
		t.Fatalf("PrincipalID = %q, want %q", validated.PrincipalID, rec.PrincipalID)
	}

	// Repeating identical upsertConnectedClient should be a fast no-op.
	if _, err := store.upsertConnectedClient("guardian-pusher:aws-1", "secret-token-1", "pusher", "aws-1", "http://aws-1:19090"); err != nil {
		t.Fatalf("upsertConnectedClient repeat: %v", err)
	}
}

func TestGuardianUpsertCachesRegisterRepositoryAndSupportsHRWReplicas(t *testing.T) {
	router, nodes, cleanup := newGuardianMultiNodeRouterTestHarness(t, map[string]*pb.KVSNodeStatus{
		"node-1": {Enabled: true, Healthy: true},
		"node-2": {Enabled: true, Healthy: true},
		"node-3": {Enabled: true, Healthy: true},
	})
	defer cleanup()
	router.config.GuardianReplicationFactor = 2

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := router.UpsertGuardianPaths(ctx, &pb.UpsertGuardianPathsRequest{
			GuardianToken: "secret-token",
			Writes: []*pb.GuardianPathWrite{{
				LogicalPath: "/partitions/genomics/intents/web.yaml",
				Content:     []byte("kind: Intent\n"),
			}},
		})
		if err != nil {
			t.Fatalf("UpsertGuardianPaths(%d): %v", i, err)
		}
	}

	totalRegCalls := 0
	totalIngestCalls := 0
	nodesTargeted := 0
	for _, n := range nodes {
		n.mu.Lock()
		if n.ingestBatchCalls > 0 {
			nodesTargeted++
		}
		totalRegCalls += n.registerCalls
		totalIngestCalls += n.ingestBatchCalls
		n.mu.Unlock()
	}
	if nodesTargeted != 2 {
		t.Fatalf("nodesTargeted = %d, want 2 (HRW GuardianReplicationFactor=2)", nodesTargeted)
	}
	if totalRegCalls != 2 {
		t.Fatalf("totalRegCalls = %d, want 2 (cached after first upsert per replica)", totalRegCalls)
	}
	if totalIngestCalls != 6 {
		t.Fatalf("totalIngestCalls = %d, want 6 (3 upserts * 2 replicas)", totalIngestCalls)
	}
}

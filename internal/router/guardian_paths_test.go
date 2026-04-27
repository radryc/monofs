package router

import (
	"context"
	"errors"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestGuardianPathsWriteReadDeleteAndHistory(t *testing.T) {
	router, nodeClient, _, cleanup := newGuardianRouterTestHarness(t)
	defer cleanup()

	ctx := context.Background()

	if _, err := router.UnregisterClient(ctx, &pb.UnregisterClientRequest{
		ClientId: "guardian-cli",
		Reason:   "test principal persistence",
	}); err != nil {
		t.Fatalf("UnregisterClient() error = %v", err)
	}

	if _, ok := router.validateGuardianToken("secret-token"); !ok {
		t.Fatal("expected persisted guardian token to remain valid after client disconnect")
	}

	writeResp, err := router.UpsertGuardianPaths(ctx, &pb.UpsertGuardianPathsRequest{
		GuardianToken: "secret-token",
		Writes: []*pb.GuardianPathWrite{{
			LogicalPath: "/partitions/genomics/intents/workers.yaml",
			Content:     []byte("apiVersion: guardian/v1alpha1\nkind: Intent\n"),
		}},
		Context: &pb.GuardianMutationContext{
			Reason:        "test write",
			CorrelationId: "corr-write",
		},
	})
	if err != nil {
		t.Fatalf("UpsertGuardianPaths() error = %v", err)
	}
	if !writeResp.GetSuccess() || len(writeResp.GetVersions()) != 1 {
		t.Fatalf("unexpected upsert response: %+v", writeResp)
	}

	attr, err := nodeClient.GetAttr(ctx, &pb.GetAttrRequest{Path: "guardian/genomics/intents/workers.yaml"})
	if err != nil {
		t.Fatalf("GetAttr() error = %v", err)
	}
	if !attr.GetFound() {
		t.Fatal("expected upserted file to be readable through MonoFS I/O")
	}

	content := readAllFromMonoFSClient(t, nodeClient, "guardian/genomics/intents/workers.yaml")
	if string(content) != "apiVersion: guardian/v1alpha1\nkind: Intent\n" {
		t.Fatalf("read content = %q", string(content))
	}

	deleteResp, err := router.DeleteGuardianPaths(ctx, &pb.DeleteGuardianPathsRequest{
		GuardianToken: "secret-token",
		Deletes: []*pb.GuardianPathDelete{{
			LogicalPath:       "/partitions/genomics/intents/workers.yaml",
			ExpectedVersionId: writeResp.GetVersions()[0].GetVersionId(),
		}},
		Context: &pb.GuardianMutationContext{
			Reason:        "test delete",
			CorrelationId: "corr-delete",
		},
	})
	if err != nil {
		t.Fatalf("DeleteGuardianPaths() error = %v", err)
	}
	if !deleteResp.GetSuccess() || len(deleteResp.GetTombstones()) != 1 {
		t.Fatalf("unexpected delete response: %+v", deleteResp)
	}

	attr, err = nodeClient.GetAttr(ctx, &pb.GetAttrRequest{Path: "guardian/genomics/intents/workers.yaml"})
	if err != nil {
		t.Fatalf("GetAttr() after delete error = %v", err)
	}
	if attr.GetFound() {
		t.Fatal("expected deleted file to disappear from current MonoFS view")
	}

	listResp, err := router.ListGuardianVersions(ctx, &pb.ListGuardianVersionsRequest{
		GuardianToken: "secret-token",
		LogicalPath:   "/partitions/genomics/intents/workers.yaml",
	})
	if err != nil {
		t.Fatalf("ListGuardianVersions() error = %v", err)
	}
	if len(listResp.GetVersions()) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(listResp.GetVersions()))
	}
	if !listResp.GetVersions()[0].GetTombstone() {
		t.Fatal("expected newest version to be the tombstone")
	}

	getResp, err := router.GetGuardianVersion(ctx, &pb.GetGuardianVersionRequest{
		GuardianToken: "secret-token",
		LogicalPath:   "/partitions/genomics/intents/workers.yaml",
		VersionId:     writeResp.GetVersions()[0].GetVersionId(),
	})
	if err != nil {
		t.Fatalf("GetGuardianVersion() error = %v", err)
	}
	if string(getResp.GetContent()) != string(content) {
		t.Fatalf("historical content = %q, want %q", string(getResp.GetContent()), string(content))
	}
}

func TestDoctorPathsWriteReadAndHistory(t *testing.T) {
	router, nodeClient, _, cleanup := newGuardianRouterTestHarness(t)
	defer cleanup()

	ctx := context.Background()
	resp, err := router.RegisterClient(ctx, &pb.RegisterClientRequest{
		ClientId: "doctor-query-1",
		GuardianConfig: &pb.GuardianConfig{
			AuthToken:   "doctor-token",
			PrincipalId: "doctor-query",
			Role:        "doctor",
			DisplayName: "doctor-query",
		},
	})
	if err != nil {
		t.Fatalf("RegisterClient() error = %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("RegisterClient() failed: %+v", resp)
	}

	logicalPath := "/doctor/v1/catalog/manifests/traces/default/2026-04-09/15/trace-1.json"
	physicalPath := "doctor/v1/catalog/manifests/traces/default/2026-04-09/15/trace-1.json"

	writeResp, err := router.UpsertGuardianPaths(ctx, &pb.UpsertGuardianPathsRequest{
		GuardianToken: "doctor-token",
		Writes: []*pb.GuardianPathWrite{{
			LogicalPath: logicalPath,
			Content:     []byte(`{"id":"trace-1"}`),
		}},
		Context: &pb.GuardianMutationContext{
			PrincipalId:   "doctor-query",
			Reason:        "doctor catalog write",
			CorrelationId: "corr-doctor-write",
		},
	})
	if err != nil {
		t.Fatalf("UpsertGuardianPaths() error = %v", err)
	}
	if !writeResp.GetSuccess() || len(writeResp.GetVersions()) != 1 {
		t.Fatalf("unexpected upsert response: %+v", writeResp)
	}

	attr, err := nodeClient.GetAttr(ctx, &pb.GetAttrRequest{Path: physicalPath})
	if err != nil {
		t.Fatalf("GetAttr() error = %v", err)
	}
	if !attr.GetFound() {
		t.Fatal("expected upserted doctor file to be readable through MonoFS I/O")
	}

	content := readAllFromMonoFSClient(t, nodeClient, physicalPath)
	if string(content) != `{"id":"trace-1"}` {
		t.Fatalf("read content = %q", string(content))
	}

	listResp, err := router.ListGuardianVersions(ctx, &pb.ListGuardianVersionsRequest{
		GuardianToken: "doctor-token",
		LogicalPath:   logicalPath,
	})
	if err != nil {
		t.Fatalf("ListGuardianVersions() error = %v", err)
	}
	if len(listResp.GetVersions()) != 1 {
		t.Fatalf("expected 1 version, got %d", len(listResp.GetVersions()))
	}

	getResp, err := router.GetGuardianVersion(ctx, &pb.GetGuardianVersionRequest{
		GuardianToken: "doctor-token",
		LogicalPath:   logicalPath,
		VersionId:     writeResp.GetVersions()[0].GetVersionId(),
	})
	if err != nil {
		t.Fatalf("GetGuardianVersion() error = %v", err)
	}
	if string(getResp.GetContent()) != string(content) {
		t.Fatalf("historical content = %q, want %q", string(getResp.GetContent()), string(content))
	}
}

func TestSubscribeGuardianChangesReceivesLogicalEvents(t *testing.T) {
	router, _, _, cleanup := newGuardianRouterTestHarness(t)
	defer cleanup()

	stream := newMockGuardianLogicalChangeStream()
	errCh := make(chan error, 1)
	go func() {
		errCh <- router.SubscribeGuardianChanges(&pb.SubscribeGuardianChangesRequest{
			GuardianToken:        "secret-token",
			LogicalPrefixes:      []string{"/partitions/genomics/intents"},
			IncludeInlineContent: true,
		}, stream)
	}()

	waitForCondition(t, func() bool {
		router.guardianLogicalChangeSubsMu.RLock()
		defer router.guardianLogicalChangeSubsMu.RUnlock()
		return len(router.guardianLogicalChangeSubs) == 1
	})

	_, err := router.UpsertGuardianPaths(context.Background(), &pb.UpsertGuardianPathsRequest{
		GuardianToken: "secret-token",
		Writes: []*pb.GuardianPathWrite{{
			LogicalPath: "/partitions/genomics/intents/web.yaml",
			Content:     []byte("kind: Intent\n"),
		}},
		Context: &pb.GuardianMutationContext{
			Reason:        "watch test",
			CorrelationId: "corr-watch",
		},
	})
	if err != nil {
		t.Fatalf("UpsertGuardianPaths() error = %v", err)
	}

	waitForCondition(t, func() bool {
		return len(stream.Events()) == 1
	})

	stream.cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("SubscribeGuardianChanges() error = %v", err)
	}

	events := stream.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].GetLogicalPath() != "/partitions/genomics/intents/web.yaml" {
		t.Fatalf("logical path = %q", events[0].GetLogicalPath())
	}
	if events[0].GetVersionId() == "" {
		t.Fatal("expected version_id in logical change event")
	}
	if string(events[0].GetInlineContent()) != "kind: Intent\n" {
		t.Fatalf("inline content = %q", string(events[0].GetInlineContent()))
	}
}

func TestGetClusterInfoGuardianAlwaysVisible(t *testing.T) {
	router := NewRouter(DefaultRouterConfig(), nil)
	defer router.Close()

	resp, err := router.GetClusterInfo(context.Background(), &pb.ClusterInfoRequest{})
	if err != nil {
		t.Fatalf("GetClusterInfo() error = %v", err)
	}
	if !resp.GetGuardianVisible() {
		t.Fatal("expected guardian namespace to always be visible")
	}
}

func TestRegisterGuardianClientWithoutBaseURL(t *testing.T) {
	cfg := DefaultRouterConfig()
	cfg.GuardianStateDir = t.TempDir()
	router := NewRouter(cfg, nil)
	defer router.Close()

	resp, err := router.RegisterClient(context.Background(), &pb.RegisterClientRequest{
		ClientId: "guardian-pusher-local",
		GuardianConfig: &pb.GuardianConfig{
			AuthToken: "secret-token",
		},
	})
	if err != nil {
		t.Fatalf("RegisterClient() error = %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("RegisterClient() failed: %+v", resp)
	}

	if clientID, ok := router.validateGuardianToken("secret-token"); !ok || clientID != "guardian-pusher-local" {
		t.Fatalf("validateGuardianToken() = (%q, %v), want (%q, true)", clientID, ok, "guardian-pusher-local")
	}

	router.guardianClientsMu.RLock()
	registered := router.guardianClients["guardian-pusher-local"]
	router.guardianClientsMu.RUnlock()
	if registered == nil {
		t.Fatal("expected guardian client to be tracked")
	}
	if registered.baseURL != "" {
		t.Fatalf("guardian baseURL = %q, want empty", registered.baseURL)
	}
}

func TestRegisterGuardianClientUsesConfiguredPrincipal(t *testing.T) {
	cfg := DefaultRouterConfig()
	cfg.GuardianStateDir = t.TempDir()
	router := NewRouter(cfg, nil)
	defer router.Close()

	resp, err := router.RegisterClient(context.Background(), &pb.RegisterClientRequest{
		ClientId: "guardian-pusher-docker-12345",
		GuardianConfig: &pb.GuardianConfig{
			AuthToken:   "secret-token",
			PrincipalId: "guardian-pusher-docker-main",
			Role:        "pusher",
			DisplayName: "docker-main",
		},
	})
	if err != nil {
		t.Fatalf("RegisterClient() error = %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("RegisterClient() failed: %+v", resp)
	}

	if principalID, ok := router.validateGuardianToken("secret-token"); !ok || principalID != "guardian-pusher-docker-main" {
		t.Fatalf("validateGuardianToken() = (%q, %v), want (%q, true)", principalID, ok, "guardian-pusher-docker-main")
	}

	router.guardianClientsMu.RLock()
	registered := router.guardianClients["guardian-pusher-docker-12345"]
	router.guardianClientsMu.RUnlock()
	if registered == nil {
		t.Fatal("expected guardian client to be tracked")
	}
	if registered.principalID != "guardian-pusher-docker-main" {
		t.Fatalf("registered principalID = %q, want %q", registered.principalID, "guardian-pusher-docker-main")
	}
	if registered.role != "pusher" {
		t.Fatalf("registered role = %q, want %q", registered.role, "pusher")
	}
	if registered.displayName != "docker-main" {
		t.Fatalf("registered displayName = %q, want %q", registered.displayName, "docker-main")
	}
}

func TestClientHeartbeatRefreshesGuardianClientHeartbeat(t *testing.T) {
	cfg := DefaultRouterConfig()
	cfg.GuardianStateDir = t.TempDir()
	router := NewRouter(cfg, nil)
	defer router.Close()

	const clientID = "guardian-pusher-local"
	resp, err := router.RegisterClient(context.Background(), &pb.RegisterClientRequest{
		ClientId: clientID,
		GuardianConfig: &pb.GuardianConfig{
			AuthToken: "secret-token",
		},
	})
	if err != nil {
		t.Fatalf("RegisterClient() error = %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("RegisterClient() failed: %+v", resp)
	}

	staleAt := time.Now().Add(-2 * time.Minute)

	router.clientsMu.RLock()
	state := router.clients[clientID]
	router.clientsMu.RUnlock()
	if state == nil {
		t.Fatal("expected client state to exist")
	}
	state.mu.Lock()
	state.lastHeartbeat = staleAt
	state.info.LastHeartbeat = staleAt.Unix()
	state.info.State = pb.ClientState_CLIENT_STALE
	state.mu.Unlock()

	router.guardianClientsMu.Lock()
	guardianState := router.guardianClients[clientID]
	if guardianState == nil {
		router.guardianClientsMu.Unlock()
		t.Fatal("expected guardian client state to exist")
	}
	guardianState.lastHeartbeat = staleAt
	router.guardianClientsMu.Unlock()

	heartbeatResp, err := router.ClientHeartbeat(context.Background(), &pb.ClientHeartbeatRequest{
		ClientId: clientID,
	})
	if err != nil {
		t.Fatalf("ClientHeartbeat() error = %v", err)
	}
	if !heartbeatResp.GetSuccess() {
		t.Fatalf("ClientHeartbeat() failed: %+v", heartbeatResp)
	}

	state.mu.RLock()
	if state.info.State != pb.ClientState_CLIENT_CONNECTED {
		t.Fatalf("client state = %v, want %v", state.info.State, pb.ClientState_CLIENT_CONNECTED)
	}
	if state.info.LastHeartbeat <= staleAt.Unix() {
		t.Fatalf("client last heartbeat = %d, want > %d", state.info.LastHeartbeat, staleAt.Unix())
	}
	state.mu.RUnlock()

	router.guardianClientsMu.RLock()
	updatedGuardianState := router.guardianClients[clientID]
	router.guardianClientsMu.RUnlock()
	if updatedGuardianState == nil {
		t.Fatal("expected guardian client state to remain tracked")
	}
	if !updatedGuardianState.lastHeartbeat.After(staleAt) {
		t.Fatalf("guardian last heartbeat = %v, want after %v", updatedGuardianState.lastHeartbeat, staleAt)
	}
}

func TestAuthenticateGuardianMutationUsesRequestedPrincipal(t *testing.T) {
	cfg := DefaultRouterConfig()
	cfg.GuardianStateDir = t.TempDir()
	router := NewRouter(cfg, nil)
	defer router.Close()

	for _, req := range []*pb.RegisterClientRequest{
		{
			ClientId: "guardian-cli-1",
			GuardianConfig: &pb.GuardianConfig{
				AuthToken:   "shared-token",
				PrincipalId: "guardianctl",
				Role:        "cli",
				DisplayName: "guardianctl",
			},
		},
		{
			ClientId: "guardian-pusher-docker-1",
			GuardianConfig: &pb.GuardianConfig{
				AuthToken:   "shared-token",
				PrincipalId: "guardian-pusher-docker-main",
				Role:        "pusher",
				DisplayName: "docker-main",
			},
		},
	} {
		resp, err := router.RegisterClient(context.Background(), req)
		if err != nil {
			t.Fatalf("RegisterClient(%s) error = %v", req.GetClientId(), err)
		}
		if !resp.GetSuccess() {
			t.Fatalf("RegisterClient(%s) failed: %+v", req.GetClientId(), resp)
		}
	}

	principal, ok := router.authenticateGuardianMutation("shared-token", &pb.GuardianMutationContext{PrincipalId: "guardianctl"})
	if !ok {
		t.Fatal("expected guardianctl principal to authenticate")
	}
	if principal.PrincipalID != "guardianctl" || principal.Role != "cli" {
		t.Fatalf("authenticateGuardianMutation(cli) = %+v, want guardianctl cli", principal)
	}

	principal, ok = router.authenticateGuardianMutation("shared-token", &pb.GuardianMutationContext{PrincipalId: "guardian-pusher-docker-main"})
	if !ok {
		t.Fatal("expected pusher principal to authenticate")
	}
	if principal.PrincipalID != "guardian-pusher-docker-main" || principal.Role != "pusher" {
		t.Fatalf("authenticateGuardianMutation(pusher) = %+v, want guardian-pusher-docker-main pusher", principal)
	}
}

func TestAuthenticateGuardianMutationRejectsUnknownRequestedPrincipal(t *testing.T) {
	router := NewRouter(DefaultRouterConfig(), nil)
	router.guardianClients["guardian-cli"] = &guardianClientState{
		authToken:   "shared-token",
		principalID: "guardianctl",
		role:        "cli",
		displayName: "guardianctl",
	}
	router.guardianClients["docker-pusher"] = &guardianClientState{
		authToken:   "shared-token",
		principalID: "guardian-pusher-docker-main",
		role:        "pusher",
		displayName: "guardian-pusher-docker-main",
	}

	if principal, ok := router.authenticateGuardianMutation("shared-token", &pb.GuardianMutationContext{PrincipalId: "guardian"}); ok || principal != nil {
		t.Fatalf("authenticateGuardianMutation(unknown requested principal) = (%+v, %v), want (nil, false)", principal, ok)
	}
}

func TestAuthorizeGuardianMutationDoctorRole(t *testing.T) {
	principal := &guardianPrincipal{
		PrincipalID: "doctor-query",
		Role:        "doctor",
	}

	for _, logicalPath := range []string{
		"/doctor/v1/catalog/manifests/traces/default/2026-04-09/15/trace-1.json",
		"/partitions/doctor-system/catalog/manifests/traces/default/2026-04-09/15/trace-1.json",
	} {
		if err := authorizeGuardianMutation(principal, logicalPath, false); err != nil {
			t.Fatalf("authorizeGuardianMutation(%q) error = %v", logicalPath, err)
		}
	}

	if err := authorizeGuardianMutation(principal, "/partitions/genomics/intents/web.yaml", false); err == nil {
		t.Fatal("expected doctor principal to be rejected outside Doctor namespaces")
	}
}

func TestGuardianUpsertAddsDirHints(t *testing.T) {
	router, _, node, cleanup := newGuardianRouterTestHarness(t)
	defer cleanup()

	_, err := router.UpsertGuardianPaths(context.Background(), &pb.UpsertGuardianPathsRequest{
		GuardianToken: "secret-token",
		Writes: []*pb.GuardianPathWrite{{
			LogicalPath: "/partitions/genomics/intents/web.yaml",
			Content:     []byte("kind: Intent\n"),
		}},
		Context: &pb.GuardianMutationContext{
			Reason:        "dir hint test",
			CorrelationId: "corr-dir-hint",
		},
	})
	if err != nil {
		t.Fatalf("UpsertGuardianPaths() error = %v", err)
	}

	node.mu.Lock()
	defer node.mu.Unlock()

	if len(node.lastIngestBatch) != 2 {
		t.Fatalf("last ingest batch size = %d, want 2", len(node.lastIngestBatch))
	}
	if node.lastIngestBatch[1].GetBackendMetadata()["dir_hint"] != "true" {
		t.Fatalf("expected dir hint in second batch entry, got %#v", node.lastIngestBatch[1].GetBackendMetadata())
	}
}

func TestGuardianManagedReposUseKVSBackend(t *testing.T) {
	router, _, node, cleanup := newGuardianRouterTestHarness(t)
	defer cleanup()

	tests := []struct {
		name        string
		logicalPath string
		content     string
		displayPath string
	}{
		{
			name:        "partition repo",
			logicalPath: "/partitions/genomics/intents/web.yaml",
			content:     "kind: Intent\n",
			displayPath: "guardian/genomics",
		},
		{
			name:        "guardian system repo",
			logicalPath: "/.queues/local/tasks/task-1.json",
			content:     "{\"id\":\"task-1\"}",
			displayPath: "guardian-system",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapped, err := mapGuardianLogicalPath(tt.logicalPath)
			if err != nil {
				t.Fatalf("mapGuardianLogicalPath() error = %v", err)
			}

			_, err = router.UpsertGuardianPaths(context.Background(), &pb.UpsertGuardianPathsRequest{
				GuardianToken: "secret-token",
				Writes: []*pb.GuardianPathWrite{{
					LogicalPath: tt.logicalPath,
					Content:     []byte(tt.content),
				}},
				Context: &pb.GuardianMutationContext{
					Reason:        "kvs backend test",
					CorrelationId: tt.name,
				},
			})
			if err != nil {
				t.Fatalf("UpsertGuardianPaths() error = %v", err)
			}

			node.mu.Lock()
			regReq := node.registerRequests[mapped.StorageID]
			batch := node.ingestBatches[mapped.StorageID]
			node.mu.Unlock()

			if regReq == nil {
				t.Fatalf("expected RegisterRepository request for storage_id %q", mapped.StorageID)
			}
			if regReq.GetDisplayPath() != tt.displayPath {
				t.Fatalf("register display path = %q, want %q", regReq.GetDisplayPath(), tt.displayPath)
			}
			if regReq.GetFetchConfig()["storage_backend"] != "kvs" {
				t.Fatalf("fetch config = %#v, want storage_backend=kvs", regReq.GetFetchConfig())
			}
			if regReq.GetIngestionConfig()["storage_backend"] != "kvs" {
				t.Fatalf("ingestion config = %#v, want storage_backend=kvs", regReq.GetIngestionConfig())
			}
			if len(batch) == 0 {
				t.Fatalf("expected ingest batch for storage_id %q", mapped.StorageID)
			}
			if batch[0].GetBackendMetadata()["storage_backend"] != "kvs" {
				t.Fatalf("file backend metadata = %#v, want storage_backend=kvs", batch[0].GetBackendMetadata())
			}
		})
	}
}

func TestGuardianManagedReposTargetKVSLeaderOnly(t *testing.T) {
	router, nodes, cleanup := newGuardianMultiNodeRouterTestHarness(t, map[string]*pb.KVSNodeStatus{
		"node-1": {Enabled: true, Healthy: true, Mode: "raft", Role: "follower", LeaderId: "node-2", PeerCount: 3},
		"node-2": {Enabled: true, Healthy: true, Mode: "raft", Role: "leader", LeaderId: "node-2", PeerCount: 3},
		"node-3": {Enabled: true, Healthy: true, Mode: "raft", Role: "follower", LeaderId: "node-2", PeerCount: 3},
	})
	defer cleanup()

	_, err := router.UpsertGuardianPaths(context.Background(), &pb.UpsertGuardianPathsRequest{
		GuardianToken: "secret-token",
		Writes: []*pb.GuardianPathWrite{{
			LogicalPath: "/partitions/genomics/intents/web.yaml",
			Content:     []byte("kind: Intent\n"),
		}},
		Context: &pb.GuardianMutationContext{
			Reason:        "kvs leader routing",
			CorrelationId: "leader-only-upsert",
		},
	})
	if err != nil {
		t.Fatalf("UpsertGuardianPaths() error = %v", err)
	}

	for nodeID, node := range nodes {
		node.mu.Lock()
		registerCalls := node.registerCalls
		ingestCalls := node.ingestBatchCalls
		_, hasFile := node.files["guardian/genomics/intents/web.yaml"]
		node.mu.Unlock()

		if nodeID == "node-2" {
			if registerCalls != 1 || ingestCalls != 1 {
				t.Fatalf("leader node calls = register:%d ingest:%d, want 1/1", registerCalls, ingestCalls)
			}
			if !hasFile {
				t.Fatal("expected leader node to receive guardian file")
			}
			continue
		}
		if registerCalls != 0 || ingestCalls != 0 {
			t.Fatalf("follower %s calls = register:%d ingest:%d, want 0/0", nodeID, registerCalls, ingestCalls)
		}
		if hasFile {
			t.Fatalf("follower %s unexpectedly stored guardian file", nodeID)
		}
	}
}

func TestGuardianManagedRepoDeletesTargetKVSLeaderOnly(t *testing.T) {
	router, nodes, cleanup := newGuardianMultiNodeRouterTestHarness(t, map[string]*pb.KVSNodeStatus{
		"node-1": {Enabled: true, Healthy: true, Mode: "raft", Role: "follower", LeaderId: "node-2", PeerCount: 3},
		"node-2": {Enabled: true, Healthy: true, Mode: "raft", Role: "leader", LeaderId: "node-2", PeerCount: 3},
		"node-3": {Enabled: true, Healthy: true, Mode: "raft", Role: "follower", LeaderId: "node-2", PeerCount: 3},
	})
	defer cleanup()

	writeResp, err := router.UpsertGuardianPaths(context.Background(), &pb.UpsertGuardianPathsRequest{
		GuardianToken: "secret-token",
		Writes: []*pb.GuardianPathWrite{{
			LogicalPath: "/partitions/genomics/intents/web.yaml",
			Content:     []byte("kind: Intent\n"),
		}},
		Context: &pb.GuardianMutationContext{
			Reason:        "kvs leader delete setup",
			CorrelationId: "leader-only-delete-setup",
		},
	})
	if err != nil {
		t.Fatalf("UpsertGuardianPaths() error = %v", err)
	}

	for _, node := range nodes {
		node.resetCallCounts()
	}

	_, err = router.DeleteGuardianPaths(context.Background(), &pb.DeleteGuardianPathsRequest{
		GuardianToken: "secret-token",
		Deletes: []*pb.GuardianPathDelete{{
			LogicalPath:       "/partitions/genomics/intents/web.yaml",
			ExpectedVersionId: writeResp.GetVersions()[0].GetVersionId(),
		}},
		Context: &pb.GuardianMutationContext{
			Reason:        "kvs leader delete",
			CorrelationId: "leader-only-delete",
		},
	})
	if err != nil {
		t.Fatalf("DeleteGuardianPaths() error = %v", err)
	}

	for nodeID, node := range nodes {
		node.mu.Lock()
		deleteFileCalls := node.deleteFileCalls
		node.mu.Unlock()

		if nodeID == "node-2" {
			if deleteFileCalls != 1 {
				t.Fatalf("leader delete calls = %d, want 1", deleteFileCalls)
			}
			continue
		}
		if deleteFileCalls != 0 {
			t.Fatalf("follower %s delete calls = %d, want 0", nodeID, deleteFileCalls)
		}
	}
}

func TestGuardianPathsMultiPartitionIsolation(t *testing.T) {
	router, nodeClient, node, cleanup := newGuardianRouterTestHarness(t)
	defer cleanup()

	ctx := context.Background()
	writes := []struct {
		logicalPath  string
		physicalPath string
		content      string
		displayPath  string
	}{
		{
			logicalPath:  "/partitions/genomics/intents/api.yaml",
			physicalPath: "guardian/genomics/intents/api.yaml",
			content:      "kind: Intent\nmetadata:\n  name: api\n",
			displayPath:  "guardian/genomics",
		},
		{
			logicalPath:  "/partitions/payments/intents/worker.yaml",
			physicalPath: "guardian/payments/intents/worker.yaml",
			content:      "kind: Intent\nmetadata:\n  name: worker\n",
			displayPath:  "guardian/payments",
		},
		{
			logicalPath:  "/.queues/local/tasks/task-42.json",
			physicalPath: "guardian-system/.queues/local/tasks/task-42.json",
			content:      "{\"id\":\"task-42\"}",
			displayPath:  "guardian-system",
		},
	}

	requestWrites := make([]*pb.GuardianPathWrite, 0, len(writes))
	storageIDs := make(map[string]string, len(writes))
	for _, write := range writes {
		mapped, err := mapGuardianLogicalPath(write.logicalPath)
		if err != nil {
			t.Fatalf("mapGuardianLogicalPath(%q) error = %v", write.logicalPath, err)
		}
		storageIDs[write.displayPath] = mapped.StorageID
		requestWrites = append(requestWrites, &pb.GuardianPathWrite{
			LogicalPath: write.logicalPath,
			Content:     []byte(write.content),
		})
	}

	writeResp, err := router.UpsertGuardianPaths(ctx, &pb.UpsertGuardianPathsRequest{
		GuardianToken: "secret-token",
		Writes:        requestWrites,
		Context: &pb.GuardianMutationContext{
			Reason:        "multi-partition write test",
			CorrelationId: "corr-multi-partition-write",
		},
	})
	if err != nil {
		t.Fatalf("UpsertGuardianPaths() error = %v", err)
	}
	if !writeResp.GetSuccess() || len(writeResp.GetVersions()) != len(writes) {
		t.Fatalf("unexpected upsert response: %+v", writeResp)
	}

	node.mu.Lock()
	if len(node.registerRequests) != len(writes) {
		node.mu.Unlock()
		t.Fatalf("register request count = %d, want %d", len(node.registerRequests), len(writes))
	}
	if len(node.ingestBatches) != len(writes) {
		node.mu.Unlock()
		t.Fatalf("ingest batch count = %d, want %d", len(node.ingestBatches), len(writes))
	}
	for _, write := range writes {
		storageID := storageIDs[write.displayPath]
		regReq := node.registerRequests[storageID]
		batch := node.ingestBatches[storageID]
		if regReq == nil {
			node.mu.Unlock()
			t.Fatalf("expected RegisterRepository request for %q", write.displayPath)
		}
		if regReq.GetDisplayPath() != write.displayPath {
			node.mu.Unlock()
			t.Fatalf("register display path = %q, want %q", regReq.GetDisplayPath(), write.displayPath)
		}
		if len(batch) == 0 {
			node.mu.Unlock()
			t.Fatalf("expected ingest batch for %q", write.displayPath)
		}
	}
	node.mu.Unlock()

	for _, write := range writes {
		attr, err := nodeClient.GetAttr(ctx, &pb.GetAttrRequest{Path: write.physicalPath})
		if err != nil {
			t.Fatalf("GetAttr(%q) error = %v", write.physicalPath, err)
		}
		if !attr.GetFound() {
			t.Fatalf("expected %q to be readable", write.physicalPath)
		}
		content := readAllFromMonoFSClient(t, nodeClient, write.physicalPath)
		if string(content) != write.content {
			t.Fatalf("content for %q = %q, want %q", write.physicalPath, string(content), write.content)
		}
	}

	deleteVersionID := ""
	for _, version := range writeResp.GetVersions() {
		if version.GetLogicalPath() == "/partitions/genomics/intents/api.yaml" {
			deleteVersionID = version.GetVersionId()
			break
		}
	}
	if deleteVersionID == "" {
		t.Fatal("expected genomics version id in upsert response")
	}

	deleteResp, err := router.DeleteGuardianPaths(ctx, &pb.DeleteGuardianPathsRequest{
		GuardianToken: "secret-token",
		Deletes: []*pb.GuardianPathDelete{{
			LogicalPath:       "/partitions/genomics/intents/api.yaml",
			ExpectedVersionId: deleteVersionID,
		}},
		Context: &pb.GuardianMutationContext{
			Reason:        "multi-partition delete test",
			CorrelationId: "corr-multi-partition-delete",
		},
	})
	if err != nil {
		t.Fatalf("DeleteGuardianPaths() error = %v", err)
	}
	if !deleteResp.GetSuccess() || len(deleteResp.GetTombstones()) != 1 {
		t.Fatalf("unexpected delete response: %+v", deleteResp)
	}

	deletedAttr, err := nodeClient.GetAttr(ctx, &pb.GetAttrRequest{Path: "guardian/genomics/intents/api.yaml"})
	if err != nil {
		t.Fatalf("GetAttr(deleted genomics file) error = %v", err)
	}
	if deletedAttr.GetFound() {
		t.Fatal("expected deleted genomics file to disappear")
	}

	for _, path := range []string{"guardian/payments/intents/worker.yaml", "guardian-system/.queues/local/tasks/task-42.json"} {
		attr, err := nodeClient.GetAttr(ctx, &pb.GetAttrRequest{Path: path})
		if err != nil {
			t.Fatalf("GetAttr(%q) after delete error = %v", path, err)
		}
		if !attr.GetFound() {
			t.Fatalf("expected %q to remain after unrelated partition delete", path)
		}
	}

	paymentsVersions, err := router.ListGuardianVersions(ctx, &pb.ListGuardianVersionsRequest{
		GuardianToken: "secret-token",
		LogicalPath:   "/partitions/payments/intents/worker.yaml",
	})
	if err != nil {
		t.Fatalf("ListGuardianVersions(payments) error = %v", err)
	}
	if len(paymentsVersions.GetVersions()) != 1 || paymentsVersions.GetVersions()[0].GetTombstone() {
		t.Fatalf("unexpected payments versions after genomics delete: %+v", paymentsVersions)
	}

	queueVersions, err := router.ListGuardianVersions(ctx, &pb.ListGuardianVersionsRequest{
		GuardianToken: "secret-token",
		LogicalPath:   "/.queues/local/tasks/task-42.json",
	})
	if err != nil {
		t.Fatalf("ListGuardianVersions(queue) error = %v", err)
	}
	if len(queueVersions.GetVersions()) != 1 || queueVersions.GetVersions()[0].GetTombstone() {
		t.Fatalf("unexpected queue versions after genomics delete: %+v", queueVersions)
	}
}

func TestGuardianPathsPartitionIsolationBetweenRepositories(t *testing.T) {
	router, nodeClient, _, cleanup := newGuardianRouterTestHarness(t)
	defer cleanup()

	ctx := context.Background()
	writeResp, err := router.UpsertGuardianPaths(ctx, &pb.UpsertGuardianPathsRequest{
		GuardianToken: "secret-token",
		Writes: []*pb.GuardianPathWrite{
			{
				LogicalPath: "/partitions/genomics/intents/api.yaml",
				Content:     []byte("kind: Intent\nmetadata:\n  name: api\n"),
			},
			{
				LogicalPath: "/partitions/payments/intents/worker.yaml",
				Content:     []byte("kind: Intent\nmetadata:\n  name: worker\n"),
			},
		},
		Context: &pb.GuardianMutationContext{
			Reason:        "partition isolation test",
			CorrelationId: "corr-partition-isolation",
		},
	})
	if err != nil {
		t.Fatalf("UpsertGuardianPaths() error = %v", err)
	}
	if !writeResp.GetSuccess() || len(writeResp.GetVersions()) != 2 {
		t.Fatalf("unexpected upsert response: %+v", writeResp)
	}

	for path := range map[string]string{
		"guardian/genomics/intents/api.yaml":    "kind: Intent\nmetadata:\n  name: api\n",
		"guardian/payments/intents/worker.yaml": "kind: Intent\nmetadata:\n  name: worker\n",
	} {
		attr, err := nodeClient.GetAttr(ctx, &pb.GetAttrRequest{Path: path})
		if err != nil {
			t.Fatalf("GetAttr(%q) error = %v", path, err)
		}
		if !attr.GetFound() {
			t.Fatalf("expected %q to be readable", path)
		}
	}

	genomicsVersionID := guardianVersionIDForPath(t, writeResp.GetVersions(), "/partitions/genomics/intents/api.yaml")
	deleteResp, err := router.DeleteGuardianPaths(ctx, &pb.DeleteGuardianPathsRequest{
		GuardianToken: "secret-token",
		Deletes: []*pb.GuardianPathDelete{{
			LogicalPath:       "/partitions/genomics/intents/api.yaml",
			ExpectedVersionId: genomicsVersionID,
		}},
		Context: &pb.GuardianMutationContext{
			Reason:        "partition isolation delete",
			CorrelationId: "corr-partition-isolation-delete",
		},
	})
	if err != nil {
		t.Fatalf("DeleteGuardianPaths() error = %v", err)
	}
	if !deleteResp.GetSuccess() || len(deleteResp.GetTombstones()) != 1 {
		t.Fatalf("unexpected delete response: %+v", deleteResp)
	}

	deletedAttr, err := nodeClient.GetAttr(ctx, &pb.GetAttrRequest{Path: "guardian/genomics/intents/api.yaml"})
	if err != nil {
		t.Fatalf("GetAttr(deleted genomics file) error = %v", err)
	}
	if deletedAttr.GetFound() {
		t.Fatal("expected deleted genomics file to disappear")
	}

	paymentsAttr, err := nodeClient.GetAttr(ctx, &pb.GetAttrRequest{Path: "guardian/payments/intents/worker.yaml"})
	if err != nil {
		t.Fatalf("GetAttr(payments file) error = %v", err)
	}
	if !paymentsAttr.GetFound() {
		t.Fatal("expected payments file to remain after deleting genomics")
	}

	paymentsVersions, err := router.ListGuardianVersions(ctx, &pb.ListGuardianVersionsRequest{
		GuardianToken: "secret-token",
		LogicalPath:   "/partitions/payments/intents/worker.yaml",
	})
	if err != nil {
		t.Fatalf("ListGuardianVersions(payments) error = %v", err)
	}
	if len(paymentsVersions.GetVersions()) != 1 || paymentsVersions.GetVersions()[0].GetTombstone() {
		t.Fatalf("unexpected payments versions after genomics delete: %+v", paymentsVersions)
	}
}

func TestGuardianSystemStaysIsolatedFromPartitionDeletes(t *testing.T) {
	router, nodeClient, _, cleanup := newGuardianRouterTestHarness(t)
	defer cleanup()

	ctx := context.Background()
	writeResp, err := router.UpsertGuardianPaths(ctx, &pb.UpsertGuardianPathsRequest{
		GuardianToken: "secret-token",
		Writes: []*pb.GuardianPathWrite{
			{
				LogicalPath: "/partitions/genomics/intents/api.yaml",
				Content:     []byte("kind: Intent\nmetadata:\n  name: api\n"),
			},
			{
				LogicalPath: "/.queues/local/tasks/task-42.json",
				Content:     []byte("{\"id\":\"task-42\"}"),
			},
		},
		Context: &pb.GuardianMutationContext{
			Reason:        "guardian-system isolation test",
			CorrelationId: "corr-guardian-system-isolation",
		},
	})
	if err != nil {
		t.Fatalf("UpsertGuardianPaths() error = %v", err)
	}
	if !writeResp.GetSuccess() || len(writeResp.GetVersions()) != 2 {
		t.Fatalf("unexpected upsert response: %+v", writeResp)
	}

	queueContent := readAllFromMonoFSClient(t, nodeClient, "guardian-system/.queues/local/tasks/task-42.json")
	if string(queueContent) != "{\"id\":\"task-42\"}" {
		t.Fatalf("unexpected queue content = %q", string(queueContent))
	}

	genomicsVersionID := guardianVersionIDForPath(t, writeResp.GetVersions(), "/partitions/genomics/intents/api.yaml")
	deleteResp, err := router.DeleteGuardianPaths(ctx, &pb.DeleteGuardianPathsRequest{
		GuardianToken: "secret-token",
		Deletes: []*pb.GuardianPathDelete{{
			LogicalPath:       "/partitions/genomics/intents/api.yaml",
			ExpectedVersionId: genomicsVersionID,
		}},
		Context: &pb.GuardianMutationContext{
			Reason:        "guardian-system isolation delete",
			CorrelationId: "corr-guardian-system-isolation-delete",
		},
	})
	if err != nil {
		t.Fatalf("DeleteGuardianPaths() error = %v", err)
	}
	if !deleteResp.GetSuccess() || len(deleteResp.GetTombstones()) != 1 {
		t.Fatalf("unexpected delete response: %+v", deleteResp)
	}

	queueAttr, err := nodeClient.GetAttr(ctx, &pb.GetAttrRequest{Path: "guardian-system/.queues/local/tasks/task-42.json"})
	if err != nil {
		t.Fatalf("GetAttr(queue file) error = %v", err)
	}
	if !queueAttr.GetFound() {
		t.Fatal("expected guardian-system queue file to remain after partition delete")
	}

	queueVersions, err := router.ListGuardianVersions(ctx, &pb.ListGuardianVersionsRequest{
		GuardianToken: "secret-token",
		LogicalPath:   "/.queues/local/tasks/task-42.json",
	})
	if err != nil {
		t.Fatalf("ListGuardianVersions(queue) error = %v", err)
	}
	if len(queueVersions.GetVersions()) != 1 || queueVersions.GetVersions()[0].GetTombstone() {
		t.Fatalf("unexpected queue versions after partition delete: %+v", queueVersions)
	}
}

func guardianVersionIDForPath(t *testing.T, versions []*pb.GuardianFileVersion, logicalPath string) string {
	t.Helper()
	for _, version := range versions {
		if version.GetLogicalPath() == logicalPath {
			return version.GetVersionId()
		}
	}
	t.Fatalf("expected version for logical path %q", logicalPath)
	return ""
}

func newGuardianRouterTestHarness(t *testing.T) (*Router, pb.MonoFSClient, *guardianTestNodeServer, func()) {
	t.Helper()

	nodeClient, node, stopNode := newGuardianTestNodeClient(t)
	cfg := DefaultRouterConfig()
	cfg.GuardianStateDir = t.TempDir()
	router := NewRouter(cfg, nil)
	router.nodes["node-1"] = &nodeState{
		info: &pb.NodeInfo{
			NodeId:  "node-1",
			Address: "bufnet",
			Healthy: true,
			Weight:  1,
		},
		client: nodeClient,
		status: NodeActive,
	}

	registerResp, err := router.RegisterClient(context.Background(), &pb.RegisterClientRequest{
		ClientId: "guardian-cli",
		GuardianConfig: &pb.GuardianConfig{
			BaseUrl:   "http://guardian.local",
			AuthToken: "secret-token",
		},
	})
	if err != nil {
		t.Fatalf("RegisterClient() error = %v", err)
	}
	if !registerResp.GetSuccess() {
		t.Fatalf("RegisterClient() failed: %+v", registerResp)
	}

	return router, nodeClient, node, func() {
		_ = router.Close()
		stopNode()
	}
}

func newGuardianMultiNodeRouterTestHarness(t *testing.T, kvsStatuses map[string]*pb.KVSNodeStatus) (*Router, map[string]*guardianTestNodeServer, func()) {
	t.Helper()

	ids := make([]string, 0, len(kvsStatuses))
	for id := range kvsStatuses {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	cfg := DefaultRouterConfig()
	cfg.GuardianStateDir = t.TempDir()
	router := NewRouter(cfg, nil)
	nodes := make(map[string]*guardianTestNodeServer, len(ids))
	stops := make([]func(), 0, len(ids))

	for _, id := range ids {
		nodeClient, node, stopNode := newGuardianTestNodeClient(t)
		router.nodes[id] = &nodeState{
			info: &pb.NodeInfo{
				NodeId:  id,
				Address: "bufnet-" + id,
				Healthy: true,
				Weight:  1,
			},
			client:    nodeClient,
			kvsStatus: normalizedKVSNodeStatus(kvsStatuses[id]),
			status:    NodeActive,
		}
		nodes[id] = node
		stops = append(stops, stopNode)
	}

	registerResp, err := router.RegisterClient(context.Background(), &pb.RegisterClientRequest{
		ClientId: "guardian-cli",
		GuardianConfig: &pb.GuardianConfig{
			BaseUrl:   "http://guardian.local",
			AuthToken: "secret-token",
		},
	})
	if err != nil {
		t.Fatalf("RegisterClient() error = %v", err)
	}
	if !registerResp.GetSuccess() {
		t.Fatalf("RegisterClient() failed: %+v", registerResp)
	}

	return router, nodes, func() {
		_ = router.Close()
		for _, stop := range stops {
			stop()
		}
	}
}

type guardianTestNodeServer struct {
	pb.UnimplementedMonoFSServer
	mu               sync.Mutex
	repos            map[string]string
	registerRequests map[string]*pb.RegisterRepositoryRequest
	files            map[string][]byte
	fileMode         map[string]uint32
	ingestBatches    map[string][]*pb.FileMetadata
	lastIngestBatch  []*pb.FileMetadata
	registerCalls    int
	ingestBatchCalls int
	deleteFileCalls  int
	deleteDirCalls   int
	deleteRepoCalls  int
}

func newGuardianTestNodeClient(t *testing.T) (pb.MonoFSClient, *guardianTestNodeServer, func()) {
	t.Helper()

	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	node := &guardianTestNodeServer{
		repos:            make(map[string]string),
		registerRequests: make(map[string]*pb.RegisterRepositoryRequest),
		files:            make(map[string][]byte),
		fileMode:         make(map[string]uint32),
		ingestBatches:    make(map[string][]*pb.FileMetadata),
	}
	pb.RegisterMonoFSServer(server, node)
	go func() {
		_ = server.Serve(listener)
	}()

	dialer := func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}
	conn, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}

	return pb.NewMonoFSClient(conn), node, func() {
		_ = conn.Close()
		server.Stop()
		_ = listener.Close()
	}
}

func (s *guardianTestNodeServer) RegisterRepository(_ context.Context, req *pb.RegisterRepositoryRequest) (*pb.RegisterRepositoryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registerCalls++
	s.repos[req.GetStorageId()] = req.GetDisplayPath()
	s.registerRequests[req.GetStorageId()] = &pb.RegisterRepositoryRequest{
		StorageId:       req.GetStorageId(),
		DisplayPath:     req.GetDisplayPath(),
		Source:          req.GetSource(),
		IngestionType:   req.GetIngestionType(),
		FetchType:       req.GetFetchType(),
		FetchConfig:     cloneStringMap(req.GetFetchConfig()),
		IngestionConfig: cloneStringMap(req.GetIngestionConfig()),
		GuardianUrl:     req.GetGuardianUrl(),
	}
	return &pb.RegisterRepositoryResponse{Success: true, Message: "ok"}, nil
}

func (s *guardianTestNodeServer) IngestFileBatch(_ context.Context, req *pb.IngestFileBatchRequest) (*pb.IngestFileBatchResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ingestBatchCalls++
	batchCopy := cloneFileMetadataSlice(req.GetFiles())
	s.lastIngestBatch = batchCopy
	s.ingestBatches[req.GetStorageId()] = batchCopy
	for _, file := range req.GetFiles() {
		if file.GetBackendMetadata()["dir_hint"] == "true" {
			continue
		}
		fullPath := guardianDisplayPathJoin(req.GetDisplayPath(), cleanGuardianRelativePath(file.GetPath()))
		s.files[fullPath] = append([]byte(nil), file.GetInlineContent()...)
		s.fileMode[fullPath] = 0o644 | uint32(syscall.S_IFREG)
	}
	s.repos[req.GetStorageId()] = req.GetDisplayPath()
	return &pb.IngestFileBatchResponse{
		Success:       true,
		FilesIngested: int64(len(req.GetFiles())),
	}, nil
}

func (s *guardianTestNodeServer) GetAttr(_ context.Context, req *pb.GetAttrRequest) (*pb.GetAttrResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if content, ok := s.files[req.GetPath()]; ok {
		return &pb.GetAttrResponse{
			Found: true,
			Mode:  s.fileMode[req.GetPath()],
			Size:  uint64(len(content)),
		}, nil
	}
	if s.hasDirectoryLocked(req.GetPath()) {
		return &pb.GetAttrResponse{
			Found: true,
			Mode:  0o755 | uint32(syscall.S_IFDIR),
		}, nil
	}
	return &pb.GetAttrResponse{Found: false}, nil
}

func (s *guardianTestNodeServer) Read(req *pb.ReadRequest, stream grpc.ServerStreamingServer[pb.DataChunk]) error {
	s.mu.Lock()
	content, ok := s.files[req.GetPath()]
	s.mu.Unlock()
	if !ok {
		return status.Error(codes.NotFound, "file not found")
	}

	offset := req.GetOffset()
	if offset > int64(len(content)) {
		return nil
	}
	content = content[offset:]
	if size := req.GetSize(); size > 0 && size < int64(len(content)) {
		content = content[:size]
	}
	return stream.Send(&pb.DataChunk{
		Data:   append([]byte(nil), content...),
		Offset: offset,
	})
}

func (s *guardianTestNodeServer) DeleteFile(_ context.Context, req *pb.DeleteFileRequest) (*pb.DeleteFileResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteFileCalls++

	displayPath, ok := s.repos[req.GetStorageId()]
	if !ok {
		return &pb.DeleteFileResponse{Success: false, Message: "unknown storage id"}, nil
	}
	fullPath := guardianDisplayPathJoin(displayPath, cleanGuardianRelativePath(req.GetFilePath()))
	delete(s.files, fullPath)
	delete(s.fileMode, fullPath)
	return &pb.DeleteFileResponse{Success: true, Message: "deleted"}, nil
}

func (s *guardianTestNodeServer) DeleteDirectoryRecursive(_ context.Context, req *pb.DeleteDirectoryRecursiveRequest) (*pb.DeleteDirectoryRecursiveResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteDirCalls++

	displayPath, ok := s.repos[req.GetStorageId()]
	if !ok {
		return &pb.DeleteDirectoryRecursiveResponse{Success: false, Message: "unknown storage id"}, nil
	}
	prefix := guardianDisplayPathJoin(displayPath, cleanGuardianRelativePath(req.GetDirPath()))
	if prefix != "" {
		prefix += "/"
	}

	var filesDeleted int64
	for path := range s.files {
		if strings.HasPrefix(path, prefix) || path == strings.TrimSuffix(prefix, "/") {
			delete(s.files, path)
			delete(s.fileMode, path)
			filesDeleted++
		}
	}
	return &pb.DeleteDirectoryRecursiveResponse{
		Success:      true,
		Message:      "deleted",
		FilesDeleted: filesDeleted,
		DirsDeleted:  1,
	}, nil
}

func (s *guardianTestNodeServer) DeleteRepository(_ context.Context, req *pb.DeleteRepositoryOnNodeRequest) (*pb.DeleteRepositoryOnNodeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteRepoCalls++

	displayPath, ok := s.repos[req.GetStorageId()]
	if !ok {
		return &pb.DeleteRepositoryOnNodeResponse{Success: true, Message: "nothing to delete"}, nil
	}

	var filesDeleted int64
	for path := range s.files {
		if path == displayPath || strings.HasPrefix(path, displayPath+"/") {
			delete(s.files, path)
			delete(s.fileMode, path)
			filesDeleted++
		}
	}
	delete(s.repos, req.GetStorageId())
	return &pb.DeleteRepositoryOnNodeResponse{
		Success:      true,
		Message:      "deleted",
		FilesDeleted: filesDeleted,
		DirsDeleted:  1,
	}, nil
}

func (s *guardianTestNodeServer) resetCallCounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registerCalls = 0
	s.ingestBatchCalls = 0
	s.deleteFileCalls = 0
	s.deleteDirCalls = 0
	s.deleteRepoCalls = 0
}

func (s *guardianTestNodeServer) hasDirectoryLocked(path string) bool {
	if path == "" {
		return true
	}
	for _, repoPath := range s.repos {
		if repoPath == path || strings.HasPrefix(repoPath, path+"/") {
			return true
		}
	}
	prefix := path + "/"
	for filePath := range s.files {
		if strings.HasPrefix(filePath, prefix) {
			return true
		}
	}
	return false
}

func readAllFromMonoFSClient(t *testing.T, client pb.MonoFSClient, path string) []byte {
	t.Helper()

	stream, err := client.Read(context.Background(), &pb.ReadRequest{
		Path:   path,
		Offset: 0,
		Size:   0,
	})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	var out []byte
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, context.Canceled) {
			t.Fatalf("Read() canceled unexpectedly: %v", err)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("Read().Recv() error = %v", err)
		}
		out = append(out, chunk.GetData()...)
	}
	return out
}

type mockGuardianLogicalChangeStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	events []*pb.GuardianChangeEvent
}

func newMockGuardianLogicalChangeStream() *mockGuardianLogicalChangeStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &mockGuardianLogicalChangeStream{
		ctx:    ctx,
		cancel: cancel,
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func cloneFileMetadataSlice(files []*pb.FileMetadata) []*pb.FileMetadata {
	if len(files) == 0 {
		return nil
	}
	cloned := make([]*pb.FileMetadata, 0, len(files))
	for _, file := range files {
		if file == nil {
			cloned = append(cloned, nil)
			continue
		}
		copyFile := *file
		copyFile.InlineContent = append([]byte(nil), file.GetInlineContent()...)
		copyFile.BackendMetadata = cloneStringMap(file.GetBackendMetadata())
		cloned = append(cloned, &copyFile)
	}
	return cloned
}

func (m *mockGuardianLogicalChangeStream) SetHeader(metadata.MD) error { return nil }

func (m *mockGuardianLogicalChangeStream) SendHeader(metadata.MD) error { return nil }

func (m *mockGuardianLogicalChangeStream) SetTrailer(metadata.MD) {}

func (m *mockGuardianLogicalChangeStream) Context() context.Context { return m.ctx }

func (m *mockGuardianLogicalChangeStream) SendMsg(any) error { return nil }

func (m *mockGuardianLogicalChangeStream) RecvMsg(any) error { return nil }

func (m *mockGuardianLogicalChangeStream) Send(event *pb.GuardianChangeEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, cloneGuardianLogicalChangeEvent(event))
	return nil
}

func (m *mockGuardianLogicalChangeStream) Events() []*pb.GuardianChangeEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*pb.GuardianChangeEvent, len(m.events))
	copy(result, m.events)
	return result
}

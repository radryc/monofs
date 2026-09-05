package router

import (
	"context"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestSubscribeToChangesRejectsUnknownClient(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), nil)
	stream := newMockChangeStream()

	err := r.SubscribeToChanges(&pb.SubscribeChangesRequest{
		StorageId: "storage-1",
		ClientId:  "missing-client",
	}, stream)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected permission denied, got %v", err)
	}
}

func TestSubscribeToChangesReceivesMatchingEvents(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), nil)
	r.guardianClients["guardian-test"] = &guardianClientState{
		clientID:  "guardian-test",
		authToken: "secret",
	}

	stream := newMockChangeStream()
	errCh := make(chan error, 1)
	go func() {
		errCh <- r.SubscribeToChanges(&pb.SubscribeChangesRequest{
			StorageId:    "storage-1",
			PathPrefixes: []string{"intents"},
			ClientId:     "guardian-test",
		}, stream)
	}()

	waitForCondition(t, func() bool {
		r.guardianChangeSubsMu.RLock()
		defer r.guardianChangeSubsMu.RUnlock()
		return len(r.guardianChangeSubs) == 1
	})

	r.publishGuardianChange(&pb.ChangeEvent{
		StorageId: "storage-1",
		FilePath:  "config.yaml",
		Type:      pb.ChangeType_MODIFIED,
	})
	r.publishGuardianChange(&pb.ChangeEvent{
		StorageId: "storage-1",
		FilePath:  "intents/web.yaml",
		Type:      pb.ChangeType_MODIFIED,
	})

	waitForCondition(t, func() bool {
		return len(stream.Events()) == 1
	})

	stream.cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("subscribe returned error: %v", err)
	}

	events := stream.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].FilePath != "intents/web.yaml" {
		t.Fatalf("expected intents/web.yaml event, got %q", events[0].FilePath)
	}
}

func TestMatchesGuardianPrefixesIncludesAncestorDeletes(t *testing.T) {
	if !matchesGuardianPrefixes(".queues", []string{".queues/pusher-a"}) {
		t.Fatal("expected parent directory delete to match child prefix watcher")
	}
	if !matchesGuardianPrefixes("", []string{"intents"}) {
		t.Fatal("expected root delete to match all subscribers")
	}
	if matchesGuardianPrefixes("config.yaml", []string{"intents"}) {
		t.Fatal("did not expect unrelated path to match")
	}
}

type mockChangeStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	events []*pb.ChangeEvent
}

func newMockChangeStream() *mockChangeStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &mockChangeStream{
		ctx:    ctx,
		cancel: cancel,
	}
}

func (m *mockChangeStream) SetHeader(metadata.MD) error { return nil }

func (m *mockChangeStream) SendHeader(metadata.MD) error { return nil }

func (m *mockChangeStream) SetTrailer(metadata.MD) {}

func (m *mockChangeStream) Context() context.Context { return m.ctx }

func (m *mockChangeStream) SendMsg(any) error { return nil }

func (m *mockChangeStream) RecvMsg(any) error { return nil }

func (m *mockChangeStream) Send(event *pb.ChangeEvent) error {
	m.events = append(m.events, cloneGuardianChangeEvent(event))
	return nil
}

func (m *mockChangeStream) Events() []*pb.ChangeEvent {
	result := make([]*pb.ChangeEvent, len(m.events))
	copy(result, m.events)
	return result
}

func waitForCondition(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

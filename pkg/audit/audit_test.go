package audit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type mockSink struct {
	mu      sync.Mutex
	batches [][]Event
	events  []Event
	err     error
}

func (m *mockSink) WriteBatch(_ context.Context, events []Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	cp := make([]Event, len(events))
	copy(cp, events)
	m.batches = append(m.batches, cp)
	m.events = append(m.events, cp...)
	return nil
}

func (m *mockSink) snapshot() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]Event, len(m.events))
	copy(cp, m.events)
	return cp
}

func TestWALSpoolEmitterHashChainAndRestart(t *testing.T) {
	ctx := context.Background()
	walPath := filepath.Join(t.TempDir(), "audit.wal")

	emitter1, err := NewWALSpoolEmitter(walPath, WithFailClosed(true))
	if err != nil {
		t.Fatalf("NewWALSpoolEmitter (1): %v", err)
	}

	firstBatch := []Event{
		{
			ActorSubject:  "alice",
			ActorClientID: "cli-1",
			Action:        ActionAuth,
			Resource:      "/auth/login",
			Decision:      DecisionAllow,
			CorrelationID: "corr-1",
			SourceIP:      "10.0.0.1",
		},
		{
			ActorSubject: "alice",
			Action:       ActionRead,
			Resource:     "/repos/core/README.md",
			Partition:    "p0",
			Decision:     DecisionAllow,
		},
	}
	for _, ev := range firstBatch {
		if err := emitter1.Emit(ctx, ev); err != nil {
			t.Fatalf("Emit before restart: %v", err)
		}
	}
	if err := emitter1.Close(); err != nil {
		t.Fatalf("Close emitter1: %v", err)
	}

	// Reopen on the same WAL and emit more events.
	emitter2, err := NewWALSpoolEmitter(walPath, WithFailClosed(true))
	if err != nil {
		t.Fatalf("NewWALSpoolEmitter (2): %v", err)
	}
	defer emitter2.Close()

	secondBatch := []Event{
		{
			ActorSubject: "bob",
			Action:       ActionWrite,
			Resource:     "/repos/core/main.go",
			Decision:     DecisionAllow,
		},
		{
			ActorSubject: "eve",
			Action:       ActionDelete,
			Resource:     "/repos/core/secrets.yaml",
			Decision:     DecisionDeny,
		},
		{
			ActorSubject: "admin",
			Action:       ActionAdmin,
			Resource:     "/admin/policy",
			Decision:     DecisionAllow,
		},
	}
	for _, ev := range secondBatch {
		if err := emitter2.Emit(ctx, ev); err != nil {
			t.Fatalf("Emit after restart: %v", err)
		}
	}

	count, err := VerifyChain(walPath)
	if err != nil {
		t.Fatalf("VerifyChain failed: %v", err)
	}
	wantCount := len(firstBatch) + len(secondBatch)
	if count != wantCount {
		t.Fatalf("VerifyChain count = %d, want %d", count, wantCount)
	}
}

func TestVerifyChainDetectsTampering(t *testing.T) {
	ctx := context.Background()
	walPath := filepath.Join(t.TempDir(), "audit.wal")

	emitter, err := NewWALSpoolEmitter(walPath, WithFailClosed(true))
	if err != nil {
		t.Fatalf("NewWALSpoolEmitter: %v", err)
	}

	for i := 0; i < 4; i++ {
		err := emitter.Emit(ctx, Event{
			ActorSubject: "user",
			Action:       ActionRead,
			Resource:     "/repos/app/file",
			Decision:     DecisionAllow,
		})
		if err != nil {
			t.Fatalf("Emit %d: %v", i, err)
		}
	}
	if err := emitter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Tamper with the second event's Resource and Decision.
	raw, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d", len(lines))
	}

	var tampered Event
	if err := json.Unmarshal([]byte(lines[1]), &tampered); err != nil {
		t.Fatalf("Unmarshal line 2: %v", err)
	}
	tampered.Resource = "/repos/app/tampered"
	tampered.Decision = DecisionDeny
	tamperedBytes, err := json.Marshal(tampered)
	if err != nil {
		t.Fatalf("Marshal tampered: %v", err)
	}
	lines[1] = string(tamperedBytes)

	if err := os.WriteFile(walPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile tampered: %v", err)
	}

	_, err = VerifyChain(walPath)
	if err == nil {
		t.Fatal("VerifyChain succeeded on tampered WAL, want error")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("VerifyChain error %q does not identify tampered line 2", err.Error())
	}
}

func TestWALSpoolEmitterFlushToSink(t *testing.T) {
	ctx := context.Background()
	walPath := filepath.Join(t.TempDir(), "audit.wal")

	emitter, err := NewWALSpoolEmitter(walPath, WithFailClosed(true))
	if err != nil {
		t.Fatalf("NewWALSpoolEmitter: %v", err)
	}
	defer emitter.Close()

	for i := 0; i < 3; i++ {
		if err := emitter.Emit(ctx, Event{
			ActorSubject: "alice",
			Action:       ActionWrite,
			Resource:     "/repos/a",
			Decision:     DecisionAllow,
		}); err != nil {
			t.Fatalf("Emit %d: %v", i, err)
		}
	}

	sink := &mockSink{}
	n, err := emitter.Flush(ctx, sink)
	if err != nil {
		t.Fatalf("Flush 1: %v", err)
	}
	if n != 3 {
		t.Fatalf("Flush 1 count = %d, want 3", n)
	}
	offset1 := emitter.FlushedOffset()
	if offset1 <= 0 {
		t.Fatalf("FlushedOffset after first flush = %d, want > 0", offset1)
	}
	if got := len(sink.snapshot()); got != 3 {
		t.Fatalf("sink event count = %d, want 3", got)
	}

	// Subsequent Flush with no new events should deliver 0 and not duplicate.
	n, err = emitter.Flush(ctx, sink)
	if err != nil {
		t.Fatalf("Flush 2: %v", err)
	}
	if n != 0 {
		t.Fatalf("Flush 2 count = %d, want 0", n)
	}
	if emitter.FlushedOffset() != offset1 {
		t.Fatalf("FlushedOffset changed from %d to %d on empty flush", offset1, emitter.FlushedOffset())
	}
	if got := len(sink.snapshot()); got != 3 {
		t.Fatalf("sink event count after empty flush = %d, want 3", got)
	}

	// Emit one more event and verify incremental Flush delivers only the new one.
	if err := emitter.Emit(ctx, Event{
		ActorSubject: "bob",
		Action:       ActionRead,
		Resource:     "/repos/b",
		Decision:     DecisionDeny,
	}); err != nil {
		t.Fatalf("Emit 4: %v", err)
	}

	n, err = emitter.Flush(ctx, sink)
	if err != nil {
		t.Fatalf("Flush 3: %v", err)
	}
	if n != 1 {
		t.Fatalf("Flush 3 count = %d, want 1", n)
	}
	if emitter.FlushedOffset() <= offset1 {
		t.Fatalf("FlushedOffset = %d, want > %d", emitter.FlushedOffset(), offset1)
	}
	if got := len(sink.snapshot()); got != 4 {
		t.Fatalf("sink event count after incremental flush = %d, want 4", got)
	}
}

func TestWALSpoolEmitterFailClosed(t *testing.T) {
	ctx := context.Background()
	// Point to a non-existent parent directory so WAL creation fails on Emit.
	unwritablePath := filepath.Join(t.TempDir(), "missing-dir", "audit.wal")

	emitter, err := NewWALSpoolEmitter(unwritablePath, WithFailClosed(true))
	if err != nil {
		t.Fatalf("NewWALSpoolEmitter: %v", err)
	}
	defer emitter.Close()

	err = emitter.Emit(ctx, Event{
		ActorSubject: "alice",
		Action:       ActionRead,
		Resource:     "/repos/secret",
		Decision:     DecisionAllow,
	})
	if err == nil {
		t.Fatal("Emit succeeded on unwritable WAL with FailClosed=true, want error")
	}
}

func TestWALSpoolEmitterFailOpen(t *testing.T) {
	ctx := context.Background()
	unwritablePath := filepath.Join(t.TempDir(), "missing-dir", "audit.wal")

	var gotErr error
	emitter, err := NewWALSpoolEmitter(
		unwritablePath,
		WithFailClosed(false),
		WithOnError(func(err error) { gotErr = err }),
	)
	if err != nil {
		t.Fatalf("NewWALSpoolEmitter: %v", err)
	}
	defer emitter.Close()

	err = emitter.Emit(ctx, Event{
		ActorSubject: "alice",
		Action:       ActionRead,
		Resource:     "/repos/secret",
		Decision:     DecisionAllow,
	})
	if err != nil {
		t.Fatalf("Emit with FailClosed=false returned error: %v", err)
	}
	if gotErr == nil {
		t.Fatal("OnError callback was not invoked")
	}
	if emitter.DroppedCount() != 1 {
		t.Fatalf("DroppedCount = %d, want 1", emitter.DroppedCount())
	}
}

func TestWALSpoolEmitterFlushRetryOnSinkError(t *testing.T) {
	ctx := context.Background()
	walPath := filepath.Join(t.TempDir(), "audit.wal")

	emitter, err := NewWALSpoolEmitter(walPath, WithFailClosed(true))
	if err != nil {
		t.Fatalf("NewWALSpoolEmitter: %v", err)
	}
	defer emitter.Close()

	if err := emitter.Emit(ctx, Event{
		ActorSubject: "alice",
		Action:       ActionRead,
		Resource:     "/repos/a",
		Decision:     DecisionAllow,
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	sink := &mockSink{err: errors.New("sink unavailable")}
	if _, err := emitter.Flush(ctx, sink); err == nil {
		t.Fatal("Flush succeeded when sink returned error")
	}
	if emitter.FlushedOffset() != 0 {
		t.Fatalf("FlushedOffset = %d, want 0 after failed flush", emitter.FlushedOffset())
	}

	sink.mu.Lock()
	sink.err = nil
	sink.mu.Unlock()

	n, err := emitter.Flush(ctx, sink)
	if err != nil {
		t.Fatalf("Flush retry: %v", err)
	}
	if n != 1 {
		t.Fatalf("Flush retry count = %d, want 1", n)
	}
}

func TestWALSpoolEmitterConcurrentEmit(t *testing.T) {
	ctx := context.Background()
	walPath := filepath.Join(t.TempDir(), "audit.wal")

	emitter, err := NewWALSpoolEmitter(walPath, WithFailClosed(true))
	if err != nil {
		t.Fatalf("NewWALSpoolEmitter: %v", err)
	}
	defer emitter.Close()

	const workers = 8
	const perWorker = 10
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if err := emitter.Emit(ctx, Event{
					ActorSubject: "worker",
					Action:       ActionRead,
					Resource:     "/repos/concurrent",
					Decision:     DecisionAllow,
				}); err != nil {
					t.Errorf("concurrent Emit: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	count, err := VerifyChain(walPath)
	if err != nil {
		t.Fatalf("VerifyChain after concurrent emits: %v", err)
	}
	if count != workers*perWorker {
		t.Fatalf("VerifyChain count = %d, want %d", count, workers*perWorker)
	}
}

package router

import (
	"testing"
	"time"
)

// TestInternalDialOptionsWhileHoldingLock guards the regression that took the
// router down: health checks and ingestion dial nodes while holding r.mu's
// write lock, so internalDialOptions must not acquire the same lock (RWMutex
// is not reentrant and self-deadlocks).
func TestInternalDialOptionsWhileHoldingLock(t *testing.T) {
	r := &Router{}
	r.internalTokenValue.Store("test-token")

	r.mu.Lock()
	done := make(chan []struct{}, 1)
	go func() {
		// Only the token read matters; converting to a count keeps the test
		// independent of grpc option internals.
		opts := r.internalDialOptions()
		done <- make([]struct{}, len(opts))
	}()
	select {
	case opts := <-done:
		if len(opts) == 0 {
			t.Fatal("expected dial options for a configured token")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("internalDialOptions deadlocked while the caller held r.mu")
	}
	r.mu.Unlock()
}

func TestInternalDialOptionsEmptyToken(t *testing.T) {
	r := &Router{}
	if opts := r.internalDialOptions(); opts != nil {
		t.Fatalf("expected nil options without a token, got %d", len(opts))
	}
}

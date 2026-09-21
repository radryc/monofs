package protos

import (
	"testing"
)

// TestMessagePoolsReuse verifies that pooled messages come back reset
// (zero-valued), whether the pooled object is reused or a fresh one is
// allocated. sync.Pool does not guarantee object identity across a GC cycle,
// so the identity is not asserted.
func TestMessagePoolsReuse(t *testing.T) {
	pools := NewMessagePools()

	// Get a ReadRequest
	req1 := pools.GetReadRequest()
	req1.Path = "/test/path"

	// Put it back
	pools.PutReadRequest(req1)

	// Get another ReadRequest - must be reset to the zero value (either the
	// reused pooled object or a freshly allocated one).
	req2 := pools.GetReadRequest()
	if req2.Path != "" {
		t.Errorf("pool reset failed: expected empty path, got %q", req2.Path)
	}

	pools.PutReadRequest(req2)
}

// TestMessagePoolsNilSafety verifies that putting nil messages is safe.
func TestMessagePoolsNilSafety(t *testing.T) {
	pools := NewMessagePools()

	// These should not panic
	pools.PutReadRequest(nil)
	pools.PutWriteRequest(nil)
	pools.PutWriteResponse(nil)
	pools.PutLookupRequest(nil)
	pools.PutLookupResponse(nil)
	pools.PutReadDirRequest(nil)
	pools.PutDirEntry(nil)
	pools.PutDataChunk(nil)
}

// TestAllMessageTypes verifies all message pool types work correctly.
func TestAllMessageTypes(t *testing.T) {
	pools := NewMessagePools()

	tests := []struct {
		name string
		test func()
	}{
		{
			name: "ReadRequest",
			test: func() {
				msg := pools.GetReadRequest()
				msg.Path = "/test"
				pools.PutReadRequest(msg)
			},
		},
		{
			name: "WriteRequest",
			test: func() {
				msg := pools.GetWriteRequest()
				msg.Data = []byte("test")
				pools.PutWriteRequest(msg)
			},
		},
		{
			name: "WriteResponse",
			test: func() {
				msg := pools.GetWriteResponse()
				msg.Size = 100
				pools.PutWriteResponse(msg)
			},
		},
		{
			name: "LookupRequest",
			test: func() {
				msg := pools.GetLookupRequest()
				msg.Name = "test"
				pools.PutLookupRequest(msg)
			},
		},
		{
			name: "LookupResponse",
			test: func() {
				msg := pools.GetLookupResponse()
				msg.Found = true
				pools.PutLookupResponse(msg)
			},
		},
		{
			name: "ReadDirRequest",
			test: func() {
				msg := pools.GetReadDirRequest()
				msg.Path = "/test"
				pools.PutReadDirRequest(msg)
			},
		},
		{
			name: "DirEntry",
			test: func() {
				msg := pools.GetDirEntry()
				msg.Name = "test"
				pools.PutDirEntry(msg)
			},
		},
		{
			name: "DataChunk",
			test: func() {
				msg := pools.GetDataChunk()
				msg.Data = []byte("test")
				pools.PutDataChunk(msg)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.test()
		})
	}
}

// TestMessagePoolsConcurrency verifies pools are safe for concurrent use.
func TestMessagePoolsConcurrency(t *testing.T) {
	pools := NewMessagePools()
	done := make(chan bool)

	// Run multiple goroutines using the pools
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				req := pools.GetReadRequest()
				req.Path = "/test"
				pools.PutReadRequest(req)
			}
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}
}

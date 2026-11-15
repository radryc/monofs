package fuse

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSessionSocketActionTableCoversAllVerbs(t *testing.T) {
	handler := &SessionSocketHandler{logger: testLogger()}

	verbs := []string{
		"start", "add", "rm", "status", "branch", "refs", "log", "tags",
		"blame", "commit", "pull", "refresh", "discard", "push",
		"conflicts", "push-blobs", "blobs-info", "diff",
	}
	for _, verb := range verbs {
		if _, ok := handler.resolveAction(verb); !ok {
			t.Errorf("action %q is missing from the command table", verb)
		}
	}
	if _, ok := handler.resolveAction("remove"); ok {
		t.Error("unknown verb \"remove\" resolved unexpectedly")
	}
	if _, ok := handler.resolveAction(""); ok {
		t.Error("empty verb resolved unexpectedly")
	}
}

func TestSessionSocketActionTableIsBuiltOnce(t *testing.T) {
	handler := &SessionSocketHandler{logger: testLogger()}
	if _, ok := handler.resolveAction("status"); !ok {
		t.Fatal("status missing")
	}
	// Once built, the table must not be rebuilt: a replacement map is used
	// verbatim by subsequent lookups.
	handler.actions = map[string]sessionAction{
		"custom": func(*SessionRequest) SessionResponse { return SessionResponse{Success: true} },
	}
	if _, ok := handler.resolveAction("custom"); !ok {
		t.Fatal("replacement table was not used")
	}
	if _, ok := handler.resolveAction("status"); ok {
		t.Fatal("action table was rebuilt after being replaced")
	}
}

func TestSessionSocketResolveActionConcurrent(t *testing.T) {
	handler := &SessionSocketHandler{logger: testLogger()}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := handler.resolveAction("status"); !ok {
				t.Error("status missing under concurrent access")
			}
		}()
	}
	wg.Wait()
}

func serveOneRequest(t *testing.T, handler *SessionSocketHandler, request string) SessionResponse {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	handler.wg.Add(1)
	go handler.handleConnection(serverConn)

	if _, err := clientConn.Write([]byte(request)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	var response SessionResponse
	if err := json.NewDecoder(clientConn).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response
}

func TestSessionSocketHandleConnectionUnknownAction(t *testing.T) {
	handler := &SessionSocketHandler{logger: testLogger()}
	response := serveOneRequest(t, handler, `{"action":"does-not-exist"}`)
	if response.Success {
		t.Fatal("unknown action reported success")
	}
	if response.Error != "unknown action: does-not-exist" {
		t.Fatalf("error = %q", response.Error)
	}
}

func TestSessionSocketHandleConnectionMalformedJSON(t *testing.T) {
	handler := &SessionSocketHandler{logger: testLogger()}
	response := serveOneRequest(t, handler, `{not json`)
	if response.Success || response.Error != "invalid request" {
		t.Fatalf("response = %+v", response)
	}
}

func TestSessionSocketStartStopLifecycle(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	overlayDir := t.TempDir()
	handler, err := NewSessionSocketHandler(overlayDir, sessionMgr, nil, testLogger())
	if err != nil {
		t.Fatalf("NewSessionSocketHandler: %v", err)
	}

	socketPath := filepath.Join(overlayDir, "session.sock")
	if info, err := os.Stat(socketPath); err != nil {
		t.Fatalf("socket missing after construction: %v", err)
	} else if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket path mode = %v", info.Mode())
	}

	handler.Start()
	t.Cleanup(handler.Stop)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial session socket: %v", err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(SessionRequest{Action: "start"}); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var response SessionResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.Success {
		t.Fatalf("start response = %+v", response)
	}

	handler.Stop()
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket still exists after Stop: %v", err)
	}
}

package audit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuditEventsDDL(t *testing.T) {
	if !strings.Contains(AuditEventsDDL, "CREATE TABLE IF NOT EXISTS audit.events") {
		t.Fatalf("AuditEventsDDL missing CREATE TABLE IF NOT EXISTS audit.events: %s", AuditEventsDDL)
	}
	if !strings.Contains(AuditEventsDDL, "ENGINE = MergeTree()") {
		t.Fatalf("AuditEventsDDL missing ENGINE = MergeTree(): %s", AuditEventsDDL)
	}
}

func TestClickHouseSinkWriteBatch(t *testing.T) {
	var (
		gotMethod string
		gotQuery  string
		gotAuth   string
		gotBody   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.Query().Get("query")
		gotAuth = r.Header.Get("Authorization")
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := NewClickHouseSink(srv.URL, "security_audit", "writer", "s3cr3t")
	ts := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	events := []Event{
		{
			EventID:       "ev-1",
			Timestamp:     ts,
			ActorSubject:  "alice",
			ActorClientID: "cli-1",
			Action:        ActionRead,
			Resource:      "github.com/acme/payments-api/main.go",
			Decision:      DecisionAllow,
			PrevHash:      GenesisPrevHash,
			Hash:          "hash-1",
		},
		{
			EventID:      "ev-2",
			Timestamp:    ts.Add(time.Second),
			ActorSubject: "bob",
			Action:       ActionRead,
			Resource:     "github.com/acme/orders-api/secret.txt",
			Decision:     DecisionDeny,
			PrevHash:     "hash-1",
			Hash:         "hash-2",
		},
	}

	if err := sink.WriteBatch(context.Background(), events); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	wantSQL := "INSERT INTO security_audit.events FORMAT JSONEachRow"
	if gotQuery != wantSQL {
		t.Fatalf("query = %q, want %q", gotQuery, wantSQL)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("writer:s3cr3t"))
	if gotAuth != wantAuth {
		t.Fatalf("Authorization = %q, want %q", gotAuth, wantAuth)
	}

	lines := strings.Split(strings.TrimRight(gotBody, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("JSONL line count = %d, want 2; body=%q", len(lines), gotBody)
	}
	for i, line := range lines {
		var decoded Event
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("unmarshal line %d: %v", i, err)
		}
		if decoded.EventID != events[i].EventID || decoded.Decision != events[i].Decision || decoded.Resource != events[i].Resource {
			t.Fatalf("line %d decoded = %+v, want %+v", i, decoded, events[i])
		}
	}
}

func TestClickHouseSinkNoAuthAndDefaultDatabase(t *testing.T) {
	var (
		gotQuery string
		gotAuth  string
		hits     int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotQuery = r.URL.Query().Get("query")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := NewClickHouseSink(srv.URL, "", "", "")

	// Empty batch must not hit the server.
	if err := sink.WriteBatch(context.Background(), nil); err != nil {
		t.Fatalf("empty WriteBatch: %v", err)
	}
	if hits != 0 {
		t.Fatalf("hits = %d on empty batch, want 0", hits)
	}

	if err := sink.WriteBatch(context.Background(), []Event{{
		EventID:      "ev-1",
		Timestamp:    time.Now().UTC(),
		ActorSubject: "alice",
		Action:       ActionAuth,
		Resource:     "/grpc/Read",
		Decision:     DecisionAllow,
		PrevHash:     GenesisPrevHash,
	}}); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	if gotQuery != "INSERT INTO audit.events FORMAT JSONEachRow" {
		t.Fatalf("query = %q, want default audit.events", gotQuery)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want empty", gotAuth)
	}
}

func TestClickHouseSinkNon2xxError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Code: 62. DB::Exception: Syntax error"))
	}))
	defer srv.Close()

	sink := NewClickHouseSink(srv.URL, "audit", "u", "p")
	err := sink.WriteBatch(context.Background(), []Event{{
		EventID:      "ev-err",
		Timestamp:    time.Now().UTC(),
		ActorSubject: "alice",
		Action:       ActionRead,
		Resource:     "/a",
		Decision:     DecisionAllow,
		PrevHash:     GenesisPrevHash,
	}})
	if err == nil {
		t.Fatal("WriteBatch succeeded on 500 response, want error")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "Syntax error") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClickHouseSinkWithWALFlush(t *testing.T) {
	var received []Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var ev Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			received = append(received, ev)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	walPath := filepath.Join(t.TempDir(), "audit.wal")
	emitter, err := NewWALSpoolEmitter(walPath, WithFailClosed(true))
	if err != nil {
		t.Fatalf("NewWALSpoolEmitter: %v", err)
	}
	defer emitter.Close()

	ctx := context.Background()
	if err := emitter.Emit(ctx, Event{
		ActorSubject: "alice",
		Action:       ActionRead,
		Resource:     "github.com/acme/payments-api/x.go",
		Decision:     DecisionAllow,
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	sink := NewClickHouseSink(srv.URL, "audit", "", "")
	n, err := emitter.Flush(ctx, sink)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if n != 1 || len(received) != 1 {
		t.Fatalf("flushed=%d received=%d, want 1", n, len(received))
	}
	if received[0].Hash == "" {
		t.Fatal("expected hash-chained event to carry non-empty Hash")
	}
}

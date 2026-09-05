package fuse

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

type doctorQueryWriterMock struct {
	mockClient
	content []byte
	calls   int
	query   string
}

func (m *doctorQueryWriterMock) WriteQueryLogs(ctx context.Context, query string, writer io.Writer) error {
	m.calls++
	m.query = query
	_, err := writer.Write(m.content)
	return err
}

func TestReadDoctorQueryResultsSpoolsAndReusesLocalFile(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	session, err := sessionMgr.StartSession()
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	statementPath, err := sessionMgr.GetLocalPath("doctor/v1/query/" + session.ID + "/statement")
	if err != nil {
		t.Fatalf("GetLocalPath(statement) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(statementPath), 0755); err != nil {
		t.Fatalf("MkdirAll(statement dir) error = %v", err)
	}
	if err := os.WriteFile(statementPath, []byte(`{service="doctor"}`), 0644); err != nil {
		t.Fatalf("WriteFile(statement) error = %v", err)
	}

	client := &doctorQueryWriterMock{content: []byte(`[{"body":"first"},{"body":"second"}]`)}
	node := &MonoNode{
		path:       "doctor/v1/query/" + session.ID + "/results.json",
		client:     client,
		sessionMgr: sessionMgr,
		logger:     testLogger(),
	}

	firstRead := make([]byte, 10)
	result, errno := node.Read(context.Background(), nil, firstRead, 0)
	if errno != 0 {
		t.Fatalf("Read(first) errno = %v", errno)
	}
	bytesOut, status := result.Bytes(nil)
	if status != fuse.OK {
		t.Fatalf("Read(first) status = %v, want OK", status)
	}
	if got, want := string(bytesOut), `[{"body":"`; got != want {
		t.Fatalf("Read(first) = %q, want %q", got, want)
	}

	resultsPath, err := sessionMgr.GetLocalPath(node.path)
	if err != nil {
		t.Fatalf("GetLocalPath(results) error = %v", err)
	}
	spooled, err := os.ReadFile(resultsPath)
	if err != nil {
		t.Fatalf("ReadFile(results) error = %v", err)
	}
	if got, want := string(spooled), string(client.content); got != want {
		t.Fatalf("spooled results = %q, want %q", got, want)
	}
	if client.calls != 1 {
		t.Fatalf("WriteQueryLogs calls after first read = %d, want 1", client.calls)
	}

	secondRead := make([]byte, 8)
	result, errno = node.Read(context.Background(), nil, secondRead, 10)
	if errno != 0 {
		t.Fatalf("Read(second) errno = %v", errno)
	}
	bytesOut, status = result.Bytes(nil)
	if status != fuse.OK {
		t.Fatalf("Read(second) status = %v, want OK", status)
	}
	if got, want := string(bytesOut), `first"},`; got != want {
		t.Fatalf("Read(second) = %q, want %q", got, want)
	}
	if client.calls != 1 {
		t.Fatalf("WriteQueryLogs calls after cached read = %d, want 1", client.calls)
	}
	if got, want := client.query, `{service="doctor"}`; got != want {
		t.Fatalf("WriteQueryLogs query = %q, want %q", got, want)
	}
}

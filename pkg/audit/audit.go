// Package audit provides a hash-chained, disk-backed Write-Ahead Log (WAL)
// audit event emitter and chain verifier.
package audit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Action represents an auditable operation type.
type Action string

const (
	ActionRead   Action = "read"
	ActionList   Action = "list"
	ActionSearch Action = "search"
	ActionQuery  Action = "query"
	ActionExport Action = "export"
	ActionWrite  Action = "write"
	ActionDelete Action = "delete"
	ActionDeploy Action = "deploy"
	ActionAdmin  Action = "admin"
	ActionAuth   Action = "auth"
	ActionAudit  Action = "audit_read"
)

// Decision represents the outcome of an audited operation.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
)

// GenesisPrevHash is the 64-character zero hex string used as PrevHash for the
// first event in a hash chain.
const GenesisPrevHash = "0000000000000000000000000000000000000000000000000000000000000000"

const maxWALLineBytes = 1024 * 1024 // 1 MiB

// ErrEmitterClosed is returned when Emit is called on a closed WALSpoolEmitter.
var ErrEmitterClosed = errors.New("audit: emitter closed")

// Event represents a single hash-chained audit record. The JSON field names are
// the wire contract shared by the WAL, the ClickHouse sink, the Doctor read API,
// and the Guardian anchor job.
type Event struct {
	// Identity of the record.
	EventID   string    `json:"event_id"`
	Timestamp time.Time `json:"timestamp"`
	WriterID  string    `json:"writer_id,omitempty"`
	Seq       uint64    `json:"seq"`
	PrevHash  string    `json:"prev_hash"`
	Hash      string    `json:"hash,omitempty"`

	// What happened.
	Service      string   `json:"service,omitempty"`
	Env          string   `json:"env,omitempty"`
	Scope        string   `json:"scope,omitempty"`
	Action       Action   `json:"action"`
	Decision     Decision `json:"decision"`
	Reason       string   `json:"reason,omitempty"`
	ResourceType string   `json:"resource_type,omitempty"`

	// Who.
	ActorSubject  string   `json:"actor_subject"`
	ActorClientID string   `json:"actor_client_id,omitempty"`
	Email         string   `json:"email,omitempty"`
	Groups        []string `json:"groups,omitempty"`

	// Which data.
	Tenant       string `json:"tenant,omitempty"`
	Resource     string `json:"resource"`
	Partition    string `json:"partition,omitempty"`
	ResourceHash string `json:"resource_hash,omitempty"`

	// Correlation and request shape.
	RequestID     string `json:"request_id,omitempty"`
	TraceID       string `json:"trace_id,omitempty"`
	Method        string `json:"method,omitempty"`
	Path          string `json:"path,omitempty"`
	Status        int    `json:"status,omitempty"`
	ResultCount   uint64 `json:"result_count,omitempty"`
	Bytes         uint64 `json:"bytes,omitempty"`
	Occurrences   uint32 `json:"occurrences,omitempty"`
	SourceIP      string `json:"source_ip,omitempty"`
	UserAgent     string `json:"user_agent,omitempty"`
	PolicyVersion string `json:"policy_version,omitempty"`
	Detail        string `json:"detail,omitempty"`

	// Compatibility aliases used by earlier callers.
	CorrelationID string `json:"correlation_id,omitempty"`

	// RawJSON is the exact canonical bytes that were hashed (without hash and
	// raw_json). The anchor and offline verifier use it as the evidence.
	RawJSON string `json:"raw_json,omitempty"`
}

// Sink receives batches of audit events flushed from the WAL.
type Sink interface {
	WriteBatch(ctx context.Context, events []Event) error
}

// Emitter is the minimal audit surface used by service hooks. Both the raw WAL
// emitter and the managed ServiceEmitter implement it.
type Emitter interface {
	Emit(ctx context.Context, ev Event) error
	EmitDurable(ctx context.Context, ev Event) error
}

// WALOption configures a WALSpoolEmitter.
type WALOption func(*WALSpoolEmitter)

// WithFailClosed configures whether Emit returns an error when WAL append fails.
func WithFailClosed(failClosed bool) WALOption {
	return func(e *WALSpoolEmitter) {
		e.FailClosed = failClosed
	}
}

// WithOnError sets a callback invoked when a WAL append fails in fail-open mode.
func WithOnError(fn func(error)) WALOption {
	return func(e *WALSpoolEmitter) {
		e.OnError = fn
	}
}

// WithSyncOnWrite configures whether each WAL append calls fsync.
func WithSyncOnWrite(syncOnWrite bool) WALOption {
	return func(e *WALSpoolEmitter) {
		e.syncOnWrite = syncOnWrite
	}
}

// WithWriterID sets the stable writer identity used for the per-writer hash
// chain. A writer should be unique per process lifetime (for example
// "monofs-server-<pod-uid>").
func WithWriterID(writerID string) WALOption {
	return func(e *WALSpoolEmitter) {
		e.writerID = writerID
	}
}

// WithService and WithEnv stamp every event with the emitting service and
// deployment environment when the caller leaves them empty.
func WithService(service string) WALOption {
	return func(e *WALSpoolEmitter) {
		e.service = service
	}
}

func WithEnv(env string) WALOption {
	return func(e *WALSpoolEmitter) {
		e.env = env
	}
}

// WALSpoolEmitter appends hash-chained audit events to a disk-backed JSONL WAL
// file and supports flushing buffered events to a downstream Sink.
type WALSpoolEmitter struct {
	mu            sync.Mutex
	flushMu       sync.Mutex
	walPath       string
	file          *os.File
	lastHash      string
	nextSeq       uint64
	writerID      string
	service       string
	env           string
	flushedOffset int64
	droppedCount  atomic.Int64
	FailClosed    bool
	OnError       func(error)
	syncOnWrite   bool
	closed        bool
}

// NewWALSpoolEmitter creates a WALSpoolEmitter backed by walPath. If walPath
// already exists, it recovers the last event's Hash so new events continue the
// existing hash chain.
func NewWALSpoolEmitter(walPath string, opts ...WALOption) (*WALSpoolEmitter, error) {
	if walPath == "" {
		return nil, errors.New("audit: walPath must not be empty")
	}

	lastHash, nextSeq, err := recoverChainState(walPath)
	if err != nil {
		return nil, err
	}

	e := &WALSpoolEmitter{
		walPath:  walPath,
		lastHash: lastHash,
		nextSeq:  nextSeq,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	return e, nil
}

// Emit assigns defaults (EventID, Timestamp) if unset, links PrevHash and Hash,
// and appends the event as a JSONL line to the WAL.
func (e *WALSpoolEmitter) Emit(ctx context.Context, ev Event) error {
	return e.emit(ctx, ev, e.FailClosed)
}

// EmitDurable appends a mutation/delete/deploy event fail-closed regardless of
// the emitter's default fail mode.
func (e *WALSpoolEmitter) EmitDurable(ctx context.Context, ev Event) error {
	return e.emit(ctx, ev, true)
}

func (e *WALSpoolEmitter) emit(ctx context.Context, ev Event, failClosed bool) error {
	if err := ctx.Err(); err != nil {
		return e.handleError(err, failClosed)
	}

	if ev.EventID == "" {
		ev.EventID = uuid.NewString()
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	} else {
		ev.Timestamp = ev.Timestamp.UTC()
	}
	if ev.WriterID == "" {
		ev.WriterID = e.writerID
	}
	if ev.Service == "" {
		ev.Service = e.service
	}
	if ev.Env == "" {
		ev.Env = e.env
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return ErrEmitterClosed
	}

	ev.PrevHash = e.lastHash
	ev.Seq = e.nextSeq
	hash, canonical, err := computeEventHash(ev)
	if err != nil {
		return e.handleError(err, failClosed)
	}
	ev.Hash = hash
	ev.RawJSON = canonical

	line, err := json.Marshal(ev)
	if err != nil {
		return e.handleError(fmt.Errorf("audit: marshal event: %w", err), failClosed)
	}
	line = append(line, '\n')

	if err := e.appendLineLocked(line); err != nil {
		return e.handleError(err, failClosed)
	}

	// Advance the chain only after the WAL write succeeds so failed writes in
	// fail-open mode do not break the persisted hash chain.
	e.lastHash = ev.Hash
	e.nextSeq++
	return nil
}

// Flush reads any unflushed events from the WAL starting at the current
// FlushedOffset, ships them to sink via WriteBatch, and advances FlushedOffset
// upon success.
func (e *WALSpoolEmitter) Flush(ctx context.Context, sink Sink) (int, error) {
	if sink == nil {
		return 0, errors.New("audit: sink must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	e.flushMu.Lock()
	defer e.flushMu.Unlock()

	events, newOffset, err := e.readUnflushedEvents()
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}

	if err := sink.WriteBatch(ctx, events); err != nil {
		return 0, fmt.Errorf("audit: sink write batch: %w", err)
	}

	e.mu.Lock()
	e.flushedOffset = newOffset
	e.mu.Unlock()

	return len(events), nil
}

// FlushedOffset returns the byte offset in the WAL up to which events have
// been successfully flushed to a Sink.
func (e *WALSpoolEmitter) FlushedOffset() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.flushedOffset
}

// DroppedCount returns the number of events dropped when FailClosed is false
// and WAL append failed.
func (e *WALSpoolEmitter) DroppedCount() int64 {
	return e.droppedCount.Load()
}

// Close closes the underlying WAL file handle if open.
func (e *WALSpoolEmitter) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return nil
	}
	e.closed = true
	if e.file != nil {
		err := e.file.Close()
		e.file = nil
		return err
	}
	return nil
}

func (e *WALSpoolEmitter) appendLineLocked(line []byte) error {
	if e.file == nil {
		f, err := os.OpenFile(e.walPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("audit: open wal %q: %w", e.walPath, err)
		}
		e.file = f
	}

	if _, err := e.file.Write(line); err != nil {
		return fmt.Errorf("audit: write wal: %w", err)
	}
	if e.syncOnWrite {
		if err := e.file.Sync(); err != nil {
			return fmt.Errorf("audit: sync wal: %w", err)
		}
	}
	return nil
}

func (e *WALSpoolEmitter) handleError(err error, failClosed bool) error {
	if failClosed {
		return err
	}
	e.droppedCount.Add(1)
	if e.OnError != nil {
		e.OnError(err)
	}
	return nil
}

func (e *WALSpoolEmitter) readUnflushedEvents() ([]Event, int64, error) {
	e.mu.Lock()
	startOffset := e.flushedOffset
	walPath := e.walPath
	e.mu.Unlock()

	f, err := os.Open(walPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, startOffset, nil
		}
		return nil, startOffset, fmt.Errorf("audit: open wal for flush: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return nil, startOffset, fmt.Errorf("audit: seek wal to %d: %w", startOffset, err)
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, startOffset, fmt.Errorf("audit: read wal: %w", err)
	}

	lastNewline := bytes.LastIndexByte(data, '\n')
	if lastNewline < 0 {
		return nil, startOffset, nil
	}

	complete := data[:lastNewline+1]
	newOffset := startOffset + int64(len(complete))

	var events []Event
	scanner := bufio.NewScanner(bytes.NewReader(complete))
	scanner.Buffer(make([]byte, 0, 64*1024), maxWALLineBytes)
	for scanner.Scan() {
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, startOffset, fmt.Errorf("audit: unmarshal wal event: %w", err)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, startOffset, fmt.Errorf("audit: scan wal for flush: %w", err)
	}

	return events, newOffset, nil
}

// VerifyChain reads the JSONL WAL at walPath, verifies every event's PrevHash
// linkage and SHA-256 Hash, and returns the total event count or an error
// identifying the tampered or corrupted line.
func VerifyChain(walPath string) (int, error) {
	f, err := os.Open(walPath)
	if err != nil {
		return 0, fmt.Errorf("audit: open wal %q: %w", walPath, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxWALLineBytes)

	expectedPrevHash := GenesisPrevHash
	var expectedSeq uint64
	count := 0
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}

		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			return count, fmt.Errorf("audit: line %d: invalid json: %w", lineNum, err)
		}

		if ev.PrevHash != expectedPrevHash {
			return count, fmt.Errorf("audit: line %d: prev_hash mismatch: got %q, want %q", lineNum, ev.PrevHash, expectedPrevHash)
		}
		if ev.Seq != expectedSeq {
			return count, fmt.Errorf("audit: line %d: seq mismatch: got %d, want %d", lineNum, ev.Seq, expectedSeq)
		}

		expectedHash, _, err := computeEventHash(ev)
		if err != nil {
			return count, fmt.Errorf("audit: line %d: %w", lineNum, err)
		}
		if ev.Hash != expectedHash {
			return count, fmt.Errorf("audit: line %d: hash mismatch: got %q, want %q", lineNum, ev.Hash, expectedHash)
		}

		expectedPrevHash = ev.Hash
		expectedSeq++
		count++
	}

	if err := scanner.Err(); err != nil {
		return count, fmt.Errorf("audit: scan wal: %w", err)
	}
	return count, nil
}

func computeEventHash(ev Event) (string, string, error) {
	ev.Hash = ""
	ev.RawJSON = ""
	ev.Timestamp = ev.Timestamp.UTC()
	canonical, err := json.Marshal(ev)
	if err != nil {
		return "", "", fmt.Errorf("audit: marshal canonical event: %w", err)
	}

	h := sha256.New()
	h.Write([]byte(ev.PrevHash))
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil)), string(canonical), nil
}

func recoverChainState(walPath string) (string, uint64, error) {
	f, err := os.Open(walPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return GenesisPrevHash, 0, nil
		}
		return "", 0, fmt.Errorf("audit: open existing wal %q: %w", walPath, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxWALLineBytes)

	lastHash := GenesisPrevHash
	var lastSeq uint64
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			return "", 0, fmt.Errorf("audit: recover wal line %d: invalid json: %w", lineNum, err)
		}
		if ev.Hash == "" {
			return "", 0, fmt.Errorf("audit: recover wal line %d: missing hash", lineNum)
		}
		lastHash = ev.Hash
		lastSeq = ev.Seq
	}
	if err := scanner.Err(); err != nil {
		return "", 0, fmt.Errorf("audit: scan existing wal: %w", err)
	}
	return lastHash, lastSeq + 1, nil
}

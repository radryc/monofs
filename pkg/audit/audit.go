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
	ActionWrite  Action = "write"
	ActionDelete Action = "delete"
	ActionAdmin  Action = "admin"
	ActionAuth   Action = "auth"
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

// Event represents a single hash-chained audit record.
type Event struct {
	EventID       string    `json:"event_id"`
	Timestamp     time.Time `json:"timestamp"`
	ActorSubject  string    `json:"actor_subject"`
	ActorClientID string    `json:"actor_client_id,omitempty"`
	Action        Action    `json:"action"`
	Resource      string    `json:"resource"`
	Partition     string    `json:"partition,omitempty"`
	Decision      Decision  `json:"decision"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	SourceIP      string    `json:"source_ip,omitempty"`
	PrevHash      string    `json:"prev_hash"`
	Hash          string    `json:"hash,omitempty"`
}

// Sink receives batches of audit events flushed from the WAL.
type Sink interface {
	WriteBatch(ctx context.Context, events []Event) error
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

// WALSpoolEmitter appends hash-chained audit events to a disk-backed JSONL WAL
// file and supports flushing buffered events to a downstream Sink.
type WALSpoolEmitter struct {
	mu            sync.Mutex
	flushMu       sync.Mutex
	walPath       string
	file          *os.File
	lastHash      string
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

	lastHash, err := recoverLastHash(walPath)
	if err != nil {
		return nil, err
	}

	e := &WALSpoolEmitter{
		walPath:  walPath,
		lastHash: lastHash,
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
	if err := ctx.Err(); err != nil {
		return e.handleError(err)
	}

	if ev.EventID == "" {
		ev.EventID = uuid.NewString()
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	} else {
		ev.Timestamp = ev.Timestamp.UTC()
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return ErrEmitterClosed
	}

	ev.PrevHash = e.lastHash
	hash, err := computeEventHash(ev)
	if err != nil {
		return e.handleError(err)
	}
	ev.Hash = hash

	line, err := json.Marshal(ev)
	if err != nil {
		return e.handleError(fmt.Errorf("audit: marshal event: %w", err))
	}
	line = append(line, '\n')

	if err := e.appendLineLocked(line); err != nil {
		return e.handleError(err)
	}

	// Advance lastHash only after the WAL write succeeds so failed writes in
	// fail-open mode do not break the persisted hash chain.
	e.lastHash = ev.Hash
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

func (e *WALSpoolEmitter) handleError(err error) error {
	if e.FailClosed {
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

		expectedHash, err := computeEventHash(ev)
		if err != nil {
			return count, fmt.Errorf("audit: line %d: %w", lineNum, err)
		}
		if ev.Hash != expectedHash {
			return count, fmt.Errorf("audit: line %d: hash mismatch: got %q, want %q", lineNum, ev.Hash, expectedHash)
		}

		expectedPrevHash = ev.Hash
		count++
	}

	if err := scanner.Err(); err != nil {
		return count, fmt.Errorf("audit: scan wal: %w", err)
	}
	return count, nil
}

func computeEventHash(ev Event) (string, error) {
	ev.Hash = ""
	ev.Timestamp = ev.Timestamp.UTC()
	canonical, err := json.Marshal(ev)
	if err != nil {
		return "", fmt.Errorf("audit: marshal canonical event: %w", err)
	}

	h := sha256.New()
	h.Write([]byte(ev.PrevHash))
	h.Write([]byte("|"))
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func recoverLastHash(walPath string) (string, error) {
	f, err := os.Open(walPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return GenesisPrevHash, nil
		}
		return "", fmt.Errorf("audit: open existing wal %q: %w", walPath, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxWALLineBytes)

	lastHash := GenesisPrevHash
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			return "", fmt.Errorf("audit: recover wal line %d: invalid json: %w", lineNum, err)
		}
		if ev.Hash == "" {
			return "", fmt.Errorf("audit: recover wal line %d: missing hash", lineNum)
		}
		lastHash = ev.Hash
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("audit: scan existing wal: %w", err)
	}
	return lastHash, nil
}

package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config describes the audit emitter used by a Strata service.
type Config struct {
	Enabled            bool
	Service            string
	Env                string
	WriterID           string
	WALDir             string
	ClickHouseURL      string
	ClickHouseDatabase string
	ClickHouseUser     string
	ClickHousePassword string
	FlushInterval      time.Duration
	FailClosed         bool
	SyncOnWrite        bool
}

// ConfigFromEnv builds a Config from the shared AUDIT_* environment contract.
// It is intentionally identical across Go services.
func ConfigFromEnv(service string) Config {
	cfg := Config{
		Enabled:            envBool("AUDIT_ENABLED", false),
		Service:            strings.TrimSpace(os.Getenv("AUDIT_SERVICE")),
		Env:                strings.TrimSpace(os.Getenv("AUDIT_ENV")),
		WriterID:           strings.TrimSpace(os.Getenv("AUDIT_WRITER_ID")),
		WALDir:             strings.TrimSpace(os.Getenv("AUDIT_WAL_DIR")),
		ClickHouseURL:      strings.TrimSpace(os.Getenv("AUDIT_CH_URL")),
		ClickHouseDatabase: strings.TrimSpace(os.Getenv("AUDIT_CH_DATABASE")),
		ClickHouseUser:     strings.TrimSpace(os.Getenv("AUDIT_CH_USER")),
		ClickHousePassword: strings.TrimSpace(os.Getenv("AUDIT_CH_PASSWORD")),
		SyncOnWrite:        envBool("AUDIT_SYNC_ON_WRITE", true),
		FailClosed:         strings.EqualFold(strings.TrimSpace(os.Getenv("AUDIT_MODE")), "strict") || envBool("AUDIT_FAIL_CLOSED", false),
	}
	if cfg.Service == "" {
		cfg.Service = service
	}
	if cfg.WriterID == "" {
		cfg.WriterID = defaultWriterID(service)
	}
	if cfg.WALDir == "" {
		cfg.WALDir = "/var/lib/audit/wal"
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = envDuration("AUDIT_FLUSH_INTERVAL", 2*time.Second)
	}
	return cfg
}

// ServiceEmitter owns the WAL emitter and the background batcher that ships
// events to a Sink. A nil *ServiceEmitter is inert, so callers can wire it
// unconditionally.
type ServiceEmitter struct {
	wal      *WALSpoolEmitter
	sink     Sink
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
}

// NewServiceEmitter builds an emitter when cfg.Enabled is true. It returns
// (nil, nil) when auditing is disabled.
func NewServiceEmitter(cfg Config) (*ServiceEmitter, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if strings.TrimSpace(cfg.WALDir) == "" {
		return nil, fmt.Errorf("audit: AUDIT_WAL_DIR must not be empty")
	}
	if err := os.MkdirAll(cfg.WALDir, 0o700); err != nil {
		return nil, fmt.Errorf("audit: create WAL dir: %w", err)
	}
	walPath := filepath.Join(cfg.WALDir, safeFileName(cfg.WriterID)+".wal")
	wal, err := NewWALSpoolEmitter(walPath,
		WithFailClosed(cfg.FailClosed),
		WithSyncOnWrite(cfg.SyncOnWrite),
		WithWriterID(cfg.WriterID),
		WithService(cfg.Service),
		WithEnv(cfg.Env),
		WithOnError(func(err error) {
			log.Printf("audit_emit_error writer=%q err=%v", cfg.WriterID, err)
		}),
	)
	if err != nil {
		return nil, err
	}
	e := &ServiceEmitter{
		wal:      wal,
		interval: cfg.FlushInterval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if strings.TrimSpace(cfg.ClickHouseURL) != "" {
		e.sink = NewClickHouseSink(cfg.ClickHouseURL, cfg.ClickHouseDatabase, cfg.ClickHouseUser, cfg.ClickHousePassword)
	}
	if e.interval <= 0 {
		e.interval = 2 * time.Second
	}
	go e.flushLoop()
	return e, nil
}

// Emit records one event using the configured fail mode.
func (e *ServiceEmitter) Emit(ctx context.Context, ev Event) error {
	if e == nil {
		return nil
	}
	return e.wal.Emit(ctx, ev)
}

// EmitDurable records a mutation/delete/deploy event fail-closed: if the WAL
// cannot accept it, the caller must fail the request.
func (e *ServiceEmitter) EmitDurable(ctx context.Context, ev Event) error {
	if e == nil {
		return nil
	}
	return e.wal.EmitDurable(ctx, ev)
}

// Flush ships unflushed WAL records to the configured sink. It is a no-op when
// no ClickHouse sink is configured.
func (e *ServiceEmitter) Flush(ctx context.Context) (int, error) {
	if e == nil || e.sink == nil {
		return 0, nil
	}
	return e.wal.Flush(ctx, e.sink)
}

// Close stops the batcher, flushes once, and closes the WAL.
func (e *ServiceEmitter) Close(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.once.Do(func() { close(e.stop) })
	select {
	case <-e.done:
	case <-ctx.Done():
	}
	_, flushErr := e.Flush(ctx)
	closeErr := e.wal.Close()
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

// DroppedCount reports events dropped in fail-open mode.
func (e *ServiceEmitter) DroppedCount() int64 {
	if e == nil {
		return 0
	}
	return e.wal.DroppedCount()
}

// WALPath returns the WAL file backing the emitter (empty for nil).
func (e *ServiceEmitter) WALPath() string {
	if e == nil {
		return ""
	}
	return e.wal.walPath
}

// VerifyWAL verifies the emitter's own WAL chain.
func (e *ServiceEmitter) VerifyWAL() (int, error) {
	if e == nil {
		return 0, nil
	}
	return VerifyChain(e.wal.walPath)
}

func (e *ServiceEmitter) flushLoop() {
	defer close(e.done)
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if _, err := e.Flush(ctx); err != nil {
				log.Printf("audit_flush_error writer=%q err=%v", e.wal.writerID, err)
			}
			cancel()
		}
	}
}

func defaultWriterID(service string) string {
	host, _ := os.Hostname()
	host = strings.TrimSpace(host)
	if host == "" {
		host = "unknown"
	}
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err == nil {
		return service + "-" + host + "-" + hex.EncodeToString(suffix[:])
	}
	return service + "-" + host + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func safeFileName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "writer"
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func envBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return value
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

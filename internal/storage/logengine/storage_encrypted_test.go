package logengine

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/radryc/monofs/internal/telemetrycrypto"
)

func TestEncryptedStoreRoundTripAndPlaintextAtRest(t *testing.T) {
	dir := t.TempDir()
	provider := testEncryptionProvider(t, telemetrycrypto.ModeObserve, 1)
	store := NewEncryptedStore(NewMockS3Store(dir), provider)
	path := "chunks/logs/partition=diva-shared/date=2026-11-09/hour=12/chunk-1/segment.parquet"
	body := []byte("payment declined for order 42")

	if err := store.Write(context.Background(), path, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, body) {
		t.Fatal("object store contains plaintext")
	}
	if !telemetrycrypto.IsEnvelope(raw) {
		t.Fatal("object store value is not an envelope")
	}

	rc, err := store.Read(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("round trip = %q, want %q", got, body)
	}
}

func TestEncryptedStoreWrongKeyFails(t *testing.T) {
	dir := t.TempDir()
	path := "chunks/logs/partition=diva-shared/chunk-1/segment.parquet"
	first := NewEncryptedStore(NewMockS3Store(dir), testEncryptionProvider(t, telemetrycrypto.ModeObserve, 1))
	if err := first.Write(context.Background(), path, bytes.NewReader([]byte("secret"))); err != nil {
		t.Fatal(err)
	}
	second := NewEncryptedStore(NewMockS3Store(dir), testEncryptionProvider(t, telemetrycrypto.ModeObserve, 2))
	if _, err := second.Read(context.Background(), path); err == nil {
		t.Fatal("wrong key must fail")
	}
}

func TestEncryptedStoreEnforceRejectsPlaintext(t *testing.T) {
	dir := t.TempDir()
	path := "chunks/logs/partition=diva-shared/legacy-segment.parquet"
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("legacy plaintext"), 0644); err != nil {
		t.Fatal(err)
	}
	store := NewEncryptedStore(NewMockS3Store(dir), testEncryptionProvider(t, telemetrycrypto.ModeEnforce, 1))
	if _, err := store.Read(context.Background(), path); err == nil {
		t.Fatal("enforce mode must reject plaintext")
	}
}

func TestEncryptedStoreObserveReadsLegacyPlaintext(t *testing.T) {
	dir := t.TempDir()
	path := "chunks/logs/partition=diva-shared/legacy-segment.parquet"
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("legacy plaintext"), 0644); err != nil {
		t.Fatal(err)
	}
	store := NewEncryptedStore(NewMockS3Store(dir), testEncryptionProvider(t, telemetrycrypto.ModeObserve, 1))
	rc, err := store.Read(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "legacy plaintext" {
		t.Fatalf("plaintext = %q", got)
	}
}

func testEncryptionProvider(t *testing.T, mode telemetrycrypto.Mode, fill byte) *telemetrycrypto.Provider {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
	cfg, err := telemetrycrypto.ParseKeyFile([]byte(`{
		"version": 1,
		"tenant": "default",
		"root_key_id": "root-v1",
		"root_key": "` + key + `"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Enabled = true
	cfg.Mode = mode
	provider, err := telemetrycrypto.NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

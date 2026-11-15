package blob

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/radryc/monofs/internal/storage"
)

func initBackendWithKey(t *testing.T, cacheDir string, keyByte byte) *BlobBackend {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = keyByte
	}
	backend := NewBlobBackend()
	if err := backend.Initialize(context.Background(), storage.BackendConfig{
		CacheDir:      cacheDir,
		Concurrency:   1,
		EncryptionKey: key,
	}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	t.Cleanup(func() {
		_ = backend.Close()
	})
	return backend
}

func TestBlobBackend_EncryptionKeyFirstUseAutoAccepted(t *testing.T) {
	backend := initBackendWithKey(t, t.TempDir(), 1)
	status := backend.EncryptionKeyStatus()

	if status.Pending {
		t.Fatal("first use should not be pending")
	}
	if status.State != "auto_accepted" {
		t.Fatalf("state = %q, want auto_accepted", status.State)
	}
	if !status.FirstKeyAutoAccepted {
		t.Fatal("FirstKeyAutoAccepted = false, want true")
	}
	if status.CanConfirm {
		t.Fatal("CanConfirm = true for a non-pending key")
	}
}

func TestBlobBackend_EncryptionKeyReopenWithSameKey(t *testing.T) {
	dir := t.TempDir()
	first := initBackendWithKey(t, dir, 1)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := initBackendWithKey(t, dir, 1)
	status := second.EncryptionKeyStatus()
	if status.State != "ok" {
		t.Fatalf("state = %q, want ok", status.State)
	}
	if status.FirstKeyAutoAccepted {
		t.Fatal("reopened backend should not report a first-key auto-accept")
	}
}

func TestBlobBackend_EncryptionKeyRotationRequiresConfirmation(t *testing.T) {
	dir := t.TempDir()
	first := initBackendWithKey(t, dir, 1)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rotated := initBackendWithKey(t, dir, 2)
	status := rotated.EncryptionKeyStatus()
	if !status.Pending || status.State != "pending" {
		t.Fatalf("status = %+v, want pending", status)
	}
	if !status.CanConfirm {
		t.Fatal("CanConfirm = false for a pending key")
	}

	if err := rotated.StoreBlob("hash", []byte("content")); err == nil {
		t.Fatal("StoreBlob should be blocked while the key is pending")
	}
	if err := rotated.keyGuardCheck(); err == nil {
		t.Fatal("keyGuardCheck should reject a pending key")
	}

	confirmed, err := rotated.ConfirmEncryptionKey()
	if err != nil {
		t.Fatalf("ConfirmEncryptionKey: %v", err)
	}
	if confirmed.Pending || confirmed.State != "confirmed" {
		t.Fatalf("confirmed status = %+v", confirmed)
	}
	if confirmed.FirstKeyAutoAccepted {
		t.Fatal("explicitly confirmed key reported as auto-accepted")
	}
	if err := rotated.keyGuardCheck(); err != nil {
		t.Fatalf("keyGuardCheck after confirmation: %v", err)
	}
	if err := rotated.StoreBlob("hash", []byte("content")); err != nil {
		t.Fatalf("StoreBlob after confirmation: %v", err)
	}
}

func TestBlobBackend_EncryptionKeyFingerprintMatchesConfiguredKey(t *testing.T) {
	dir := t.TempDir()
	backend := initBackendWithKey(t, dir, 3)

	key := make([]byte, 32)
	for i := range key {
		key[i] = 3
	}
	want := fmt.Sprintf("%x", sha256.Sum256(key))
	if backend.currentKeyFingerprint != want {
		t.Fatalf("fingerprint = %q, want %q", backend.currentKeyFingerprint, want)
	}
	if backend.acceptedKeyFingerprint != want {
		t.Fatalf("accepted fingerprint = %q, want %q", backend.acceptedKeyFingerprint, want)
	}
}

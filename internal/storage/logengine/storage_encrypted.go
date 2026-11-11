package logengine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/radryc/monofs/internal/telemetrycrypto"
)

// EncryptedStore is an ObjectStoreBackend decorator that seals every logengine
// object with the tenant/partition subkey. It deliberately does not implement
// LocalPathProvider: callers must go through Read so decrypted data never
// bypasses this layer.
type EncryptedStore struct {
	inner    ObjectStoreBackend
	provider *telemetrycrypto.Provider
	tenant   string
}

// NewEncryptedStore wraps backend when provider is enabled. It returns backend
// unchanged when encryption is off.
func NewEncryptedStore(backend ObjectStoreBackend, provider *telemetrycrypto.Provider) ObjectStoreBackend {
	if backend == nil || provider == nil || !provider.Active() {
		return backend
	}
	return &EncryptedStore{inner: backend, provider: provider, tenant: provider.Tenant()}
}

func (s *EncryptedStore) Write(ctx context.Context, path string, reader io.Reader) error {
	plain, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read object for encryption: %w", err)
	}
	sealed, err := s.provider.Seal(plain, s.tenant, partitionFromPath(path), objectContext(path))
	if err != nil {
		return err
	}
	return s.inner.Write(ctx, path, bytes.NewReader(sealed))
}

func (s *EncryptedStore) Read(ctx context.Context, path string) (io.ReadSeekCloser, error) {
	rc, err := s.inner.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read object for decryption: %w", err)
	}
	plain, err := s.provider.Open(data, objectContext(path))
	if err != nil {
		return nil, fmt.Errorf("decrypt %s: %w", path, err)
	}
	return &bytesReadSeekCloser{Reader: bytes.NewReader(plain)}, nil
}

func (s *EncryptedStore) ListChunks(ctx context.Context, prefix string) ([]string, error) {
	return s.inner.ListChunks(ctx, prefix)
}

func (s *EncryptedStore) Delete(ctx context.Context, path string) error {
	return s.inner.Delete(ctx, path)
}

func objectContext(path string) string {
	return strings.Trim(filepath.ToSlash(path), "/")
}

// partitionFromPath extracts the Hive partition directory from a logengine
// object path. Legacy paths without partition= use the unpartitioned subkey.
func partitionFromPath(path string) string {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if strings.HasPrefix(part, "partition=") {
			return strings.TrimPrefix(part, "partition=")
		}
	}
	return ""
}

type bytesReadSeekCloser struct {
	*bytes.Reader
}

func (b *bytesReadSeekCloser) Close() error { return nil }

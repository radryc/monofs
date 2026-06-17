package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

type BlobStore struct {
	client *Client
}

func NewBlobStore(client *Client) *BlobStore {
	return &BlobStore{client: client}
}

func BlobPath(digest string) string {
	return "_blobs/" + strings.Replace(digest, ":", "/", 1)
}

func DigestKey(digest string) string {
	return "_blobs/" + digest
}

func (b *BlobStore) Get(ctx context.Context, digest string) ([]byte, error) {
	path := BlobPath(digest)
	data, err := b.client.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (b *BlobStore) Exists(ctx context.Context, digest string) (bool, error) {
	path := BlobPath(digest)
	return b.client.Exists(ctx, path)
}

func (b *BlobStore) Put(ctx context.Context, digest string, content []byte) error {
	verifiedDigest := "sha256:" + hex.EncodeToString(sha256Hash(content))
	if digest != verifiedDigest {
		return fmt.Errorf("digest mismatch: expected %s, got %s", digest, verifiedDigest)
	}
	path := BlobPath(digest)
	dir := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		dir = path[:idx]
	}
	if err := b.client.CreateDir(ctx, dir); err != nil {
		return fmt.Errorf("create blob dir: %w", err)
	}
	if err := b.client.Write(ctx, path, content); err != nil {
		return fmt.Errorf("write blob: %w", err)
	}
	return nil
}

func (b *BlobStore) PutUnchecked(ctx context.Context, digest string, content []byte) error {
	path := BlobPath(digest)
	dir := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		dir = path[:idx]
	}
	if err := b.client.CreateDir(ctx, dir); err != nil {
		return fmt.Errorf("create blob dir: %w", err)
	}
	if err := b.client.Write(ctx, path, content); err != nil {
		return fmt.Errorf("write blob: %w", err)
	}
	return nil
}

func (b *BlobStore) Delete(ctx context.Context, digest string) error {
	path := BlobPath(digest)
	err := b.client.Delete(ctx, path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (b *BlobStore) Size(ctx context.Context, digest string) (int64, error) {
	path := BlobPath(digest)
	resp, err := b.client.Stat(ctx, path)
	if err != nil {
		return 0, err
	}
	return int64(resp.GetSize()), nil
}

func sha256Hash(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}

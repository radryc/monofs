package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
)

const chunkSize = 512 * 1024 * 1024 // 512 MB per chunk to stay well under gRPC 1 GB limit

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
	if err == nil {
		return data, nil
	}
	return b.readChunked(ctx, digest)
}

func (b *BlobStore) GetReader(ctx context.Context, digest string) (io.ReadCloser, error) {
	path := BlobPath(digest)
	rc, err := b.client.ReadStream(ctx, path)
	if err == nil {
		return rc, nil
	}
	return b.readChunkedReader(ctx, digest)
}

func (b *BlobStore) readChunkedReader(ctx context.Context, digest string) (io.ReadCloser, error) {
	var readers []io.ReadCloser
	for i := 0; ; i++ {
		chunkPath := chunkPath(digest, i)
		rc, err := b.client.ReadStream(ctx, chunkPath)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			break
		}
		readers = append(readers, rc)
	}
	if len(readers) == 0 {
		return nil, os.ErrNotExist
	}
	return &multiReadCloser{readers: readers}, nil
}

func (b *BlobStore) readChunked(ctx context.Context, digest string) ([]byte, error) {
	var result []byte
	for i := 0; ; i++ {
		chunkPath := chunkPath(digest, i)
		data, err := b.client.Read(ctx, chunkPath)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			break
		}
		result = append(result, data...)
	}
	return result, nil
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
	return b.putChunkedFromReader(ctx, digest, bytesReader(content), int64(len(content)))
}

func (b *BlobStore) PutUnchecked(ctx context.Context, digest string, content []byte) error {
	return b.putChunkedFromReader(ctx, digest, bytesReader(content), int64(len(content)))
}

func (b *BlobStore) PutUncheckedFromReader(ctx context.Context, digest string, r io.Reader, size int64) error {
	return b.putChunkedFromReader(ctx, digest, r, size)
}

func (b *BlobStore) putChunkedFromReader(ctx context.Context, digest string, r io.Reader, size int64) error {
	if size <= 0 {
		return b.writeSingle(ctx, BlobPath(digest), nil)
	}
	chunks := int(math.Ceil(float64(size) / float64(chunkSize)))
	for i := 0; i < chunks; i++ {
		remain := size - int64(i)*int64(chunkSize)
		readSize := chunkSize
		if int64(readSize) > remain {
			readSize = int(remain)
		}
		buf := make([]byte, readSize)
		if _, err := io.ReadFull(r, buf); err != nil {
			return fmt.Errorf("read chunk %d: %w", i, err)
		}
		chunkPath := chunkPath(digest, i)
		if err := b.writeSingle(ctx, chunkPath, buf); err != nil {
			return fmt.Errorf("write chunk %d: %w", i, err)
		}
	}
	return nil
}

func (b *BlobStore) writeSingle(ctx context.Context, path string, data []byte) error {
	dir := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		dir = path[:idx]
	}
	if err := b.client.CreateDir(ctx, dir); err != nil {
		return fmt.Errorf("create blob dir: %w", err)
	}
	if err := b.client.Write(ctx, path, data); err != nil {
		return fmt.Errorf("write blob: %w", err)
	}
	return nil
}

func (b *BlobStore) Delete(ctx context.Context, digest string) error {
	path := BlobPath(digest)
	b.client.Delete(ctx, path)
	for i := 0; ; i++ {
		chunkPath := chunkPath(digest, i)
		exists, _ := b.client.Exists(ctx, chunkPath)
		if !exists {
			break
		}
		b.client.Delete(ctx, chunkPath)
	}
	return nil
}

func (b *BlobStore) Size(ctx context.Context, digest string) (int64, error) {
	path := BlobPath(digest)
	resp, err := b.client.Stat(ctx, path)
	if err == nil {
		return int64(resp.GetSize()), nil
	}
	var total int64
	for i := 0; ; i++ {
		chunkPath := chunkPath(digest, i)
		resp, err := b.client.Stat(ctx, chunkPath)
		if err != nil {
			if i == 0 {
				return 0, err
			}
			break
		}
		total += int64(resp.GetSize())
	}
	return total, nil
}

func chunkPath(digest string, index int) string {
	return BlobPath(digest) + "/" + fmt.Sprintf("%04d", index)
}

func sha256Hash(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}

type multiReadCloser struct {
	readers []io.ReadCloser
	current int
}

func (m *multiReadCloser) Read(p []byte) (int, error) {
	for m.current < len(m.readers) {
		n, err := m.readers[m.current].Read(p)
		if err == io.EOF {
			m.readers[m.current].Close()
			m.current++
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
	return 0, io.EOF
}

func (m *multiReadCloser) Close() error {
	var lastErr error
	for i := m.current; i < len(m.readers); i++ {
		if err := m.readers[i].Close(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

type byteReader struct {
	data   []byte
	offset int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

func bytesReader(data []byte) io.Reader {
	return &byteReader{data: data}
}

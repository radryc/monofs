package blob

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/radryc/monofs/internal/storage"
	"github.com/radryc/monofs/internal/storage/blob/layout"
)

// TestV2S3StoreIntegration exercises the real S3 adapter, including
// conditional manifest writes. It is skipped unless MONOFS_TEST_S3_ENDPOINT is
// set, so it never runs in the normal unit-test path.
func TestV2S3StoreIntegration(t *testing.T) {
	endpoint := os.Getenv("MONOFS_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set MONOFS_TEST_S3_ENDPOINT to run the S3 integration test")
	}
	bucket := os.Getenv("MONOFS_TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "monofs"
	}
	accessKey := os.Getenv("MONOFS_TEST_S3_ACCESS_KEY")
	if accessKey == "" {
		accessKey = "minioadmin"
	}
	secretKey := os.Getenv("MONOFS_TEST_S3_SECRET_KEY")
	if secretKey == "" {
		secretKey = "minioadmin"
	}

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	prefix := fmt.Sprintf("_v2test-%d", time.Now().UnixNano())

	backend := NewBlobBackend()
	if err := backend.Initialize(context.Background(), storage.BackendConfig{
		CacheDir:      t.TempDir(),
		EncryptionKey: key,
		StorageType:   storage.StorageTypeS3,
		Cloud: storage.CloudStorageConfig{
			S3Region:          "us-east-1",
			S3Bucket:          bucket,
			S3Endpoint:        endpoint,
			S3AccessKeyID:     accessKey,
			S3SecretAccessKey: secretKey,
			S3UsePathStyle:    true,
			S3Prefix:          prefix,
		},
	}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	backend.layoutMode = "v2"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	shard := "ab"
	next := layout.NewManifest(shard)
	etag, err := backend.CompareAndSwap(ctx, shard, "", next)
	if err != nil {
		t.Fatalf("create manifest: %v", err)
	}
	if etag == "" {
		t.Fatal("empty etag from create")
	}

	if _, err := backend.CompareAndSwap(ctx, shard, "", next); !errors.Is(err, layout.ErrVersionConflict) {
		t.Fatalf("expected stale-create conflict, got %v", err)
	}

	read, readETag, err := backend.ReadManifest(ctx, shard)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if read.Version != 1 || readETag != etag {
		t.Fatalf("manifest version=%d etag=%q, want 1/%q", read.Version, readETag, etag)
	}

	update := read.Clone()
	if err := update.AddPack(layout.PackRef{ID: "1-0000", Bytes: 10, Blobs: 1, Sealed: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.CompareAndSwap(ctx, shard, readETag, update); err != nil {
		t.Fatalf("update manifest: %v", err)
	}
	if _, err := backend.CompareAndSwap(ctx, shard, readETag, update); !errors.Is(err, layout.ErrVersionConflict) {
		t.Fatalf("expected stale-update conflict, got %v", err)
	}

	// Clean up the test objects.
	_ = backend.DeletePack(ctx, shard, "1-0000")
	_ = backend.v2Objects.Delete(ctx, backend.v2ObjectKey(layout.ManifestKey(shard)))
}

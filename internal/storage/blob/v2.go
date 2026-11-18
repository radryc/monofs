package blob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/radryc/monofs/internal/storage"
	"github.com/radryc/monofs/internal/storage/blob/layout"
	"github.com/radryc/packager"
	pkgstorage "github.com/radryc/packager/storage"
)

// v2ObjectStore is the small object-store surface the v2 manifest/index needs.
// It is deliberately narrower than the S3 client so tests can supply an
// in-memory implementation.
type v2ObjectStore interface {
	Get(ctx context.Context, key string) (data []byte, etag string, err error)
	Put(ctx context.Context, key string, data []byte, ifMatch, ifNoneMatch string) (etag string, err error)
	Delete(ctx context.Context, key string) error
}

// s3V2ObjectStore implements v2ObjectStore on top of the S3 client.
type s3V2ObjectStore struct {
	bb *BlobBackend
}

func (s *s3V2ObjectStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	if s.bb.s3Client == nil {
		return nil, "", fmt.Errorf("s3 client not configured")
	}
	out, err := s.bb.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bb.config.Cloud.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, "", os.ErrNotExist
		}
		return nil, "", err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", err
	}
	return data, aws.ToString(out.ETag), nil
}

func (s *s3V2ObjectStore) Put(ctx context.Context, key string, data []byte, ifMatch, ifNoneMatch string) (string, error) {
	if s.bb.s3Client == nil {
		return "", fmt.Errorf("s3 client not configured")
	}
	in := &s3.PutObjectInput{
		Bucket:      aws.String(s.bb.config.Cloud.S3Bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	}
	if ifMatch != "" {
		in.IfMatch = aws.String(ifMatch)
	}
	if ifNoneMatch != "" {
		in.IfNoneMatch = aws.String(ifNoneMatch)
	}
	out, err := s.bb.s3Client.PutObject(ctx, in)
	if err != nil {
		if isS3Precondition(err) {
			return "", layout.ErrVersionConflict
		}
		return "", err
	}
	return aws.ToString(out.ETag), nil
}

func (s *s3V2ObjectStore) Delete(ctx context.Context, key string) error {
	if s.bb.s3Client == nil {
		return fmt.Errorf("s3 client not configured")
	}
	_, err := s.bb.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bb.config.Cloud.S3Bucket),
		Key:    aws.String(key),
	})
	return err
}

func s3ErrorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

func isS3NotFound(err error) bool {
	switch s3ErrorCode(err) {
	case "NoSuchKey", "NotFound", "404":
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "statuscode: 404")
}

func isS3Precondition(err error) bool {
	switch s3ErrorCode(err) {
	case "PreconditionFailed", "ConditionalRequestConflict":
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "preconditionfailed") || strings.Contains(msg, "statuscode: 412") ||
		strings.Contains(msg, "conditionalrequestconflict")
}

// v2CachedManifest and v2CachedIndex are short-lived read-through caches so a
// pull does not fetch the manifest and every index sidecar on every request.
type v2CachedManifest struct {
	manifest *layout.Manifest
	etag     string
	expires  time.Time
}

type v2CachedIndex struct {
	set     map[string]struct{}
	expires time.Time
}

type v2Cache struct {
	mu        sync.Mutex
	manifests map[string]v2CachedManifest
	indexes   map[string]v2CachedIndex
	shards    map[string]struct{}
}

const (
	v2CacheTTL        = 30 * time.Second
	v2CacheMaxEntries = 4096
)

func newV2Cache() *v2Cache {
	return &v2Cache{
		manifests: make(map[string]v2CachedManifest),
		indexes:   make(map[string]v2CachedIndex),
		shards:    make(map[string]struct{}),
	}
}

func (c *v2Cache) getManifest(shard string) (*layout.Manifest, string, bool) {
	if c == nil {
		return nil, "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.manifests[shard]
	if c.shards == nil {
		c.shards = make(map[string]struct{})
	}
	c.shards[shard] = struct{}{}
	if !ok || time.Now().After(entry.expires) {
		delete(c.manifests, shard)
		return nil, "", false
	}
	return entry.manifest.Clone(), entry.etag, true
}

func (c *v2Cache) putManifest(shard string, m *layout.Manifest, etag string) {
	if c == nil || m == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.manifests) >= v2CacheMaxEntries {
		c.manifests = make(map[string]v2CachedManifest)
	}
	if c.shards == nil {
		c.shards = make(map[string]struct{})
	}
	c.shards[shard] = struct{}{}
	c.manifests[shard] = v2CachedManifest{manifest: m.Clone(), etag: etag, expires: time.Now().Add(v2CacheTTL)}
}

// seenShards reports how many shard manifests this fetcher has read or written.
func (c *v2Cache) seenShards() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.shards)
}

func (c *v2Cache) getIndex(key string) (map[string]struct{}, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.indexes[key]
	if !ok || time.Now().After(entry.expires) {
		delete(c.indexes, key)
		return nil, false
	}
	return entry.set, true
}

func (c *v2Cache) putIndex(key string, set map[string]struct{}) {
	if c == nil || set == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.indexes) >= v2CacheMaxEntries {
		c.indexes = make(map[string]v2CachedIndex)
	}
	c.indexes[key] = v2CachedIndex{set: set, expires: time.Now().Add(v2CacheTTL)}
}

func (c *v2Cache) invalidateIndex(shard, packID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.indexes, shard+"/"+packID)
	c.mu.Unlock()
}

// v2WriteEnabled reports whether new blobs should be written in the v2 layout.
func (bb *BlobBackend) v2WriteEnabled() bool {
	return bb.LayoutMode() == "v2" && bb.v2Objects != nil && bb.hasCloudUpload() && bb.pipeline != nil
}

// v2ReadEnabled reports whether v2 manifests can be consulted on reads.
func (bb *BlobBackend) v2ReadEnabled() bool {
	return bb.LayoutMode() == "v2" && bb.v2Objects != nil
}

// v2ObjectKey applies the configured cloud prefix to a layout-relative key.
func (bb *BlobBackend) v2ObjectKey(rel string) string {
	prefix := bb.config.Cloud.S3Prefix
	if bb.config.StorageType == storage.StorageTypeGCS {
		prefix = bb.config.Cloud.GCSPrefix
	}
	rel = strings.TrimLeft(rel, "/")
	if prefix == "" {
		return rel
	}
	return strings.TrimRight(prefix, "/") + "/" + rel
}

func (bb *BlobBackend) v2LocalDir(shard string) string {
	return filepath.Join(bb.config.CacheDir, "archives", "packs", "v2", "sha256", shard)
}

func (bb *BlobBackend) v2LocalPack(shard, packID string) string {
	return filepath.Join(bb.v2LocalDir(shard), packID+layout.PackSuffix)
}

func (bb *BlobBackend) v2LocalIndex(shard, packID string) string {
	return filepath.Join(bb.v2LocalDir(shard), packID+layout.PackSuffix+layout.IndexSuffix)
}

// normalizeV2Entry maps a content ID to a clean archive entry name.
func normalizeV2Entry(name string) string {
	n := strings.TrimSpace(name)
	n = strings.TrimPrefix(n, "sha256:")
	n = strings.TrimPrefix(n, "sha256-")
	if n == "" || strings.ContainsAny(n, `/\`) || strings.Contains(n, "..") {
		return ""
	}
	return n
}

// v2WritePack writes one pack for a shard and uploads it. The manifest is
// updated separately by addV2Pack so the pack is invisible until committed.
func (bb *BlobBackend) v2WritePack(ctx context.Context, shard, packID string, blobs map[string][]byte) (layout.PackRef, error) {
	if err := layout.ValidateShard(shard); err != nil {
		return layout.PackRef{}, err
	}
	if packID == "" {
		return layout.PackRef{}, fmt.Errorf("v2: pack id is required")
	}
	if len(blobs) == 0 {
		return layout.PackRef{}, fmt.Errorf("v2: pack has no blobs")
	}
	if bb.pipeline == nil {
		return layout.PackRef{}, fmt.Errorf("v2: packager pipeline not initialized")
	}
	if bb.uploadArchiveFunc == nil {
		return layout.PackRef{}, fmt.Errorf("v2: cloud upload is not configured")
	}

	dir := bb.v2LocalDir(shard)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return layout.PackRef{}, fmt.Errorf("v2: create pack dir: %w", err)
	}
	archivePath := bb.v2LocalPack(shard, packID)
	tmpPath := archivePath + ".tmp"
	file, err := os.Create(tmpPath)
	if err != nil {
		return layout.PackRef{}, fmt.Errorf("v2: create pack: %w", err)
	}
	writer := packager.NewArchiveWriter(file, bb.pipeline)

	keys := make([]string, 0, len(blobs))
	for key := range blobs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	names := make([]string, 0, len(keys))
	var rawBytes int64
	for _, key := range keys {
		entry := normalizeV2Entry(key)
		if entry == "" {
			file.Close()
			os.Remove(tmpPath)
			return layout.PackRef{}, fmt.Errorf("v2: invalid archive entry %q", key)
		}
		if err := writer.AddFile(entry, blobs[key], packager.DefaultAddFileOptions()); err != nil {
			file.Close()
			os.Remove(tmpPath)
			return layout.PackRef{}, fmt.Errorf("v2: add %q: %w", entry, err)
		}
		names = append(names, entry)
		rawBytes += int64(len(blobs[key]))
	}
	if err := writer.Close(); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return layout.PackRef{}, fmt.Errorf("v2: close pack writer: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return layout.PackRef{}, fmt.Errorf("v2: sync pack: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(tmpPath)
		return layout.PackRef{}, fmt.Errorf("v2: close pack file: %w", err)
	}
	if err := os.Rename(tmpPath, archivePath); err != nil {
		os.Remove(tmpPath)
		return layout.PackRef{}, fmt.Errorf("v2: rename pack: %w", err)
	}
	if err := bb.writeArchiveIndex(archivePath, names); err != nil {
		return layout.PackRef{}, fmt.Errorf("v2: write pack index: %w", err)
	}
	if err := bb.uploadArchiveFunc(ctx, archivePath); err != nil {
		return layout.PackRef{}, fmt.Errorf("v2: upload pack: %w", err)
	}
	if _, err := bb.indexArchive(archivePath); err != nil {
		return layout.PackRef{}, fmt.Errorf("v2: index pack: %w", err)
	}

	return layout.PackRef{
		ID:          packID,
		Bytes:       rawBytes,
		Blobs:       int64(len(names)),
		CreatedUnix: time.Now().UTC().Unix(),
		Sealed:      true,
	}, nil
}

// addV2Pack commits a pack to its shard manifest with CAS retries.
func (bb *BlobBackend) addV2Pack(ctx context.Context, shard string, ref layout.PackRef) error {
	for attempt := 0; attempt < 8; attempt++ {
		m, etag, err := bb.ReadManifest(ctx, shard)
		if errors.Is(err, layout.ErrManifestNotFound) || errors.Is(err, os.ErrNotExist) {
			m = layout.NewManifest(shard)
			etag = ""
		} else if err != nil {
			return err
		}
		next := m.Clone()
		if unsealed, ok := next.UnsealedPack(); ok {
			next.Seal(unsealed.ID)
		}
		if _, ok := next.Find(ref.ID); !ok {
			if err := next.AddPack(ref); err != nil {
				return err
			}
		}
		next.Seal(ref.ID)
		if _, err := bb.CompareAndSwap(ctx, shard, etag, next); err == nil {
			return nil
		} else if !errors.Is(err, layout.ErrVersionConflict) {
			return err
		}
	}
	return fmt.Errorf("v2: manifest CAS retries exhausted for shard %s", shard)
}

// v2StoreBlob writes a single blob into a fresh pack and commits it.
func (bb *BlobBackend) v2StoreBlob(ctx context.Context, blobHash string, content []byte) error {
	shard, err := layout.Shard(blobHash)
	if err != nil {
		return err
	}
	packID := layout.NewPackID(time.Now(), int(bb.v2PackSeq.Add(1)))
	ref, err := bb.v2WritePack(ctx, shard, packID, map[string][]byte{blobHash: content})
	if err != nil {
		return err
	}
	if err := bb.addV2Pack(ctx, shard, ref); err != nil {
		return err
	}
	stats := *bb.stats.Load()
	stats.CachedItems++
	stats.CacheBytes += int64(len(content))
	bb.stats.Store(&stats)
	packagerStoreBlobsTotal.WithLabelValues("v2").Inc()
	packagerIndexedBlobsGauge.Inc()
	return nil
}

// v2FindBlob resolves a content ID through the shard manifest and pack indexes.
func (bb *BlobBackend) v2FindBlob(ctx context.Context, blobHash string) (archiveRef, bool, error) {
	if !bb.v2ReadEnabled() {
		return archiveRef{}, false, nil
	}
	shard, err := layout.Shard(blobHash)
	if err != nil {
		return archiveRef{}, false, nil
	}
	m, err := bb.v2Manifest(ctx, shard)
	if errors.Is(err, layout.ErrManifestNotFound) || errors.Is(err, os.ErrNotExist) {
		return archiveRef{}, false, nil
	}
	if err != nil {
		return archiveRef{}, false, err
	}
	bare := normalizeV2Entry(blobHash)
	if bare == "" {
		return archiveRef{}, false, nil
	}
	for _, pack := range m.Packs {
		index, err := bb.v2Index(ctx, shard, pack.ID)
		if err != nil {
			continue
		}
		if _, ok := index[bare]; !ok {
			if _, ok := index[blobHash]; !ok {
				continue
			}
		}
		return bb.loadCloudArchive(blobHash, bb.v2LocalPack(shard, pack.ID))
	}
	return archiveRef{}, false, nil
}

func (bb *BlobBackend) v2Manifest(ctx context.Context, shard string) (*layout.Manifest, error) {
	if bb.v2Cache != nil {
		if m, _, ok := bb.v2Cache.getManifest(shard); ok {
			return m, nil
		}
	}
	m, etag, err := bb.ReadManifest(ctx, shard)
	if err != nil {
		return nil, err
	}
	if bb.v2Cache != nil {
		bb.v2Cache.putManifest(shard, m, etag)
	}
	return m, nil
}

func (bb *BlobBackend) v2Index(ctx context.Context, shard, packID string) (map[string]struct{}, error) {
	cacheKey := shard + "/" + packID
	if bb.v2Cache != nil {
		if set, ok := bb.v2Cache.getIndex(cacheKey); ok {
			return set, nil
		}
	}
	hashes, err := bb.ReadIndex(ctx, shard, packID)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(hashes)*2)
	for _, hash := range hashes {
		set[hash] = struct{}{}
		if bare := normalizeV2Entry(hash); bare != "" {
			set[bare] = struct{}{}
		}
	}
	if bb.v2Cache != nil {
		bb.v2Cache.putIndex(cacheKey, set)
	}
	return set, nil
}

// ReadManifest implements layout.Store.
func (bb *BlobBackend) ReadManifest(ctx context.Context, shard string) (*layout.Manifest, string, error) {
	if bb.v2Objects == nil {
		return nil, "", fmt.Errorf("v2: object store not configured")
	}
	data, etag, err := bb.v2Objects.Get(ctx, bb.v2ObjectKey(layout.ManifestKey(shard)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", layout.ErrManifestNotFound
		}
		return nil, "", err
	}
	var m layout.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, "", fmt.Errorf("v2: decode manifest %s: %w", shard, err)
	}
	return &m, etag, nil
}

// CompareAndSwap implements layout.Store.
func (bb *BlobBackend) CompareAndSwap(ctx context.Context, shard, expectedETag string, next *layout.Manifest) (string, error) {
	if bb.v2Objects == nil {
		return "", fmt.Errorf("v2: object store not configured")
	}
	if next == nil {
		return "", fmt.Errorf("v2: nil manifest")
	}
	cp := next.Clone()
	cp.Layout = layout.Version
	cp.Shard = shard
	cp.Version = next.Version + 1
	cp.UpdatedUnix = time.Now().UTC().Unix()
	data, err := json.Marshal(cp)
	if err != nil {
		return "", err
	}
	ifMatch, ifNoneMatch := expectedETag, ""
	if expectedETag == "" {
		ifNoneMatch = "*"
	}
	etag, err := bb.v2Objects.Put(ctx, bb.v2ObjectKey(layout.ManifestKey(shard)), data, ifMatch, ifNoneMatch)
	if err != nil {
		return "", err
	}
	if bb.v2Cache != nil {
		bb.v2Cache.putManifest(shard, cp, etag)
	}
	return etag, nil
}

// ReadIndex implements layout.Store.
func (bb *BlobBackend) ReadIndex(ctx context.Context, shard, packID string) ([]string, error) {
	if bb.v2Objects == nil {
		return nil, fmt.Errorf("v2: object store not configured")
	}
	data, _, err := bb.v2Objects.Get(ctx, bb.v2ObjectKey(layout.IndexKey(shard, packID)))
	if err != nil {
		return nil, err
	}
	var hashes []string
	if err := json.Unmarshal(data, &hashes); err != nil {
		return nil, fmt.Errorf("v2: decode index %s/%s: %w", shard, packID, err)
	}
	return hashes, nil
}

// RewritePack implements layout.Store. It copies live entries from the source
// packs into a new pack. It does not consult a liveness set; callers that need
// dead-byte reclamation must delete blobs first.
func (bb *BlobBackend) RewritePack(ctx context.Context, shard, targetID string, sources []layout.PackRef) (layout.PackRef, error) {
	merged := make(map[string][]byte)
	for _, source := range sources {
		local, err := bb.v2EnsureLocalPack(ctx, shard, source.ID)
		if err != nil {
			return layout.PackRef{}, err
		}
		hashes, err := bb.ReadIndex(ctx, shard, source.ID)
		if err != nil {
			return layout.PackRef{}, err
		}
		store, err := pkgstorage.NewLocalFileReader(local)
		if err != nil {
			return layout.PackRef{}, fmt.Errorf("v2: open source pack %s: %w", source.ID, err)
		}
		ar, err := packager.OpenArchive(store, bb.pipeline)
		if err != nil {
			store.Close()
			return layout.PackRef{}, fmt.Errorf("v2: decode source pack %s: %w", source.ID, err)
		}
		for _, name := range hashes {
			if _, ok := merged[name]; ok {
				continue
			}
			data, _, err := ar.GetFile(name)
			if err != nil {
				continue
			}
			merged[name] = data
		}
		ar.Close()
		store.Close()
	}
	if len(merged) == 0 {
		return layout.PackRef{}, fmt.Errorf("v2: rewrite shard %s: no live entries", shard)
	}
	return bb.v2WritePack(ctx, shard, targetID, merged)
}

// DeletePack implements layout.Store.
func (bb *BlobBackend) DeletePack(ctx context.Context, shard, packID string) error {
	if bb.v2Objects == nil {
		return fmt.Errorf("v2: object store not configured")
	}
	var firstErr error
	for _, key := range []string{
		bb.v2ObjectKey(layout.PackKey(shard, packID)),
		bb.v2ObjectKey(layout.IndexKey(shard, packID)),
	} {
		if err := bb.v2Objects.Delete(ctx, key); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	_ = os.Remove(bb.v2LocalPack(shard, packID))
	_ = os.Remove(bb.v2LocalIndex(shard, packID))
	if bb.v2Cache != nil {
		bb.v2Cache.invalidateIndex(shard, packID)
	}
	bb.archiveDeletesTotal.Add(1)
	if bb.otel.archiveDeletes != nil {
		bb.otel.archiveDeletes.Add(context.Background(), 1)
	}
	return firstErr
}

func (bb *BlobBackend) v2EnsureLocalPack(ctx context.Context, shard, packID string) (string, error) {
	local := bb.v2LocalPack(shard, packID)
	if _, err := os.Stat(local); err == nil {
		return local, nil
	}
	if err := bb.downloadFromCloud(local); err != nil {
		return "", fmt.Errorf("v2: download pack %s/%s: %w", shard, packID, err)
	}
	return local, nil
}

// RunCompactor runs one compaction cycle over a bounded set of shards.
func (bb *BlobBackend) RunCompactor(ctx context.Context, cfg layout.CompactionConfig) (layout.RunResult, error) {
	if !bb.v2ReadEnabled() {
		return layout.RunResult{}, nil
	}
	compactor := layout.NewCompactor(bb, cfg)
	res, err := compactor.Run(ctx)
	bb.RecordCompactorRun(
		int64(res.ShardsScanned),
		int64(res.PlansApplied),
		int64(res.PacksRetired),
		int64(res.PacksDeleted),
		res.BytesRewritten,
		err,
	)
	return res, err
}

// v2Counts reports the local pack count and the number of shard manifests this
// fetcher has observed. Manifests live in S3, so they are counted from the
// read/write cache rather than by scanning the local filesystem.
func (bb *BlobBackend) v2Counts() (packs, manifests int64) {
	manifests = int64(bb.v2Cache.seenShards())
	root := filepath.Join(bb.config.CacheDir, "archives", "packs", "v2", "sha256")
	shards, err := os.ReadDir(root)
	if err != nil {
		return 0, manifests
	}
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, shard.Name()))
		if err != nil {
			continue
		}
		for _, file := range files {
			if strings.HasSuffix(file.Name(), layout.PackSuffix) {
				packs++
			}
		}
	}
	return packs, manifests
}

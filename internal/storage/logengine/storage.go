package logengine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	chunkListCacheTTL   = 10 * time.Second
	manifestCacheTTL    = 30 * time.Minute
	seriesIndexCacheTTL = 5 * time.Minute
	cacheSweepInterval  = 30 * time.Second
	// chunkTimeBucketDuration is the directory bucket used for newly written
	// chunks. Queries list only the buckets overlapping their time range instead
	// of the flat chunks/<signal>/ directory, which grows with retention.
	chunkTimeBucketDuration = time.Hour
	chunkTimeBucketLayout   = "2006/01/02/15"
	// maxBucketedRangeScan bounds how many hourly buckets a single query will
	// enumerate before falling back to a recursive bucket walk. This keeps an
	// accidentally unbounded from/to from turning into thousands of listings.
	maxBucketedRangeScan = 90 * 24 * time.Hour
	// maxManifestCacheEntries bounds the in-memory chunk manifest cache.
	maxManifestCacheEntries = 32768
	// maxSeriesIndexCacheEntries bounds the in-memory series index cache. Each
	// entry is a small per-chunk slice (tens of KB); the cap keeps a large
	// store from pinning hundreds of megabytes.
	maxSeriesIndexCacheEntries = 512
	// maxLocalIndexDirs bounds the number of extracted Bluge index directories
	// kept on local disk under CachedStore.localDir.
	maxLocalIndexDirs = 256
)

type chunkListCacheEntry struct {
	chunkIDs  []string
	expiresAt time.Time
}

type manifestCacheEntry struct {
	manifest  ChunkManifest
	expiresAt time.Time
}

type seriesIndexCacheEntry struct {
	series    []MetricSeriesMeta
	expiresAt time.Time
}

// ErrGhostChunk is returned when a chunk is requested but not found in the remote storage.
var ErrGhostChunk = errors.New("ghost chunk: not found in storage")

// ObjectStoreBackend defines the interface for underlying chunk storage (S3/Local).
type ObjectStoreBackend interface {
	Write(ctx context.Context, path string, reader io.Reader) error
	// Read should return ErrGhostChunk if the file does not exist (e.g. S3 NoSuchKey).
	Read(ctx context.Context, path string) (io.ReadSeekCloser, error)
	ListChunks(ctx context.Context, prefix string) ([]string, error)
}

// CachedStore implements a caching decorator around a remote ObjectStoreBackend.
// It caches index files to a local NVMe drive and uses singleflight to
// prevent cache stampedes.
type CachedStore struct {
	remote       ObjectStoreBackend
	localDir     string
	sf           singleflight.Group
	mu           sync.RWMutex
	chunks       map[string]chunkListCacheEntry
	manifests    map[string]manifestCacheEntry
	series       map[string]seriesIndexCacheEntry
	localIndexes map[string]time.Time
	nextSweep    time.Time
}

// NewCachedStore creates a new CachedStore.
func NewCachedStore(remote ObjectStoreBackend, localDir string) *CachedStore {
	return &CachedStore{
		remote:       remote,
		localDir:     localDir,
		chunks:       make(map[string]chunkListCacheEntry),
		manifests:    make(map[string]manifestCacheEntry),
		series:       make(map[string]seriesIndexCacheEntry),
		localIndexes: make(map[string]time.Time),
		nextSweep:    time.Now().Add(cacheSweepInterval),
	}
}

func (c *CachedStore) pruneExpiredLocked(now time.Time) {
	if !c.nextSweep.IsZero() && now.Before(c.nextSweep) {
		return
	}
	for prefix, entry := range c.chunks {
		if !now.Before(entry.expiresAt) {
			delete(c.chunks, prefix)
		}
	}
	for path, entry := range c.manifests {
		if !now.Before(entry.expiresAt) {
			delete(c.manifests, path)
		}
	}
	for path, entry := range c.series {
		if !now.Before(entry.expiresAt) {
			delete(c.series, path)
		}
	}
	c.nextSweep = now.Add(cacheSweepInterval)
}

// trimManifestsLocked enforces the manifest cache size cap by dropping
// entries with the earliest expiry.
func (c *CachedStore) trimManifestsLocked() {
	for len(c.manifests) > maxManifestCacheEntries {
		var (
			oldestPath string
			oldestTime time.Time
		)
		for path, entry := range c.manifests {
			if oldestPath == "" || entry.expiresAt.Before(oldestTime) {
				oldestPath = path
				oldestTime = entry.expiresAt
			}
		}
		delete(c.manifests, oldestPath)
	}
}

// trimSeriesIndexLocked enforces the series index cache size cap by dropping
// entries with the earliest expiry.
func (c *CachedStore) trimSeriesIndexLocked() {
	for len(c.series) > maxSeriesIndexCacheEntries {
		var (
			oldestPath string
			oldestTime time.Time
		)
		for path, entry := range c.series {
			if oldestPath == "" || entry.expiresAt.Before(oldestTime) {
				oldestPath = path
				oldestTime = entry.expiresAt
			}
		}
		delete(c.series, oldestPath)
	}
}

// Write passes through to the remote storage.
func (c *CachedStore) Write(ctx context.Context, path string, reader io.Reader) error {
	if err := c.remote.Write(ctx, path, reader); err != nil {
		return err
	}
	c.invalidatePath(path)
	return nil
}

// ListChunks passes through to the remote storage.
func (c *CachedStore) ListChunks(ctx context.Context, prefix string) ([]string, error) {
	normalizedPrefix := normalizeChunkPrefix(prefix)
	if normalizedPrefix == "" {
		return c.remote.ListChunks(ctx, prefix)
	}

	now := time.Now()
	c.mu.RLock()
	entry, ok := c.chunks[normalizedPrefix]
	c.mu.RUnlock()
	if ok && now.Before(entry.expiresAt) {
		return append([]string(nil), entry.chunkIDs...), nil
	}
	if ok {
		c.mu.Lock()
		if staleEntry, staleOK := c.chunks[normalizedPrefix]; staleOK && !now.Before(staleEntry.expiresAt) {
			delete(c.chunks, normalizedPrefix)
		}
		c.mu.Unlock()
	}

	chunkIDs, err := c.remote.ListChunks(ctx, normalizedPrefix)
	if err != nil {
		return nil, err
	}
	sort.Strings(chunkIDs)

	c.mu.Lock()
	c.pruneExpiredLocked(now)
	c.chunks[normalizedPrefix] = chunkListCacheEntry{
		chunkIDs:  append([]string(nil), chunkIDs...),
		expiresAt: now.Add(chunkListCacheTTL),
	}
	c.mu.Unlock()

	return chunkIDs, nil
}

// ListChunkPaths returns chunk paths relative to chunks/<signal>/ for chunks
// that overlap [from, to]. New chunks are written under hourly time buckets
// (YYYY/MM/DD/HH/<chunkID>); legacy flat chunks are still returned. The result
// is suitable for filepath.Join("chunks", signal, path, ...).
func (c *CachedStore) ListChunkPaths(ctx context.Context, signal Signal, from, to time.Time) ([]string, error) {
	root := filepath.ToSlash(filepath.Join("chunks", string(signal)))
	entries, err := c.ListChunks(ctx, root)
	if err != nil {
		return nil, err
	}

	paths := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	add := func(path string) {
		if path == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}

	years := make([]string, 0)
	for _, entry := range entries {
		if isTimeBucketName(entry, 4) {
			years = append(years, entry)
			continue
		}
		if looksLikeChunkID(entry) {
			add(entry)
		}
	}

	if !from.IsZero() && !to.IsZero() && to.Sub(from) <= maxBucketedRangeScan {
		start := chunkBucketStart(from).Add(-chunkTimeBucketDuration)
		end := chunkBucketStart(to)
		for bucket := start; !bucket.After(end); bucket = bucket.Add(chunkTimeBucketDuration) {
			relativeBucket := bucket.Format(chunkTimeBucketLayout)
			bucketPrefix := filepath.ToSlash(filepath.Join(root, relativeBucket))
			chunkIDs, listErr := c.ListChunks(ctx, bucketPrefix)
			if listErr != nil {
				return nil, listErr
			}
			for _, chunkID := range chunkIDs {
				add(filepath.ToSlash(filepath.Join(relativeBucket, chunkID)))
			}
		}
	} else {
		for _, year := range years {
			if err := c.walkTimeBuckets(ctx, root, year, 1, add); err != nil {
				return nil, err
			}
		}
	}

	if !from.IsZero() || !to.IsZero() {
		filtered := paths[:0]
		for _, path := range paths {
			if chunkPathOverlaps(path, from, to) {
				filtered = append(filtered, path)
			}
		}
		paths = filtered
	}
	sort.Strings(paths)
	return paths, nil
}

func (c *CachedStore) walkTimeBuckets(ctx context.Context, root, relative string, depth int, add func(string)) error {
	prefix := filepath.ToSlash(filepath.Join(root, relative))
	entries, err := c.ListChunks(ctx, prefix)
	if err != nil {
		return err
	}
	if depth >= 4 {
		for _, chunkID := range entries {
			if looksLikeChunkID(chunkID) {
				add(filepath.ToSlash(filepath.Join(relative, chunkID)))
			}
		}
		return nil
	}
	width := 2
	for _, entry := range entries {
		if !isTimeBucketName(entry, width) {
			continue
		}
		if err := c.walkTimeBuckets(ctx, root, filepath.ToSlash(filepath.Join(relative, entry)), depth+1, add); err != nil {
			return err
		}
	}
	return nil
}

func chunkBucketStart(t time.Time) time.Time {
	return t.UTC().Truncate(chunkTimeBucketDuration)
}

// relativeChunkPath returns the path of a new chunk relative to
// chunks/<signal>/. Buckets are selected by the data's min timestamp, not by
// the ingestion wall clock.
func relativeChunkPath(chunkID string, minTime time.Time) string {
	if minTime.IsZero() {
		return chunkID
	}
	return filepath.ToSlash(filepath.Join(minTime.UTC().Format(chunkTimeBucketLayout), chunkID))
}

func isTimeBucketName(name string, width int) bool {
	if len(name) != width {
		return false
	}
	for _, ch := range name {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func looksLikeChunkID(name string) bool {
	return strings.HasPrefix(name, "chunk-") ||
		strings.HasPrefix(name, "compact-") ||
		strings.HasPrefix(name, "ts_")
}

func chunkPathOverlaps(path string, from, to time.Time) bool {
	minTime, maxTime, ok := ParseChunkTimeRangeFromID(path)
	if !ok {
		// Legacy or malformed IDs are kept; the manifest remains authoritative.
		return true
	}
	if !to.IsZero() && minTime.After(to) {
		return false
	}
	if !from.IsZero() && maxTime.Before(from) {
		return false
	}
	return true
}

func (c *CachedStore) ReadManifest(ctx context.Context, signal Signal, chunkID string) (ChunkManifest, error) {
	manifestPath := filepath.ToSlash(filepath.Join("chunks", string(signal), chunkID, "metadata.json"))
	now := time.Now()

	c.mu.RLock()
	entry, ok := c.manifests[manifestPath]
	c.mu.RUnlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.manifest, nil
	}
	if ok {
		c.mu.Lock()
		if staleEntry, staleOK := c.manifests[manifestPath]; staleOK && !now.Before(staleEntry.expiresAt) {
			delete(c.manifests, manifestPath)
		}
		c.mu.Unlock()
	}

	rc, err := c.remote.Read(ctx, manifestPath)
	if err != nil {
		return ChunkManifest{}, err
	}
	defer rc.Close()

	var manifest ChunkManifest
	if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
		return ChunkManifest{}, err
	}

	c.mu.Lock()
	c.pruneExpiredLocked(now)
	c.manifests[manifestPath] = manifestCacheEntry{
		manifest:  manifest,
		expiresAt: now.Add(manifestCacheTTL),
	}
	c.trimManifestsLocked()
	c.mu.Unlock()

	return manifest, nil
}

func normalizeChunkPrefix(prefix string) string {
	cleaned := strings.Trim(strings.TrimSpace(filepath.ToSlash(prefix)), "/")
	return cleaned
}

// ReadSeriesIndex returns the cached per-chunk metric series index. Chunks are
// immutable once written, so entries stay cached until the TTL sweeps them.
// Chunks written before the series index existed return ErrGhostChunk.
func (c *CachedStore) ReadSeriesIndex(ctx context.Context, chunkID string) ([]MetricSeriesMeta, error) {
	path := filepath.ToSlash(filepath.Join("chunks", string(SignalMetrics), chunkID, "series.index.json"))
	now := time.Now()

	c.mu.RLock()
	entry, ok := c.series[path]
	c.mu.RUnlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.series, nil
	}

	rc, err := c.remote.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	var series []MetricSeriesMeta
	if err := json.NewDecoder(rc).Decode(&series); err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.pruneExpiredLocked(now)
	c.series[path] = seriesIndexCacheEntry{series: series, expiresAt: now.Add(seriesIndexCacheTTL)}
	c.trimSeriesIndexLocked()
	c.mu.Unlock()

	return series, nil
}

func (c *CachedStore) invalidatePath(path string) {
	cleaned := strings.Trim(strings.TrimSpace(path), "/")
	if cleaned == "" {
		return
	}

	parts := strings.Split(cleaned, "/")
	if len(parts) < 3 || parts[0] != "chunks" {
		return
	}

	// Invalidate the signal root and every time-bucket ancestor of the written
	// path. Recursive bucket walks cache each level, so deleting only the leaf
	// would let a query miss a newly created hour bucket until the 10s TTL.
	signalPrefix := filepath.ToSlash(filepath.Join(parts[0], parts[1]))
	ancestors := []string{signalPrefix}
	if len(parts) >= 5 {
		ancestors = append(ancestors,
			strings.Join(parts[:3], "/"),
			strings.Join(parts[:4], "/"),
			strings.Join(parts[:5], "/"),
		)
	}
	c.mu.Lock()
	for _, prefix := range ancestors {
		delete(c.chunks, prefix)
	}
	if strings.HasSuffix(cleaned, "/metadata.json") {
		delete(c.manifests, cleaned)
	}
	c.mu.Unlock()
}

// Read handles caching for index files and pass-through for Parquet/metadata.
func (c *CachedStore) Read(ctx context.Context, path string) (io.ReadSeekCloser, error) {
	// Only cache index files
	if !strings.HasSuffix(path, ".index.tar.gz") {
		return c.remote.Read(ctx, path)
	}

	localPath := filepath.Join(c.localDir, path)

	// Singleflight ensures multiple requests for the same missing chunk
	// trigger only a single download.
	_, err, _ := c.sf.Do(path, func() (interface{}, error) {
		// Check if it already exists locally
		if _, err := os.Stat(localPath); err == nil {
			return nil, nil // Cache hit
		}

		// Cache miss: download and extract
		rc, err := c.remote.Read(ctx, path)
		if err != nil {
			return nil, err
		}
		defer rc.Close()

		// Extract the tar.gz to the local directory
		if err := extractTarGz(rc, localPath); err != nil {
			return nil, fmt.Errorf("failed to extract index: %w", err)
		}

		return nil, nil
	})

	if err != nil {
		return nil, err
	}

	// At this point, the file/directory exists locally.
	// We return a dummy ReadSeekCloser because Bluge uses memory-mapped access
	// on the directory rather than standard IO. The query engine will just use
	// the local path. We can just return a file to satisfy the interface, or
	// we can add a method specifically for getting the local path of an index.
	// For now, return the open file if we were just caching a file, but since
	// it's an extracted directory, returning nil or a custom closer is better.
	// Since bluge opens the path directly, we might not need this Read to return the file.
	// Let's return a dummy closer for the directory or handle it differently in the query layer.
	// Returning the local directory path is what Bluge actually needs.
	return nil, fmt.Errorf("use GetLocalIndexPath for index directories")
}

// GetLocalIndexPath retrieves the local path for an index, downloading it if necessary.
func (c *CachedStore) GetLocalIndexPath(ctx context.Context, path string) (string, error) {
	localPath := filepath.Join(c.localDir, strings.TrimSuffix(path, ".tar.gz"))

	_, err, _ := c.sf.Do(path, func() (interface{}, error) {
		if _, err := os.Stat(localPath); err == nil {
			return nil, nil // Cache hit
		}

		rc, err := c.remote.Read(ctx, path)
		if err != nil {
			return nil, err
		}
		defer rc.Close()

		if err := extractTarGz(rc, localPath); err != nil {
			return nil, fmt.Errorf("failed to extract index: %w", err)
		}

		return nil, nil
	})

	if err != nil {
		return "", err
	}

	c.touchLocalIndex(localPath, time.Now())
	return localPath, nil
}

func (c *CachedStore) touchLocalIndex(localPath string, now time.Time) {
	var evicted []string
	c.mu.Lock()
	if c.localIndexes == nil {
		c.localIndexes = make(map[string]time.Time)
	}
	c.localIndexes[localPath] = now
	for len(c.localIndexes) > maxLocalIndexDirs {
		var (
			oldestPath string
			oldestTime time.Time
		)
		for p, t := range c.localIndexes {
			if p == localPath {
				continue
			}
			if oldestPath == "" || t.Before(oldestTime) {
				oldestPath = p
				oldestTime = t
			}
		}
		if oldestPath == "" {
			break
		}
		delete(c.localIndexes, oldestPath)
		evicted = append(evicted, oldestPath)
	}
	c.mu.Unlock()

	for _, p := range evicted {
		_ = os.RemoveAll(p)
	}
}

// FormatTimePrefixedChunkID encodes a chunk's [minTime, maxTime] bounds into its
// chunk ID prefix ("ts_<minUnixNano>_<maxUnixNano>_<chunkID>") so query planning
// can time-prune chunks directly from ListChunks before reading metadata.json.
func FormatTimePrefixedChunkID(minTime, maxTime time.Time, chunkID string) string {
	if minTime.IsZero() || maxTime.IsZero() || strings.HasPrefix(chunkID, "ts_") {
		return chunkID
	}
	return fmt.Sprintf("ts_%d_%d_%s", minTime.UTC().UnixNano(), maxTime.UTC().UnixNano(), chunkID)
}

// ParseChunkTimeRangeFromID extracts the [minTime, maxTime] range from a
// time-prefixed chunk ID. It accepts both the legacy ts_<min>_<max>_<id>
// format and the native chunk-<min>_<max> IDs written by the ingester.
func ParseChunkTimeRangeFromID(chunkID string) (time.Time, time.Time, bool) {
	chunkID = filepath.Base(strings.TrimSuffix(filepath.ToSlash(strings.TrimSpace(chunkID)), "/"))
	var minStr, maxStr string
	switch {
	case strings.HasPrefix(chunkID, "ts_"):
		rest := strings.TrimPrefix(chunkID, "ts_")
		firstSep := strings.IndexByte(rest, '_')
		if firstSep <= 0 {
			return time.Time{}, time.Time{}, false
		}
		minStr = rest[:firstSep]
		rest = rest[firstSep+1:]
		secondSep := strings.IndexByte(rest, '_')
		if secondSep <= 0 {
			return time.Time{}, time.Time{}, false
		}
		maxStr = rest[:secondSep]
	case strings.HasPrefix(chunkID, "compact-"):
		// compact-<minNano>-<maxNano>-<sequence>
		rest := strings.TrimPrefix(chunkID, "compact-")
		parts := strings.SplitN(rest, "-", 3)
		if len(parts) < 3 {
			return time.Time{}, time.Time{}, false
		}
		minStr, maxStr = parts[0], parts[1]
	case strings.HasPrefix(chunkID, "chunk-"):
		// Native chunk-<minNano>-<maxNano>. Keep this strict so malformed IDs
		// with extra fields are not silently accepted.
		rest := strings.TrimPrefix(chunkID, "chunk-")
		if strings.Count(rest, "-") != 1 {
			return time.Time{}, time.Time{}, false
		}
		sep := strings.IndexByte(rest, '-')
		if sep <= 0 || sep == len(rest)-1 {
			return time.Time{}, time.Time{}, false
		}
		minStr = rest[:sep]
		maxStr = rest[sep+1:]
	default:
		return time.Time{}, time.Time{}, false
	}
	minNano, err := strconv.ParseInt(minStr, 10, 64)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	maxNano, err := strconv.ParseInt(maxStr, 10, 64)
	if err != nil || maxNano < minNano {
		return time.Time{}, time.Time{}, false
	}
	return time.Unix(0, minNano).UTC(), time.Unix(0, maxNano).UTC(), true
}

// extractTarGz extracts a gzipped tarball to a destination directory.
func extractTarGz(r io.Reader, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	gzr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, header.Name)
		// Guard against zip-slip: reject entries that escape the destination.
		if rel, relErr := filepath.Rel(destDir, target); relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry %q escapes destination directory", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}

package blob

import (
	"os"
	"strconv"
	"strings"
)

const (
	defaultBlobCacheBytes     = 256 << 20 // 256 MiB
	defaultBlobCacheMaxObject = 16 << 20  // 16 MiB
)

func int64Env(name string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return fallback
	}
	return v
}

// cachedBlob returns a full blob from the in-memory cache.
func (bb *BlobBackend) cachedBlob(contentID string) ([]byte, bool) {
	bb.blobCacheMu.Lock()
	defer bb.blobCacheMu.Unlock()
	data, ok := bb.blobCache[contentID]
	return data, ok
}

// putBlob caches a full blob with a simple FIFO byte budget. Blob content is
// immutable (content-addressed), so no invalidation is required.
func (bb *BlobBackend) putBlob(contentID string, data []byte) {
	if len(data) == 0 || int64(len(data)) > bb.blobCacheMaxObject || bb.blobCacheMaxBytes <= 0 {
		return
	}
	bb.blobCacheMu.Lock()
	defer bb.blobCacheMu.Unlock()
	if bb.blobCache == nil {
		bb.blobCache = make(map[string][]byte)
	}
	if _, exists := bb.blobCache[contentID]; exists {
		return
	}
	for bb.blobCacheBytes+int64(len(data)) > bb.blobCacheMaxBytes && len(bb.blobCacheOrder) > 0 {
		oldest := bb.blobCacheOrder[0]
		bb.blobCacheOrder = bb.blobCacheOrder[1:]
		if old, ok := bb.blobCache[oldest]; ok {
			bb.blobCacheBytes -= int64(len(old))
			delete(bb.blobCache, oldest)
		}
	}
	bb.blobCache[contentID] = data
	bb.blobCacheOrder = append(bb.blobCacheOrder, contentID)
	bb.blobCacheBytes += int64(len(data))
}

func (bb *BlobBackend) dropCachedBlob(contentID string) {
	bb.blobCacheMu.Lock()
	defer bb.blobCacheMu.Unlock()
	if old, ok := bb.blobCache[contentID]; ok {
		bb.blobCacheBytes -= int64(len(old))
		delete(bb.blobCache, contentID)
	}
}

// applyFetchRange slices a full blob according to the optional offset/length
// source_config keys. The fetcher still reads the whole archive entry (the
// packager provides O(1) file access, not intra-file ranges), but the network
// transfer and the node allocation are limited to the requested window.
func applyFetchRange(data []byte, cfg map[string]string) []byte {
	offset := int64(0)
	length := int64(0)
	if raw := strings.TrimSpace(cfg["offset"]); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
			offset = v
		}
	}
	if raw := strings.TrimSpace(cfg["length"]); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
			length = v
		}
	}
	if offset >= int64(len(data)) {
		return nil
	}
	data = data[offset:]
	if length > 0 && int64(len(data)) > length {
		data = data[:length]
	}
	return data
}

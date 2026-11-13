package blob

import (
	"fmt"
	"time"

	"github.com/radryc/packager"

	pkgstorage "github.com/radryc/packager/storage"
)

// archiveRef maps a blob hash to its location inside a packager archive.
type archiveRef struct {
	archivePath string // filesystem path to .pack file
	entryPath   string // path within the archive (the blob hash or file path)
}

// openArchive wraps a packager.ArchiveReader with LRU tracking.
type openArchive struct {
	reader   *packager.ArchiveReader
	store    pkgstorage.ObjectReader
	lastUsed time.Time
}

// getArchiveReader returns a cached or freshly opened ArchiveReader.
// If the local file is missing but cloud storage is configured, the archive
// is downloaded from the cloud and cached locally before opening.
func (bb *BlobBackend) getArchiveReader(archivePath string) (*packager.ArchiveReader, error) {
	bb.archiveMu.Lock()
	defer bb.archiveMu.Unlock()

	if cached, ok := bb.archiveCache[archivePath]; ok {
		cached.lastUsed = time.Now()
		return cached.reader, nil
	}

	if len(bb.archiveCache) >= maxOpenArchives {
		bb.evictOldestLocked()
	}

	// Try opening the local file first.
	store, err := pkgstorage.NewLocalFileReader(archivePath)
	if err != nil && bb.hasCloudDownload() {
		// Local cache miss — download from cloud and retry.
		if dlErr := bb.downloadFromCloud(archivePath); dlErr != nil {
			return nil, fmt.Errorf("open archive file (cloud fallback failed: %v): %w", dlErr, err)
		}
		store, err = pkgstorage.NewLocalFileReader(archivePath)
	}
	if err != nil {
		return nil, fmt.Errorf("open archive file: %w", err)
	}

	ar, err := packager.OpenArchive(store, bb.pipeline)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("open/decrypt archive: %w", err)
	}

	bb.archiveCache[archivePath] = &openArchive{
		reader:   ar,
		store:    store,
		lastUsed: time.Now(),
	}

	return ar, nil
}

func (bb *BlobBackend) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time

	for key, cached := range bb.archiveCache {
		if oldestKey == "" || cached.lastUsed.Before(oldestTime) {
			oldestKey = key
			oldestTime = cached.lastUsed
		}
	}

	if oldestKey != "" {
		cached := bb.archiveCache[oldestKey]
		cached.reader.Close()
		cached.store.Close()
		delete(bb.archiveCache, oldestKey)
	}
}

func (bb *BlobBackend) evictArchiveReader(archivePath string) {
	bb.archiveMu.Lock()
	defer bb.archiveMu.Unlock()

	if cached, ok := bb.archiveCache[archivePath]; ok {
		cached.reader.Close()
		cached.store.Close()
		delete(bb.archiveCache, archivePath)
	}
}

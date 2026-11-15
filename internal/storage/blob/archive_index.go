package blob

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/radryc/monofs/internal/storage"
	"github.com/radryc/packager"
	"github.com/radryc/packager/pipeline"

	pkgstorage "github.com/radryc/packager/storage"
)

// scanArchives discovers all .pack files on disk and indexes their contents.
func (bb *BlobBackend) scanArchives(archiveDir string) error {
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var totalFiles int64
	var totalBytes int64
	var scanErrors []error

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		storageID := entry.Name()
		storageDir := filepath.Join(archiveDir, storageID)
		bb.storageIDs[storageID] = true

		packs, err := filepath.Glob(filepath.Join(storageDir, "*.pack"))
		if err != nil {
			continue
		}

		for _, packPath := range packs {
			info, err := os.Stat(packPath)
			if err != nil {
				if bb.logger != nil {
					bb.logger.Warn("failed to stat archive during scan", "path", packPath, "error", err)
				}
				continue
			}
			totalBytes += info.Size()

			count, err := bb.indexArchive(packPath)
			if err != nil {
				scanErrors = append(scanErrors, fmt.Errorf("archive %s: %w", filepath.Base(packPath), err))
				if bb.logger != nil {
					bb.logger.Warn("failed to index archive during scan (wrong encryption key? corrupt file?)", "path", packPath, "error", err)
				}
				continue
			}
			totalFiles += int64(count)
			bb.storageBlobCounts[storageID] += int64(count)
			bb.archivePaths[packPath] = true
		}
	}

	if len(scanErrors) > 0 {
		hasDecrypt := false
		joined := errors.Join(scanErrors...)
		for _, e := range scanErrors {
			if errors.Is(e, pipeline.ErrDecryption) || errors.Is(e, pipeline.ErrDecompression) {
				hasDecrypt = true
				break
			}
		}
		if bb.logger != nil {
			if hasDecrypt {
				bb.logger.Error("some archives could not be decrypted (wrong encryption key?); continuing with remaining archives",
					"count", len(scanErrors),
					"errors", joined)
			} else {
				bb.logger.Error("some archives could not be indexed; continuing with remaining archives",
					"count", len(scanErrors),
					"errors", joined)
			}
		}
		// Do not fail initialization because of individual corrupt archives.
		// Once the encryption-key guard is in place, a global key mismatch is
		// caught before scanArchives runs. Skipping bad archives lets the
		// backend start and lets the cloud-backup scanner repair/backfill
		// whatever is still readable.
	}

	bb.stats.Store(&storage.BackendStats{
		CachedItems: totalFiles,
		CacheBytes:  totalBytes,
	})

	return nil
}

// indexArchive opens an archive and adds all its file entries to the blob index.
// Returns the number of files indexed.
func (bb *BlobBackend) indexArchive(archivePath string) (int, error) {
	store, err := pkgstorage.NewLocalFileReader(archivePath)
	if err != nil {
		return 0, fmt.Errorf("open archive %s: %w", archivePath, err)
	}

	ar, err := packager.OpenArchive(store, bb.pipeline)
	if err != nil {
		store.Close()
		return 0, fmt.Errorf("open/decrypt archive %s: %w", archivePath, err)
	}

	files := ar.ListFiles()
	var indexed int
	bb.mu.Lock()
	for _, entry := range files {
		if strings.Contains(entry, "/") || strings.Contains(entry, ".") {
			continue
		}
		key := normalizeBlobEntry(entry)
		if !isHexString(key) {
			continue
		}
		bb.blobIndex[key] = archiveRef{
			archivePath: archivePath,
			entryPath:   entry,
		}
		indexed++
	}
	bb.mu.Unlock()

	ar.Close()
	store.Close()

	return indexed, nil
}

// normalizeBlobEntry maps an archive entry name to the bare content hash used
// as the blob index key. Older archives may store "sha256:"-prefixed names.
func normalizeBlobEntry(name string) string {
	return strings.TrimPrefix(name, "sha256:")
}

// archiveHashes lists the blob hashes indexed in an archive. Only entries that
// look like content IDs are returned, matching indexArchive's filter.
func (bb *BlobBackend) archiveHashes(archivePath string) ([]string, error) {
	store, err := pkgstorage.NewLocalFileReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open archive %s: %w", archivePath, err)
	}
	defer store.Close()

	ar, err := packager.OpenArchive(store, bb.pipeline)
	if err != nil {
		return nil, fmt.Errorf("open/decrypt archive %s: %w", archivePath, err)
	}
	defer ar.Close()

	hashes := make([]string, 0)
	for _, entry := range ar.ListFiles() {
		if strings.Contains(entry, "/") || strings.Contains(entry, ".") {
			continue
		}
		key := normalizeBlobEntry(entry)
		if isHexString(key) {
			hashes = append(hashes, key)
		}
	}
	sort.Strings(hashes)
	return hashes, nil
}

// writeArchiveIndex writes the sidecar index atomically next to an archive.
func (bb *BlobBackend) writeArchiveIndex(archivePath string, hashes []string) error {
	if len(hashes) == 0 {
		return nil
	}
	unique := make([]string, 0, len(hashes))
	seen := make(map[string]struct{}, len(hashes))
	for _, hash := range hashes {
		if hash == "" {
			continue
		}
		if _, ok := seen[hash]; ok {
			continue
		}
		seen[hash] = struct{}{}
		unique = append(unique, hash)
	}
	sort.Strings(unique)
	data, err := json.Marshal(unique)
	if err != nil {
		return err
	}
	indexPath := archivePath + archiveIndexSuffix
	tmpPath := indexPath + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, indexPath)
}

func (bb *BlobBackend) readLocalArchiveIndex(archivePath string) ([]string, error) {
	data, err := os.ReadFile(archivePath + archiveIndexSuffix)
	if err != nil {
		return nil, err
	}
	var hashes []string
	if err := json.Unmarshal(data, &hashes); err != nil {
		return nil, err
	}
	return hashes, nil
}

package server

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"syscall"

	"github.com/radryc/monofs/internal/metastore"
)

// Directory entries are stored one KV record per child, keyed by
//
//	sha256(storageID || 0x00 || dirPath)   // 32-byte directory id
//	|| 0x00                                // separator (never in a filename)
//	|| entryName                           // raw UTF-8, no '/'
//
// so a directory's children are a contiguous, name-ordered prefix range that
// can be streamed and paged without materializing the whole directory.
// The repo identity is folded into the 32-byte hash so no long prefix is
// repeated per entry.
const dirEntryIDLen = sha256.Size
const dirEntryPrefixLen = dirEntryIDLen + 1
const dirEntryVersion = 1

// encodeDirEntry serializes an entry without its name (carried in the key) and
// without IsDir (derivable from the mode bits).
func encodeDirEntry(e dirIndexEntry) []byte {
	buf := make([]byte, 0, dirEntryPrefixLen+len(e.HashKey))
	var tmp [8]byte
	buf = append(buf, dirEntryVersion)
	binary.LittleEndian.PutUint32(tmp[:4], e.Mode)
	buf = append(buf, tmp[:4]...)
	binary.LittleEndian.PutUint64(tmp[:], e.Size)
	buf = append(buf, tmp[:]...)
	binary.LittleEndian.PutUint64(tmp[:], uint64(e.Mtime))
	buf = append(buf, tmp[:]...)
	hk := e.HashKey
	if len(hk) > 255 {
		hk = hk[:255]
	}
	buf = append(buf, byte(len(hk)))
	buf = append(buf, hk...)
	return buf
}

func decodeDirEntry(name string, data []byte) (dirIndexEntry, error) {
	if len(data) < 22 || data[0] != dirEntryVersion {
		return dirIndexEntry{}, fmt.Errorf("direntry: bad record for %q", name)
	}
	e := dirIndexEntry{Name: name}
	e.Mode = binary.LittleEndian.Uint32(data[1:5])
	e.Size = binary.LittleEndian.Uint64(data[5:13])
	e.Mtime = int64(binary.LittleEndian.Uint64(data[13:21]))
	hl := int(data[21])
	if len(data) < 22+hl {
		return dirIndexEntry{}, fmt.Errorf("direntry: truncated record for %q", name)
	}
	e.HashKey = string(data[22 : 22+hl])
	e.IsDir = e.Mode&uint32(syscall.S_IFDIR) != 0
	return e, nil
}

// prefixUpperBound returns the smallest key greater than every key with the
// given prefix, or nil when the prefix is all 0xff.
func prefixUpperBound(prefix []byte) []byte {
	end := make([]byte, len(prefix))
	copy(end, prefix)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

// dirEntryPrefix returns the scan prefix for a directory's children.
func dirEntryPrefix(storageID, dirPath string) []byte {
	sum := sha256.Sum256([]byte(storageID + "\x00" + dirPath))
	prefix := make([]byte, dirEntryPrefixLen)
	copy(prefix, sum[:])
	prefix[dirEntryIDLen] = 0x00
	return prefix
}

// makeDirEntryKey returns the full key for a directory child.
func makeDirEntryKey(storageID, dirPath, name string) []byte {
	prefix := dirEntryPrefix(storageID, dirPath)
	key := make([]byte, 0, len(prefix)+len(name))
	key = append(key, prefix...)
	key = append(key, name...)
	return key
}

func dirEntryNameFromKey(key []byte) string {
	if len(key) <= dirEntryPrefixLen {
		return ""
	}
	return string(key[dirEntryPrefixLen:])
}

func getDirEntry(tx metastore.Tx, storageID, dirPath, name string) (dirIndexEntry, bool, error) {
	value, err := tx.Get(bucketDirEntries, makeDirEntryKey(storageID, dirPath, name))
	if err != nil {
		if errors.Is(err, metastore.ErrKeyNotFound) {
			return dirIndexEntry{}, false, nil
		}
		return dirIndexEntry{}, false, err
	}
	e, decErr := decodeDirEntry(name, value)
	if decErr != nil {
		return dirIndexEntry{}, false, decErr
	}
	return e, true, nil
}

func deleteDirEntry(tx metastore.Tx, storageID, dirPath, name string) error {
	return tx.Delete(bucketDirEntries, makeDirEntryKey(storageID, dirPath, name))
}

func listDirEntries(tx metastore.Tx, storageID, dirPath string) ([]dirIndexEntry, error) {
	var out []dirIndexEntry
	err := tx.ScanPrefix(bucketDirEntries, dirEntryPrefix(storageID, dirPath), func(key, value []byte) error {
		e, decErr := decodeDirEntry(dirEntryNameFromKey(key), value)
		if decErr != nil {
			return nil // skip corrupt records
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

func hasDirEntries(tx metastore.Tx, storageID, dirPath string) (bool, error) {
	found := false
	err := tx.ScanPrefix(bucketDirEntries, dirEntryPrefix(storageID, dirPath), func(key, value []byte) error {
		found = true
		return metastore.ErrStopIteration
	})
	return found, err
}

func deleteDirEntries(tx metastore.Tx, storageID, dirPath string) (int, error) {
	var keys [][]byte
	err := tx.ScanPrefix(bucketDirEntries, dirEntryPrefix(storageID, dirPath), func(key, value []byte) error {
		keys = append(keys, append([]byte(nil), key...))
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, key := range keys {
		if err := tx.Delete(bucketDirEntries, key); err != nil {
			return 0, err
		}
	}
	return len(keys), nil
}

// accumulateDirectoryIndexEntry records the entry for every ancestor of
// filePath into pending, keyed by the encoded entry key. Callers accumulate a
// whole ingest batch and flush once so each entry is written exactly once.
func accumulateDirectoryIndexEntry(pending map[string]dirIndexEntry, storageID, filePath string, mode uint32, size uint64, mtime int64, isDir bool, hashKey string) {
	if filePath == "" {
		return
	}
	parts := strings.Split(filePath, "/")
	for i := 0; i < len(parts); i++ {
		var dirPath, entryName string
		var entryIsDir bool
		if i == 0 {
			dirPath = ""
			entryName = parts[0]
			entryIsDir = (i < len(parts)-1) || isDir
		} else {
			dirPath = strings.Join(parts[:i], "/")
			entryName = parts[i]
			entryIsDir = (i < len(parts)-1) || (isDir && i == len(parts)-1)
		}

		entry := dirIndexEntry{Name: entryName, IsDir: entryIsDir, Mtime: mtime}
		if !entryIsDir {
			entry.Mode = mode
			entry.Size = size
			entry.HashKey = hashKey
		} else if isDir && i == len(parts)-1 {
			entry.Mode = normalizeExplicitDirectoryMode(mode)
		} else {
			entry.Mode = inferDirectoryMode(mode)
		}

		key := string(makeDirEntryKey(storageID, dirPath, entryName))
		if existing, ok := pending[key]; ok {
			switch {
			case entry.IsDir && existing.IsDir:
				if existing.Mtime > entry.Mtime {
					entry.Mtime = existing.Mtime
				}
			case entry.IsDir && !existing.IsDir:
				// An existing real file entry wins over a directory marker.
				entry = existing
			}
		}
		pending[key] = entry
	}
}

// flushDirectoryIndexEntries writes accumulated entries in one pass.
func flushDirectoryIndexEntries(tx metastore.Tx, pending map[string]dirIndexEntry) error {
	for key, entry := range pending {
		if err := tx.Put(bucketDirEntries, []byte(key), encodeDirEntry(entry), 0); err != nil {
			return err
		}
	}
	return nil
}

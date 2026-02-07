// Package cache provides optional metadata caching using NutsDB.
package cache

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/nutsdb/nutsdb"
)

const (
	attrBucket = "attr_cache"
	dirBucket  = "dir_cache"
)

// DefaultAttrTTL is the default time-to-live for cached attributes.
const DefaultAttrTTL = 30 * time.Second

// DefaultDirTTL is the default time-to-live for cached directory listings.
const DefaultDirTTL = 30 * time.Second

// AttrEntry represents cached file attributes.
type AttrEntry struct {
	Ino   uint64 `json:"ino"`
	Mode  uint32 `json:"mode"`
	Size  uint64 `json:"size"`
	Mtime int64  `json:"mtime"`
	Atime int64  `json:"atime"`
	Ctime int64  `json:"ctime"`
	Nlink uint32 `json:"nlink"`
	Uid   uint32 `json:"uid"`
	Gid   uint32 `json:"gid"`
}

// DirEntry represents a cached directory entry.
type DirEntry struct {
	Name string `json:"name"`
	Mode uint32 `json:"mode"`
	Ino  uint64 `json:"ino"`
}

// Cache provides metadata caching with NutsDB.
type Cache struct {
	db     *nutsdb.DB
	logger *slog.Logger
}

// New creates a new cache instance at the specified directory.
func New(dir string, logger *slog.Logger) (*Cache, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "cache")

	db, err := nutsdb.Open(
		nutsdb.DefaultOptions,
		nutsdb.WithDir(dir),
		nutsdb.WithSegmentSize(64*1024*1024), // 64MB segments
		nutsdb.WithEntryIdxMode(nutsdb.HintKeyAndRAMIdxMode), // Use hint file for faster startup (only keys in RAM)
		nutsdb.WithRWMode(nutsdb.MMap), // Use mmap for faster reads
	)
	if err != nil {
		logger.Error("failed to open cache database", "dir", dir, "error", err)
		return nil, err
	}

	// Create buckets
	err = db.Update(func(tx *nutsdb.Tx) error {
		if err := tx.NewBucket(nutsdb.DataStructureBTree, attrBucket); err != nil && err != nutsdb.ErrBucketAlreadyExist {
			return err
		}
		if err := tx.NewBucket(nutsdb.DataStructureBTree, dirBucket); err != nil && err != nutsdb.ErrBucketAlreadyExist {
			return err
		}
		return nil
	})
	if err != nil {
		logger.Error("failed to create cache buckets", "error", err)
		db.Close()
		return nil, err
	}

	logger.Info("cache initialized", "dir", dir)
	return &Cache{db: db, logger: logger}, nil
}

func (c *Cache) cacheKey(path string) []byte {
	if path == "" {
		return []byte("/")
	}
	return []byte(path)
}

// GetAttr retrieves cached attributes for a path.
func (c *Cache) GetAttr(path string) (*AttrEntry, error) {
	var entry AttrEntry
	err := c.db.View(func(tx *nutsdb.Tx) error {
		val, err := tx.Get(attrBucket, c.cacheKey(path))
		if err != nil {
			return err
		}
		return json.Unmarshal(val, &entry)
	})
	if err != nil {
		return nil, err
	}
	c.logger.Debug("cache hit", "type", "attr", "path", path)
	return &entry, nil
}

// PutAttr stores attributes for a path with TTL.
func (c *Cache) PutAttr(path string, entry *AttrEntry) error {
	ttlSec := uint32(DefaultAttrTTL.Seconds())
	err := c.db.Update(func(tx *nutsdb.Tx) error {
		data, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		return tx.Put(attrBucket, c.cacheKey(path), data, ttlSec)
	})
	if err != nil {
		c.logger.Warn("failed to cache attr", "path", path, "error", err)
		return err
	}
	c.logger.Debug("cached attr", "path", path, "ttl", ttlSec)
	return nil
}

// GetDir retrieves cached directory entries for a path.
func (c *Cache) GetDir(path string) ([]fuse.DirEntry, error) {
	var entries []DirEntry
	err := c.db.View(func(tx *nutsdb.Tx) error {
		val, err := tx.Get(dirBucket, c.cacheKey(path))
		if err != nil {
			return err
		}
		return json.Unmarshal(val, &entries)
	})
	if err != nil {
		return nil, err
	}

	// Convert to fuse.DirEntry
	result := make([]fuse.DirEntry, len(entries))
	for i, e := range entries {
		result[i] = fuse.DirEntry{
			Name: e.Name,
			Mode: e.Mode,
			Ino:  e.Ino,
		}
	}
	c.logger.Debug("cache hit", "type", "dir", "path", path, "entries", len(result))
	return result, nil
}

// PutDir stores directory entries for a path with TTL.
func (c *Cache) PutDir(path string, entries []DirEntry) error {
	ttlSec := uint32(DefaultDirTTL.Seconds())
	err := c.db.Update(func(tx *nutsdb.Tx) error {
		data, err := json.Marshal(entries)
		if err != nil {
			return err
		}
		return tx.Put(dirBucket, c.cacheKey(path), data, ttlSec)
	})
	if err != nil {
		c.logger.Warn("failed to cache dir", "path", path, "error", err)
		return err
	}
	c.logger.Debug("cached dir", "path", path, "entries", len(entries), "ttl", ttlSec)
	return nil
}

// Invalidate removes a path from both caches.
func (c *Cache) Invalidate(path string) {
	c.db.Update(func(tx *nutsdb.Tx) error {
		tx.Delete(attrBucket, c.cacheKey(path))
		tx.Delete(dirBucket, c.cacheKey(path))
		return nil
	})
	c.logger.Debug("invalidated cache", "path", path)
}

// Close closes the cache database.
func (c *Cache) Close() error {
	c.logger.Info("closing cache")
	return c.db.Close()
}

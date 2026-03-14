package sharding

import (
	"crypto/sha256"
	"encoding/hex"
)

// GenerateStorageID creates a SHA-256 hash of the display path.
// This provides a consistent, fixed-length internal identifier regardless of
// the display path format (path-based or simple string).
//
// This function is used by both the router and client to ensure consistent
// storage ID generation across the cluster.
func GenerateStorageID(displayPath string) string {
	hash := sha256.Sum256([]byte(displayPath))
	return hex.EncodeToString(hash[:])
}

// BuildShardKey builds the sharding key in the format "storageID:filePath".
// This format is used for consistent hashing across the cluster.
//
// The sharding key format must be consistent between router and client
// for proper HRW (Rendezvous) hashing to work correctly.
func BuildShardKey(storageID, filePath string) string {
	return storageID + ":" + filePath
}

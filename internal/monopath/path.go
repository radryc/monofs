// Package monopath centralizes MonoFS path semantics shared by clients and
// future native-mount work. It defines how user-visible paths split into
// display paths and file paths, and how those paths become shard keys.
package monopath

import (
	"strings"

	"github.com/radryc/monofs/internal/sharding"
)

// SplitDisplayPath splits a user-visible MonoFS path into displayPath and the
// path within that mounted namespace.
func SplitDisplayPath(fullPath string) (displayPath, filePath string, ok bool) {
	if fullPath == "" || fullPath == "/" {
		return "", "", false
	}

	trimmed := strings.Trim(fullPath, "/")
	if trimmed == "" {
		return "", "", false
	}

	parts := strings.Split(trimmed, "/")
	switch parts[0] {
	case "dependency", "guardian-system":
		displayPath = parts[0]
		if len(parts) > 1 {
			filePath = strings.Join(parts[1:], "/")
		}
		return displayPath, filePath, true
	case "guardian", "doctor":
		if len(parts) < 2 {
			return "", "", false
		}
		displayPath = strings.Join(parts[:2], "/")
		if len(parts) > 2 {
			filePath = strings.Join(parts[2:], "/")
		}
		return displayPath, filePath, true
	default:
		if len(parts) < 3 {
			return "", "", false
		}
		displayPath = strings.Join(parts[:3], "/")
		if len(parts) > 3 {
			filePath = strings.Join(parts[3:], "/")
		}
		return displayPath, filePath, true
	}
}

// BuildShardKey builds the exact shard key the client uses to match router
// ingestion sharding. Repo roots become the storage ID; files become
// "storageID:filePath"; non-repo intermediate paths pass through unchanged.
func BuildShardKey(fullPath string) string {
	if fullPath == "" || fullPath == "/" {
		return fullPath
	}

	displayPath, filePath, ok := SplitDisplayPath(fullPath)
	if !ok {
		return fullPath
	}

	storageID := sharding.GenerateStorageID(displayPath)
	if filePath == "" {
		return storageID
	}

	return sharding.BuildShardKey(storageID, filePath)
}

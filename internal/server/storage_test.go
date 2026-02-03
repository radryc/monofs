// Package server provides tests for storage ID functionality.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestMakeStorageKey verifies storage key generation.
func TestMakeStorageKey(t *testing.T) {
	tests := []struct {
		name      string
		storageID string
		filePath  string
	}{
		{
			name:      "simple path",
			storageID: "abc123",
			filePath:  "README.md",
		},
		{
			name:      "nested path",
			storageID: "def456",
			filePath:  "src/main.go",
		},
		{
			name:      "empty file path",
			storageID: "ghi789",
			filePath:  "",
		},
		{
			name:      "long storage ID",
			storageID: "a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7r8s9t0u1v2w3x4y5z6",
			filePath:  "deep/nested/path/to/file.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := makeStorageKey(tt.storageID, tt.filePath)

			// Verify it's a valid hex string
			if len(key) != 64 {
				t.Errorf("expected 64-char hex string, got %d chars", len(key))
			}

			// Verify it's deterministic
			key2 := makeStorageKey(tt.storageID, tt.filePath)
			if string(key) != string(key2) {
				t.Error("makeStorageKey is not deterministic")
			}

			// Verify it actually hashes the composite key
			compositeKey := tt.storageID + ":" + tt.filePath
			expectedHash := sha256.Sum256([]byte(compositeKey))
			expectedHex := hex.EncodeToString(expectedHash[:])
			if string(key) != expectedHex {
				t.Errorf("hash mismatch: expected %s, got %s", expectedHex, string(key))
			}
		})
	}
}

// TestMakeFullPath verifies full path construction.
func TestMakeFullPath(t *testing.T) {
	tests := []struct {
		name        string
		displayPath string
		filePath    string
		expected    string
	}{
		{
			name:        "simple file",
			displayPath: "myrepo",
			filePath:    "README.md",
			expected:    "myrepo/README.md",
		},
		{
			name:        "nested display path",
			displayPath: "github.com/user/repo",
			filePath:    "src/main.go",
			expected:    "github.com/user/repo/src/main.go",
		},
		{
			name:        "empty file path",
			displayPath: "myrepo",
			filePath:    "",
			expected:    "myrepo",
		},
		{
			name:        "deep nesting",
			displayPath: "a/b/c",
			filePath:    "d/e/f.txt",
			expected:    "a/b/c/d/e/f.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := makeFullPath(tt.displayPath, tt.filePath)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// TestStorageIDUniqueness ensures different display paths produce different storage IDs.
func TestStorageIDUniqueness(t *testing.T) {
	displayPaths := []string{
		"myrepo",
		"github.com/user/repo",
		"gitlab_com/org/project",
		"a/b/c/d/e",
		"simple",
	}

	seen := make(map[string]string)
	for _, path := range displayPaths {
		hash := sha256.Sum256([]byte(path))
		storageID := hex.EncodeToString(hash[:])

		if existing, found := seen[storageID]; found {
			t.Errorf("collision: %q and %q produce same storage ID %s", path, existing, storageID)
		}
		seen[storageID] = path

		// Verify length
		if len(storageID) != 64 {
			t.Errorf("storage ID should be 64 chars, got %d for %q", len(storageID), path)
		}
	}
}

// TestExtractDirPath verifies directory path extraction from file paths.
func TestExtractDirPath(t *testing.T) {
	tests := []struct {
		name     string
		filePath string
		expected string
	}{
		{
			name:     "root file",
			filePath: "README.md",
			expected: "",
		},
		{
			name:     "nested file",
			filePath: "src/main.go",
			expected: "src",
		},
		{
			name:     "deeply nested file",
			filePath: "a/b/c/d/file.txt",
			expected: "a/b/c/d",
		},
		{
			name:     "empty path",
			filePath: "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractDirPath(tt.filePath)
			if result != tt.expected {
				t.Errorf("extractDirPath(%q) = %q; want %q", tt.filePath, result, tt.expected)
			}
		})
	}
}

// TestExtractFileName verifies filename extraction from file paths.
func TestExtractFileName(t *testing.T) {
	tests := []struct {
		name     string
		filePath string
		expected string
	}{
		{
			name:     "root file",
			filePath: "README.md",
			expected: "README.md",
		},
		{
			name:     "nested file",
			filePath: "src/main.go",
			expected: "main.go",
		},
		{
			name:     "deeply nested file",
			filePath: "a/b/c/d/file.txt",
			expected: "file.txt",
		},
		{
			name:     "empty path",
			filePath: "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractFileName(tt.filePath)
			if result != tt.expected {
				t.Errorf("extractFileName(%q) = %q; want %q", tt.filePath, result, tt.expected)
			}
		})
	}
}

// TestMakeDirIndexKey verifies directory index key generation.
func TestMakeDirIndexKey(t *testing.T) {
	storageID := "abcd1234"
	dirPath := "src/internal"

	// Test determinism
	key1 := makeDirIndexKey(storageID, dirPath)
	key2 := makeDirIndexKey(storageID, dirPath)

	if string(key1) != string(key2) {
		t.Errorf("makeDirIndexKey not deterministic: %s != %s", string(key1), string(key2))
	}

	// Test uniqueness for different paths
	key3 := makeDirIndexKey(storageID, "src/other")
	if string(key1) == string(key3) {
		t.Errorf("makeDirIndexKey collision: same key for different paths")
	}

	// Test storage ID separation
	key4 := makeDirIndexKey("different_id", dirPath)
	if string(key1) == string(key4) {
		t.Errorf("makeDirIndexKey collision: same key for different storage IDs")
	}

	// Test format (should be storageID:hash(dirPath))
	keyStr := string(key1)
	dirHash := sha256.Sum256([]byte(dirPath))
	expected := storageID + ":" + hex.EncodeToString(dirHash[:])
	if keyStr != expected {
		t.Errorf("makeDirIndexKey format incorrect: expected %q, got %q", expected, keyStr)
	}
}

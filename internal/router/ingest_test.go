// Package router provides tests for ingestion functionality.
package router

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestGenerateStorageID verifies storage ID generation from display paths.
func TestGenerateStorageID(t *testing.T) {
	tests := []struct {
		name        string
		displayPath string
	}{
		{
			name:        "simple repo",
			displayPath: "myrepo",
		},
		{
			name:        "github path",
			displayPath: "github.com/owner/repo",
		},
		{
			name:        "gitlab path",
			displayPath: "gitlab.com/org/project",
		},
		{
			name:        "nested path",
			displayPath: "a/b/c/d",
		},
		{
			name:        "single char",
			displayPath: "x",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storageID := generateStorageID(tt.displayPath)

			// Verify it's a valid 64-char hex string
			if len(storageID) != 64 {
				t.Errorf("expected 64-char hex string, got %d chars", len(storageID))
			}

			// Verify it's deterministic
			storageID2 := generateStorageID(tt.displayPath)
			if storageID != storageID2 {
				t.Error("generateStorageID is not deterministic")
			}

			// Verify it matches SHA-256
			hash := sha256.Sum256([]byte(tt.displayPath))
			expected := hex.EncodeToString(hash[:])
			if storageID != expected {
				t.Errorf("hash mismatch: expected %s, got %s", expected, storageID)
			}
		})
	}
}

// TestNormalizeRepoID verifies URL-to-path normalization.
func TestNormalizeRepoID(t *testing.T) {
	tests := []struct {
		name     string
		repoURL  string
		expected string
	}{
		{
			name:     "github https",
			repoURL:  "https://github.com/owner/repo",
			expected: "github.com/owner/repo",
		},
		{
			name:     "github with .git suffix",
			repoURL:  "https://github.com/owner/repo.git",
			expected: "github.com/owner/repo",
		},
		{
			name:     "gitlab",
			repoURL:  "https://gitlab.com/group/project",
			expected: "gitlab.com/group/project",
		},
		{
			name:     "git protocol",
			repoURL:  "git@github.com:owner/repo.git",
			expected: "git@github.com:owner/repo.git", // Invalid URL format, returns as-is
		},
		{
			name:     "custom domain",
			repoURL:  "https://git.company.com/team/service",
			expected: "git.company.com/team/service",
		},
		{
			name:     "with port",
			repoURL:  "https://github.com:443/owner/repo",
			expected: "github.com:443/owner/repo",
		},
		{
			name:     "go module with version",
			repoURL:  "github.com/google/uuid@v1.3.0",
			expected: "github.com/google/uuid@v1.3.0",
		},
		{
			name:     "go module with version - different domain",
			repoURL:  "golang.org/x/crypto@v0.17.0",
			expected: "golang.org/x/crypto@v0.17.0",
		},
		{
			name:     "go module without version",
			repoURL:  "github.com/gin-gonic/gin",
			expected: "github.com/gin-gonic/gin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeRepoID(tt.repoURL)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// TestStorageIDCollisionResistance ensures different inputs produce different IDs.
func TestStorageIDCollisionResistance(t *testing.T) {
	testCases := []string{
		"repo",
		"repo/",
		"repo/path",
		"repo-path",
		"repo_path",
		"github.com/a/b",
		"github.com/a/c",
		"gitlab.com/a/b",
	}

	seen := make(map[string]string)
	for _, input := range testCases {
		storageID := generateStorageID(input)
		if existing, found := seen[storageID]; found {
			t.Errorf("collision: %q and %q produce same storage ID", input, existing)
		}
		seen[storageID] = input
	}
}

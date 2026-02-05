package client

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/radryc/monofs/internal/sharding"
)

// testGenerateStorageID is a test helper that matches the router's generateStorageID
func testGenerateStorageID(displayPath string) string {
	hash := sha256.Sum256([]byte(displayPath))
	return hex.EncodeToString(hash[:])
}

// TestClientRouterShardingMatch verifies that client and router use the same sharding keys.
// This is CRITICAL - if client and router disagree on which node has a file,
// lookups will query the wrong node and files will be "invisible".
func TestClientRouterShardingMatch(t *testing.T) {
	// Setup mock cluster with 5 nodes (matching production)
	nodes := []sharding.Node{
		{ID: "node1", Address: "localhost:9001", Weight: 100, Healthy: true},
		{ID: "node2", Address: "localhost:9002", Weight: 100, Healthy: true},
		{ID: "node3", Address: "localhost:9003", Weight: 100, Healthy: true},
		{ID: "node4", Address: "localhost:9004", Weight: 100, Healthy: true},
		{ID: "node5", Address: "localhost:9005", Weight: 100, Healthy: true},
	}
	hrw := sharding.NewHRW(nodes)

	// Test cases: file paths and expected shard keys
	// NOTE: storageID is now a SHA-256 hash of the displayPath (matches router)
	tests := []struct {
		name        string
		fullPath    string // What FUSE client sees
		displayPath string // Repository display path
		filePath    string // Path within repo
	}{
		{
			name:        "linux kernel README",
			fullPath:    "github_com/torvalds/linux/README",
			displayPath: "github_com/torvalds/linux",
			filePath:    "README",
		},
		{
			name:        "nested arch file",
			fullPath:    "github_com/torvalds/linux/arch/x86/Makefile",
			displayPath: "github_com/torvalds/linux",
			filePath:    "arch/x86/Makefile",
		},
		{
			name:        "vue README",
			fullPath:    "github_com/vuejs/vue/README.md",
			displayPath: "github_com/vuejs/vue",
			filePath:    "README.md",
		},
		{
			name:        "deeply nested file",
			fullPath:    "github_com/owner/repo/a/b/c/d/file.go",
			displayPath: "github_com/owner/repo",
			filePath:    "a/b/c/d/file.go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Client builds shard key from full path
			clientKey := buildShardKey(tt.fullPath)

			// Router builds shard key during ingestion using SHA-256 hash of displayPath
			storageID := testGenerateStorageID(tt.displayPath)
			routerKey := storageID + ":" + tt.filePath

			// CRITICAL: Keys MUST match
			if clientKey != routerKey {
				t.Errorf("SHARD KEY MISMATCH!\nClient key: %q\nRouter key: %q\nThis will cause files to be invisible!",
					clientKey, routerKey)
			}

			// Verify both route to same node
			clientNode := hrw.GetNode(clientKey)
			routerNode := hrw.GetNode(routerKey)

			if clientNode.ID != routerNode.ID {
				t.Errorf("CLIENT AND ROUTER ROUTE TO DIFFERENT NODES!\nClient -> %s\nRouter -> %s\nFile will be invisible!",
					clientNode.ID, routerNode.ID)
			}

			t.Logf("✓ %s -> node %s (key: %s)", tt.fullPath, clientNode.ID, clientKey)
		})
	}
}

// TestDirectoryShardKey verifies directory paths don't break sharding
func TestDirectoryShardKey(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		checkFormat func(string) bool
		wantSame    bool // Should return same as input for special dirs
		description string
	}{
		{
			name: "repo root - becomes hashed storage ID",
			path: "github_com/owner/repo",
			checkFormat: func(key string) bool {
				// Should be a 64-char hex string (SHA-256 hash)
				return len(key) == 64 && isHexString(key)
			},
			wantSame:    false,
			description: "repo root should become SHA-256 hashed storageID",
		},
		{
			name: "intermediate dir - github_com",
			path: "github_com",
			checkFormat: func(key string) bool {
				return key == "github_com"
			},
			wantSame:    true,
			description: "intermediate dirs pass through unchanged",
		},
		{
			name: "intermediate dir - owner level",
			path: "github_com/owner",
			checkFormat: func(key string) bool {
				return key == "github_com/owner"
			},
			wantSame:    true,
			description: "intermediate dirs pass through unchanged",
		},
		{
			name: "subdirectory in repo",
			path: "github_com/owner/repo/subdir",
			checkFormat: func(key string) bool {
				// Should be "sha256hash:subdir" format
				parts := strings.Split(key, ":")
				if len(parts) != 2 {
					return false
				}
				return len(parts[0]) == 64 && isHexString(parts[0]) && parts[1] == "subdir"
			},
			wantSame:    false,
			description: "subdirs become sha256hash:relativePath",
		},
		{
			name: "root",
			path: "/",
			checkFormat: func(key string) bool {
				return key == "/"
			},
			wantSame:    true,
			description: "root passes through unchanged",
		},
		{
			name: "empty",
			path: "",
			checkFormat: func(key string) bool {
				return key == ""
			},
			wantSame:    true,
			description: "empty passes through unchanged",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := buildShardKey(tt.path)

			if !tt.checkFormat(key) {
				t.Errorf("buildShardKey(%q) = %q, format check failed: %s", tt.path, key, tt.description)
			}

			if tt.wantSame && key != tt.path {
				t.Errorf("Expected key to equal input for special directory, got %q != %q", key, tt.path)
			}

			t.Logf("buildShardKey(%q) = %q ✓", tt.path, key)
		})
	}
}

// isHexString checks if a string contains only hexadecimal characters
func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// TestShardingConsistency verifies same file in different repos goes to different nodes
func TestShardingConsistency(t *testing.T) {
	nodes := []sharding.Node{
		{ID: "node1", Address: "localhost:9001", Weight: 100, Healthy: true},
		{ID: "node2", Address: "localhost:9002", Weight: 100, Healthy: true},
		{ID: "node3", Address: "localhost:9003", Weight: 100, Healthy: true},
		{ID: "node4", Address: "localhost:9004", Weight: 100, Healthy: true},
		{ID: "node5", Address: "localhost:9005", Weight: 100, Healthy: true},
	}
	hrw := sharding.NewHRW(nodes)

	// Same filename (README.md) in different repos
	paths := []struct {
		fullPath string
		repo     string
	}{
		{"github_com/torvalds/linux/README.md", "github_com/torvalds/linux"},
		{"github_com/vuejs/vue/README.md", "github_com/vuejs/vue"},
		{"github_com/golang/go/README.md", "github_com/golang/go"},
	}

	nodeAssignments := make(map[string]string) // path -> nodeID

	for _, p := range paths {
		key := buildShardKey(p.fullPath)
		node := hrw.GetNode(key)
		nodeAssignments[p.fullPath] = node.ID
		t.Logf("%s -> %s (key: %s)", p.fullPath, node.ID, key)
	}

	// Verify not all going to same node (that would indicate broken sharding)
	uniqueNodes := make(map[string]bool)
	for _, nodeID := range nodeAssignments {
		uniqueNodes[nodeID] = true
	}

	if len(uniqueNodes) == 1 {
		t.Error("All README.md files routed to same node - sharding may be broken")
	}

	t.Logf("README.md files distributed across %d different nodes ✓", len(uniqueNodes))
}

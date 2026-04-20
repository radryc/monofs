package monopath

import (
	"testing"

	"github.com/radryc/monofs/internal/sharding"
)

func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func TestSplitDisplayPath(t *testing.T) {
	tests := []struct {
		name        string
		fullPath    string
		displayPath string
		filePath    string
		ok          bool
	}{
		{
			name:        "repo file",
			fullPath:    "github.com/owner/repo/file.txt",
			displayPath: "github.com/owner/repo",
			filePath:    "file.txt",
			ok:          true,
		},
		{
			name:        "repo root",
			fullPath:    "github.com/owner/repo",
			displayPath: "github.com/owner/repo",
			filePath:    "",
			ok:          true,
		},
		{
			name:        "guardian partition file",
			fullPath:    "guardian/monofs-local/intents/storage-core.yaml",
			displayPath: "guardian/monofs-local",
			filePath:    "intents/storage-core.yaml",
			ok:          true,
		},
		{
			name:        "doctor namespace file",
			fullPath:    "doctor/v1/catalog/manifests/traces/default/2026-04-09/15/trace-1.json",
			displayPath: "doctor/v1",
			filePath:    "catalog/manifests/traces/default/2026-04-09/15/trace-1.json",
			ok:          true,
		},
		{
			name:        "guardian-system archive",
			fullPath:    "guardian-system/.archive/monofs-local/storage-core/rev-1/state.json",
			displayPath: "guardian-system",
			filePath:    ".archive/monofs-local/storage-core/rev-1/state.json",
			ok:          true,
		},
		{
			name:        "dependency file",
			fullPath:    "dependency/go/pkg/mod/cache/download/example.com/mod/@v/v1.0.0.zip",
			displayPath: "dependency",
			filePath:    "go/pkg/mod/cache/download/example.com/mod/@v/v1.0.0.zip",
			ok:          true,
		},
		{
			name:     "guardian top-level namespace only",
			fullPath: "guardian",
			ok:       false,
		},
		{
			name:     "doctor top-level namespace only",
			fullPath: "doctor",
			ok:       false,
		},
		{
			name:     "intermediate repo host dir",
			fullPath: "github.com",
			ok:       false,
		},
		{
			name:     "empty path",
			fullPath: "",
			ok:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			displayPath, filePath, ok := SplitDisplayPath(tt.fullPath)
			if ok != tt.ok {
				t.Fatalf("SplitDisplayPath(%q) ok = %v, want %v", tt.fullPath, ok, tt.ok)
			}
			if !ok {
				return
			}
			if displayPath != tt.displayPath {
				t.Fatalf("SplitDisplayPath(%q) displayPath = %q, want %q", tt.fullPath, displayPath, tt.displayPath)
			}
			if filePath != tt.filePath {
				t.Fatalf("SplitDisplayPath(%q) filePath = %q, want %q", tt.fullPath, filePath, tt.filePath)
			}
		})
	}
}

func TestBuildShardKey(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		validate func(string) bool
	}{
		{
			name: "repo root becomes storage id",
			path: "github.com/owner/repo",
			validate: func(key string) bool {
				return len(key) == 64 && isHexString(key)
			},
		},
		{
			name: "intermediate path passes through",
			path: "github.com",
			validate: func(key string) bool {
				return key == "github.com"
			},
		},
		{
			name: "file path becomes storage and file path",
			path: "github.com/owner/repo/pkg/file.go",
			validate: func(key string) bool {
				return len(key) == 64+1+len("pkg/file.go")
			},
		},
		{
			name: "guardian file becomes partition storage and file path",
			path: "guardian/monofs-local/intents/storage-core.yaml",
			validate: func(key string) bool {
				return len(key) == 64+1+len("intents/storage-core.yaml")
			},
		},
		{
			name: "root slash passes through",
			path: "/",
			validate: func(key string) bool {
				return key == "/"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := BuildShardKey(tt.path)
			if !tt.validate(key) {
				t.Fatalf("BuildShardKey(%q) returned unexpected key %q", tt.path, key)
			}
		})
	}
}

func TestClientRouterShardingMatch(t *testing.T) {
	nodes := []sharding.Node{
		{ID: "node1", Address: "localhost:9001", Weight: 100, Healthy: true},
		{ID: "node2", Address: "localhost:9002", Weight: 100, Healthy: true},
		{ID: "node3", Address: "localhost:9003", Weight: 100, Healthy: true},
	}
	hrw := sharding.NewHRW(nodes)

	tests := []struct {
		name        string
		fullPath    string
		displayPath string
		filePath    string
	}{
		{
			name:        "repo file",
			fullPath:    "github.com/owner/repo/pkg/file.go",
			displayPath: "github.com/owner/repo",
			filePath:    "pkg/file.go",
		},
		{
			name:        "guardian file",
			fullPath:    "guardian/monofs-local/intents/storage-core.yaml",
			displayPath: "guardian/monofs-local",
			filePath:    "intents/storage-core.yaml",
		},
		{
			name:        "dependency file",
			fullPath:    "dependency/a/b/c.txt",
			displayPath: "dependency",
			filePath:    "a/b/c.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientKey := BuildShardKey(tt.fullPath)
			routerKey := sharding.BuildShardKey(sharding.GenerateStorageID(tt.displayPath), tt.filePath)

			if clientKey != routerKey {
				t.Fatalf("client key %q != router key %q", clientKey, routerKey)
			}

			clientNode := hrw.GetNode(clientKey)
			routerNode := hrw.GetNode(routerKey)
			if clientNode == nil || routerNode == nil {
				t.Fatal("expected nodes for both client and router keys")
			}
			if clientNode.ID != routerNode.ID {
				t.Fatalf("client routed to %s, router routed to %s", clientNode.ID, routerNode.ID)
			}
		})
	}
}

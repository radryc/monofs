package cargo

import (
	"strings"
	"testing"

	"github.com/radryc/monofs/internal/buildlayout"
)

func TestCargoMapper_Type(t *testing.T) {
	mapper := NewCargoMapper()
	if got := mapper.Type(); got != "cargo" {
		t.Errorf("Type() = %v, want %v", got, "cargo")
	}
}

func TestCargoMapper_Matches(t *testing.T) {
	mapper := NewCargoMapper()

	tests := []struct {
		name        string
		info        buildlayout.RepoInfo
		wantMatches bool
	}{
		{
			name: "matches cargo ingestion type",
			info: buildlayout.RepoInfo{
				IngestionType: "cargo",
			},
			wantMatches: true,
		},
		{
			name: "does not match npm ingestion type",
			info: buildlayout.RepoInfo{
				IngestionType: "npm",
			},
			wantMatches: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapper.Matches(tt.info); got != tt.wantMatches {
				t.Errorf("Matches() = %v, want %v", got, tt.wantMatches)
			}
		})
	}
}

func TestCargoMapper_MapPaths_RealSerdeStructure(t *testing.T) {
	mapper := NewCargoMapper()

	// Simulate real serde@1.0.152 crate structure
	info := buildlayout.RepoInfo{
		DisplayPath:   "serde@1.0.152",
		StorageID:     "test-storage-id",
		Source:        "serde@1.0.152",
		Ref:           "1.0.152",
		IngestionType: "cargo",
		FetchType:     "cargo",
	}

	files := []buildlayout.FileInfo{
		{Path: "Cargo.toml", BlobHash: "hash1", Size: 2500, Mode: 0644},
		{Path: "Cargo.toml.orig", BlobHash: "hash2", Size: 2800, Mode: 0644},
		{Path: "src/lib.rs", BlobHash: "hash3", Size: 50000, Mode: 0644},
		{Path: "src/de/mod.rs", BlobHash: "hash4", Size: 20000, Mode: 0644},
		{Path: "src/ser/mod.rs", BlobHash: "hash5", Size: 15000, Mode: 0644},
		{Path: "README.md", BlobHash: "hash6", Size: 3000, Mode: 0644},
		{Path: "LICENSE-MIT", BlobHash: "hash7", Size: 1200, Mode: 0644},
		{Path: "LICENSE-APACHE", BlobHash: "hash8", Size: 10000, Mode: 0644},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths() error = %v", err)
	}

	// With hard links, we get 2x entries: canonical path + .cargo/ path
	expectedEntries := len(files) * 2
	if len(entries) != expectedEntries {
		t.Errorf("MapPaths() returned %d entries, want %d (2x for hard links)", len(entries), expectedEntries)
	}

	// Verify we have both canonical and .cargo paths
	hasCanonical := false
	hasCargo := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.VirtualDisplayPath, "crates.io/") {
			hasCanonical = true
		}
		if entry.VirtualDisplayPath == ".cargo/registry/src/serde-1.0.152" {
			hasCargo = true
		}
	}
	if !hasCanonical {
		t.Error("Missing canonical path entries (crates.io/)")
	}
	if !hasCargo {
		t.Error("Missing .cargo/registry/src/ hard link entries")
	}

	// Verify nested file paths are preserved
	nestedFileFound := false
	for _, entry := range entries {
		if entry.VirtualFilePath == "src/lib.rs" {
			nestedFileFound = true
			if entry.OriginalFilePath != "src/lib.rs" {
				t.Errorf("OriginalFilePath = %v, want src/lib.rs", entry.OriginalFilePath)
			}
		}
	}
	if !nestedFileFound {
		t.Error("src/lib.rs file not found in mapped entries")
	}
}

func TestCargoMapper_MapPaths_TokioAsyncRuntime(t *testing.T) {
	mapper := NewCargoMapper()

	// Simulate real tokio@1.35.1 crate structure
	info := buildlayout.RepoInfo{
		DisplayPath:   "tokio@1.35.1",
		StorageID:     "test-storage-id",
		Source:        "tokio@1.35.1",
		Ref:           "1.35.1",
		IngestionType: "cargo",
		FetchType:     "cargo",
	}

	files := []buildlayout.FileInfo{
		{Path: "Cargo.toml", BlobHash: "hash1", Size: 3500, Mode: 0644},
		{Path: "src/lib.rs", BlobHash: "hash2", Size: 15000, Mode: 0644},
		{Path: "src/runtime/mod.rs", BlobHash: "hash3", Size: 8000, Mode: 0644},
		{Path: "src/io/mod.rs", BlobHash: "hash4", Size: 6000, Mode: 0644},
		{Path: "src/net/tcp.rs", BlobHash: "hash5", Size: 10000, Mode: 0644},
		{Path: "tests/async_send_sync.rs", BlobHash: "hash6", Size: 2000, Mode: 0644},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths() error = %v", err)
	}

	// With hard links, we get 2x entries: canonical path + .cargo/ path
	expectedEntries := len(files) * 2
	if len(entries) != expectedEntries {
		t.Errorf("MapPaths() returned %d entries, want %d (2x for hard links)", len(entries), expectedEntries)
	}

	// Verify path format with hyphen
	hasCanonical := false
	hasCargo := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.VirtualDisplayPath, "crates.io/") {
			hasCanonical = true
		}
		if entry.VirtualDisplayPath == ".cargo/registry/src/tokio-1.35.1" {
			hasCargo = true
		}
	}
	if !hasCanonical {
		t.Error("Missing canonical path entries (crates.io/)")
	}
	if !hasCargo {
		t.Error("Missing .cargo/registry/src/ hard link entries with hyphen format")
	}
}

func TestCargoMapper_MapPaths_NoCargoToml(t *testing.T) {
	mapper := NewCargoMapper()

	info := buildlayout.RepoInfo{
		DisplayPath:   "not-cargo@1.0.0",
		IngestionType: "cargo",
	}

	// Files without Cargo.toml
	files := []buildlayout.FileInfo{
		{Path: "README.md", BlobHash: "hash1", Size: 100, Mode: 0644},
		{Path: "src/main.rs", BlobHash: "hash2", Size: 500, Mode: 0644},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths() error = %v", err)
	}

	// Should return nil when no Cargo.toml is found
	if entries != nil {
		t.Errorf("MapPaths() returned entries for non-Cargo crate, want nil")
	}
}

func TestCargoMapper_ParseDependencyFile_RealCargoToml(t *testing.T) {
	mapper := NewCargoMapper()

	// Real Cargo.toml from a typical Rust web server
	content := []byte(`[package]
name = "my-rust-server"
version = "0.1.0"
edition = "2021"
authors = ["John Doe <john@example.com>"]

[dependencies]
tokio = { version = "1.35", features = ["full"] }
axum = "0.7.2"
serde = { version = "1.0", features = ["derive"] }
serde_json = "1.0"
tracing = "0.1"
tracing-subscriber = "0.3"
sqlx = { version = "0.7", features = ["runtime-tokio-native-tls", "postgres"] }

[dev-dependencies]
mockito = "1.2.0"
tempfile = "3.8"`)

	deps, err := mapper.ParseDependencyFile(content)
	if err != nil {
		t.Fatalf("ParseDependencyFile() error = %v", err)
	}

	// Should parse all 9 dependencies (7 regular + 2 dev)
	expectedCount := 9
	if len(deps) != expectedCount {
		t.Errorf("ParseDependencyFile() returned %d dependencies, want %d", len(deps), expectedCount)
	}

	// Verify dependency parsing
	depMap := make(map[string]buildlayout.Dependency)
	for _, dep := range deps {
		depMap[dep.Module] = dep
	}

	// Check simple version
	if dep, ok := depMap["axum"]; ok {
		if dep.Version != "0.7.2" {
			t.Errorf("axum version = %v, want 0.7.2", dep.Version)
		}
		if dep.Source != "axum@0.7.2" {
			t.Errorf("axum source = %v, want axum@0.7.2", dep.Source)
		}
	} else {
		t.Error("axum dependency not found")
	}

	// Check complex version with features
	if dep, ok := depMap["tokio"]; ok {
		if dep.Version != "1.35" {
			t.Errorf("tokio version = %v, want 1.35", dep.Version)
		}
		if dep.Source != "tokio@1.35" {
			t.Errorf("tokio source = %v, want tokio@1.35", dep.Source)
		}
	} else {
		t.Error("tokio dependency not found")
	}

	// Check dev dependency
	if dep, ok := depMap["mockito"]; ok {
		if dep.Version != "1.2.0" {
			t.Errorf("mockito version = %v, want 1.2.0", dep.Version)
		}
	} else {
		t.Error("mockito dev-dependency not found")
	}

	// Check complex version with multiple features
	if dep, ok := depMap["sqlx"]; ok {
		if dep.Version != "0.7" {
			t.Errorf("sqlx version = %v, want 0.7", dep.Version)
		}
	} else {
		t.Error("sqlx dependency not found")
	}
}

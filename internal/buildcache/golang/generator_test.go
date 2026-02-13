package golang

import (
	"strings"
	"testing"

	"github.com/radryc/monofs/internal/buildcache"
)

func TestGenerator_BuildSystem(t *testing.T) {
	g := NewGenerator()
	if g.BuildSystem() != "go" {
		t.Errorf("expected 'go', got %q", g.BuildSystem())
	}
}

func TestGenerator_ShouldGenerate(t *testing.T) {
	g := NewGenerator()

	tests := []struct {
		ingestionType string
		want          bool
	}{
		{"go", true},
		{"git", false},
		{"npm", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := g.ShouldGenerate(tt.ingestionType); got != tt.want {
			t.Errorf("ShouldGenerate(%q) = %v, want %v", tt.ingestionType, got, tt.want)
		}
	}
}

func TestGenerator_GenerateEntries_Count(t *testing.T) {
	g := NewGenerator()

	entries, err := g.GenerateEntries(buildcache.ArtifactParams{
		ModulePath: "github.com/google/uuid",
		Version:    "v1.6.0",
		Source:     "github.com/google/uuid@v1.6.0",
	})
	if err != nil {
		t.Fatalf("GenerateEntries failed: %v", err)
	}

	if len(entries) != 6 {
		t.Fatalf("expected 6 entries, got %d", len(entries))
	}
}

func TestGenerator_GenerateEntries_Paths(t *testing.T) {
	g := NewGenerator()

	entries, err := g.GenerateEntries(buildcache.ArtifactParams{
		ModulePath: "github.com/google/uuid",
		Version:    "v1.6.0",
	})
	if err != nil {
		t.Fatal(err)
	}

	expectedPaths := map[buildcache.ArtifactType]string{
		buildcache.ArtifactGoModInfo:    "cache/download/github.com/google/uuid/@v/v1.6.0.info",
		buildcache.ArtifactGoModFile:    "cache/download/github.com/google/uuid/@v/v1.6.0.mod",
		buildcache.ArtifactGoModZip:     "cache/download/github.com/google/uuid/@v/v1.6.0.zip",
		buildcache.ArtifactGoModZipHash: "cache/download/github.com/google/uuid/@v/v1.6.0.ziphash",
		buildcache.ArtifactGoModList:    "cache/download/github.com/google/uuid/@v/list",
		buildcache.ArtifactGoModLock:    "cache/download/github.com/google/uuid/@v/v1.6.0.lock",
	}

	for _, e := range entries {
		expected, ok := expectedPaths[e.Type]
		if !ok {
			t.Errorf("unexpected artifact type: %s", e.Type)
			continue
		}
		if e.RelativePath != expected {
			t.Errorf("type %s: got path %q, want %q", e.Type, e.RelativePath, expected)
		}
		// Verify metadata
		if e.Metadata["artifact_type"] != string(e.Type) {
			t.Errorf("type %s: metadata artifact_type = %q, want %q",
				e.Type, e.Metadata["artifact_type"], string(e.Type))
		}
		if e.Metadata["module"] != "github.com/google/uuid" {
			t.Errorf("type %s: metadata module = %q", e.Type, e.Metadata["module"])
		}
		if e.Metadata["version"] != "v1.6.0" {
			t.Errorf("type %s: metadata version = %q", e.Type, e.Metadata["version"])
		}
		if e.Metadata["generatable"] != "true" {
			t.Errorf("type %s: metadata generatable = %q", e.Type, e.Metadata["generatable"])
		}
	}
}

func TestGenerator_GenerateEntries_Sizes(t *testing.T) {
	g := NewGenerator()

	goModSize := uint64(256)
	entries, err := g.GenerateEntries(buildcache.ArtifactParams{
		ModulePath:    "github.com/google/uuid",
		Version:       "v1.6.0",
		GoModFileSize: goModSize,
	})
	if err != nil {
		t.Fatalf("GenerateEntries failed: %v", err)
	}

	expectedSizes := map[buildcache.ArtifactType]uint64{
		buildcache.ArtifactGoModList: uint64(len("v1.6.0") + 1), // "v1.6.0\n"
		buildcache.ArtifactGoModFile: goModSize,                 // from ingested go.mod
		buildcache.ArtifactGoModLock: 0,                         // empty sentinel
		// These depend on on-demand fetcher generation — remain 0
		buildcache.ArtifactGoModInfo:    0,
		buildcache.ArtifactGoModZip:     0,
		buildcache.ArtifactGoModZipHash: 0,
	}

	for _, e := range entries {
		want, ok := expectedSizes[e.Type]
		if !ok {
			t.Errorf("unexpected type %s", e.Type)
			continue
		}
		if e.Size != want {
			t.Errorf("type %s: Size = %d, want %d", e.Type, e.Size, want)
		}
	}
}

func TestGenerator_GenerateEntries_UppercaseModule(t *testing.T) {
	g := NewGenerator()

	entries, err := g.GenerateEntries(buildcache.ArtifactParams{
		ModulePath: "github.com/ProtonMail/go-crypto",
		Version:    "v1.3.0",
	})
	if err != nil {
		t.Fatal(err)
	}

	// ProtonMail → !proton!mail in paths
	for _, e := range entries {
		if !strings.Contains(e.RelativePath, "!proton!mail") {
			t.Errorf("path should contain escaped uppercase: %q", e.RelativePath)
		}
		if strings.Contains(e.RelativePath, "ProtonMail") {
			t.Errorf("path should NOT contain unescaped uppercase: %q", e.RelativePath)
		}
		// Metadata keeps original (unescaped) module path
		if e.Metadata["module"] != "github.com/ProtonMail/go-crypto" {
			t.Errorf("metadata module should be unescaped: %q", e.Metadata["module"])
		}
	}
}

func TestGenerator_GenerateEntries_MissingParams(t *testing.T) {
	g := NewGenerator()

	_, err := g.GenerateEntries(buildcache.ArtifactParams{})
	if err == nil {
		t.Fatal("expected error for empty params")
	}

	_, err = g.GenerateEntries(buildcache.ArtifactParams{ModulePath: "foo"})
	if err == nil {
		t.Fatal("expected error for missing version")
	}

	_, err = g.GenerateEntries(buildcache.ArtifactParams{Version: "v1.0.0"})
	if err == nil {
		t.Fatal("expected error for missing module path")
	}
}

func TestDeterministicHash_Stable(t *testing.T) {
	h1 := DeterministicHash("github.com/google/uuid", "v1.6.0", buildcache.ArtifactGoModInfo)
	h2 := DeterministicHash("github.com/google/uuid", "v1.6.0", buildcache.ArtifactGoModInfo)

	if h1 != h2 {
		t.Errorf("hash not stable: %q != %q", h1, h2)
	}

	if len(h1) != 64 { // SHA-256 hex = 64 chars
		t.Errorf("expected 64-char hex hash, got %d chars", len(h1))
	}
}

func TestDeterministicHash_Unique(t *testing.T) {
	hashes := make(map[string]bool)

	types := []buildcache.ArtifactType{
		buildcache.ArtifactGoModInfo,
		buildcache.ArtifactGoModFile,
		buildcache.ArtifactGoModZip,
		buildcache.ArtifactGoModZipHash,
		buildcache.ArtifactGoModList,
		buildcache.ArtifactGoModLock,
	}

	for _, at := range types {
		h := DeterministicHash("github.com/google/uuid", "v1.6.0", at)
		if hashes[h] {
			t.Errorf("duplicate hash for artifact type %s", at)
		}
		hashes[h] = true
	}

	// Different module → different hash
	h1 := DeterministicHash("github.com/google/uuid", "v1.6.0", buildcache.ArtifactGoModInfo)
	h2 := DeterministicHash("github.com/other/module", "v1.6.0", buildcache.ArtifactGoModInfo)
	if h1 == h2 {
		t.Error("different modules should produce different hashes")
	}

	// Different version → different hash
	h3 := DeterministicHash("github.com/google/uuid", "v1.5.0", buildcache.ArtifactGoModInfo)
	if h1 == h3 {
		t.Error("different versions should produce different hashes")
	}
}

func TestEscapeModulePath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"github.com/google/uuid", "github.com/google/uuid"},
		{"github.com/Azure/go-autorest", "github.com/!azure/go-autorest"},
		{"github.com/ProtonMail/go-crypto", "github.com/!proton!mail/go-crypto"},
		{"github.com/RoaringBitmap/roaring", "github.com/!roaring!bitmap/roaring"},
		{"", ""},
	}

	for _, tt := range tests {
		got := EscapeModulePath(tt.input)
		if got != tt.want {
			t.Errorf("EscapeModulePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

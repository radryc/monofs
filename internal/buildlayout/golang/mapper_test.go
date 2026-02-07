package golang

import (
	"testing"

	"github.com/radryc/monofs/internal/buildlayout"
)

func TestGoMapper_Type(t *testing.T) {
	m := NewGoMapper()
	if m.Type() != "go" {
		t.Errorf("expected type 'go', got %q", m.Type())
	}
}

func TestGoMapper_Matches(t *testing.T) {
	m := NewGoMapper()

	tests := []struct {
		name          string
		ingestionType string
		want          bool
	}{
		{"go ingestion", "go", true},
		{"git ingestion", "git", false},
		{"empty", "", false},
		{"s3", "s3", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.Matches(buildlayout.RepoInfo{IngestionType: tt.ingestionType})
			if got != tt.want {
				t.Errorf("Matches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGoMapper_MapPaths_Standard(t *testing.T) {
	m := NewGoMapper()

	info := buildlayout.RepoInfo{
		DisplayPath:   "github.com/google/uuid@v1.6.0",
		StorageID:     "abc123",
		Source:        "github.com/google/uuid@v1.6.0",
		Ref:           "v1.6.0",
		IngestionType: "go",
		FetchType:     "gomod",
	}

	files := []buildlayout.FileInfo{
		{Path: "uuid.go", BlobHash: "hash1", Size: 100, Mode: 0644},
		{Path: "go.mod", BlobHash: "hash2", Size: 50, Mode: 0644},
		{Path: "internal/doc.go", BlobHash: "hash3", Size: 200, Mode: 0644},
	}

	entries, err := m.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths failed: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	expectedDisplayPath := "go-modules/pkg/mod/github.com/google/uuid@v1.6.0"
	for i, e := range entries {
		if e.VirtualDisplayPath != expectedDisplayPath {
			t.Errorf("entry %d: VirtualDisplayPath = %q, want %q", i, e.VirtualDisplayPath, expectedDisplayPath)
		}
		if e.VirtualFilePath != files[i].Path {
			t.Errorf("entry %d: VirtualFilePath = %q, want %q", i, e.VirtualFilePath, files[i].Path)
		}
		if e.OriginalFilePath != files[i].Path {
			t.Errorf("entry %d: OriginalFilePath = %q, want %q", i, e.OriginalFilePath, files[i].Path)
		}
	}
}

func TestGoMapper_MapPaths_UppercaseModule(t *testing.T) {
	m := NewGoMapper()

	info := buildlayout.RepoInfo{
		DisplayPath:   "github.com/Azure/azure-sdk-for-go@v1.0.0",
		IngestionType: "go",
	}

	files := []buildlayout.FileInfo{
		{Path: "sdk.go", BlobHash: "h1"},
	}

	entries, err := m.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths failed: %v", err)
	}

	// Azure → !azure in Go module cache
	expected := "go-modules/pkg/mod/github.com/!azure/azure-sdk-for-go@v1.0.0"
	if entries[0].VirtualDisplayPath != expected {
		t.Errorf("expected %q, got %q", expected, entries[0].VirtualDisplayPath)
	}
}

func TestGoMapper_MapPaths_VersionInRef(t *testing.T) {
	m := NewGoMapper()

	// DisplayPath has no @version, version is in Ref
	info := buildlayout.RepoInfo{
		DisplayPath:   "github.com/google/uuid",
		Ref:           "v1.6.0",
		IngestionType: "go",
	}

	files := []buildlayout.FileInfo{{Path: "uuid.go", BlobHash: "h1"}}

	entries, err := m.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths failed: %v", err)
	}

	expected := "go-modules/pkg/mod/github.com/google/uuid@v1.6.0"
	if entries[0].VirtualDisplayPath != expected {
		t.Errorf("expected %q, got %q", expected, entries[0].VirtualDisplayPath)
	}
}

func TestGoMapper_MapPaths_EmptyFiles(t *testing.T) {
	m := NewGoMapper()
	entries, err := m.MapPaths(buildlayout.RepoInfo{IngestionType: "go"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty files, got %d", len(entries))
	}
}

func TestGoMapper_MapPaths_NoVersion(t *testing.T) {
	m := NewGoMapper()
	_, err := m.MapPaths(buildlayout.RepoInfo{
		DisplayPath:   "github.com/google/uuid",
		Ref:           "", // no version
		IngestionType: "go",
	}, []buildlayout.FileInfo{{Path: "f.go"}})

	if err == nil {
		t.Error("expected error when no version is available")
	}
}

func TestGoMapper_ParseDependencyFile(t *testing.T) {
	gomod := []byte(`module myproject

go 1.21

require (
	github.com/google/uuid v1.6.0
	github.com/stretchr/testify v1.9.0 // indirect
	golang.org/x/sync v0.6.0
)

require github.com/pkg/errors v0.9.1

replace github.com/foo/bar => ../local

exclude github.com/old/thing v0.1.0
`)

	m := NewGoMapper()
	deps, err := m.ParseDependencyFile(gomod)
	if err != nil {
		t.Fatalf("ParseDependencyFile failed: %v", err)
	}

	if len(deps) != 4 {
		t.Fatalf("expected 4 deps, got %d: %+v", len(deps), deps)
	}

	expected := []buildlayout.Dependency{
		{Module: "github.com/google/uuid", Version: "v1.6.0", Source: "github.com/google/uuid@v1.6.0"},
		{Module: "github.com/stretchr/testify", Version: "v1.9.0", Source: "github.com/stretchr/testify@v1.9.0"},
		{Module: "golang.org/x/sync", Version: "v0.6.0", Source: "golang.org/x/sync@v0.6.0"},
		{Module: "github.com/pkg/errors", Version: "v0.9.1", Source: "github.com/pkg/errors@v0.9.1"},
	}

	for i, dep := range deps {
		if dep.Module != expected[i].Module {
			t.Errorf("dep %d: Module = %q, want %q", i, dep.Module, expected[i].Module)
		}
		if dep.Version != expected[i].Version {
			t.Errorf("dep %d: Version = %q, want %q", i, dep.Version, expected[i].Version)
		}
		if dep.Source != expected[i].Source {
			t.Errorf("dep %d: Source = %q, want %q", i, dep.Source, expected[i].Source)
		}
	}
}

func TestGoMapper_ParseDependencyFile_Empty(t *testing.T) {
	m := NewGoMapper()
	deps, err := m.ParseDependencyFile([]byte(`module myproject
go 1.21
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 deps, got %d", len(deps))
	}
}

func TestEncodePath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"github.com/google/uuid", "github.com/google/uuid"},
		{"github.com/Azure/go-autorest", "github.com/!azure/go-autorest"},
		{"github.com/BurntSushi/toml", "github.com/!burnt!sushi/toml"},
		{"", ""},
	}

	for _, tt := range tests {
		got := EncodePath(tt.input)
		if got != tt.want {
			t.Errorf("EncodePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDecodePath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"github.com/google/uuid", "github.com/google/uuid"},
		{"github.com/!azure/go-autorest", "github.com/Azure/go-autorest"},
		{"github.com/!burnt!sushi/toml", "github.com/BurntSushi/toml"},
		{"", ""},
	}

	for _, tt := range tests {
		got := DecodePath(tt.input)
		if got != tt.want {
			t.Errorf("DecodePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestEncodeDecodeRoundtrip(t *testing.T) {
	paths := []string{
		"github.com/Azure/go-autorest",
		"github.com/BurntSushi/toml",
		"github.com/google/uuid",
		"golang.org/x/crypto",
	}

	for _, p := range paths {
		decoded := DecodePath(EncodePath(p))
		if decoded != p {
			t.Errorf("roundtrip failed: %q → encode → decode → %q", p, decoded)
		}
	}
}

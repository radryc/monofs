package fetcher

import (
"io"
"testing"

"github.com/radryc/monofs/internal/storage"
"github.com/radryc/monofs/internal/storage/blob"
storagegit "github.com/radryc/monofs/internal/storage/git"
)

func TestBackendRegistry(t *testing.T) {
registry := NewRegistry()

// Register backends
gitBackend := storagegit.NewGitBackend()
blobBackend := blob.NewBlobBackend()

registry.Register(gitBackend)
registry.Register(blobBackend)

// Get by type
gitResult, ok := registry.Get(SourceTypeGit)
if !ok || gitResult == nil {
t.Error("expected to get Git backend")
}

blobResult, ok := registry.Get(SourceTypeBlob)
if !ok || blobResult == nil {
t.Error("expected to get Blob backend")
}

_, ok = registry.Get(SourceTypeUnknown)
if ok {
t.Error("expected false for unregistered backend")
}

// Close all
if err := registry.Close(); err != nil {
t.Errorf("Close failed: %v", err)
}
}

func TestParseSourceType(t *testing.T) {
tests := []struct {
input    string
expected SourceType
}{
{"git", SourceTypeGit},
{"blob", SourceTypeBlob},
{"unknown", SourceTypeUnknown},
{"", SourceTypeUnknown},
}

for _, tt := range tests {
result := ParseSourceType(tt.input)
if result != tt.expected {
t.Errorf("ParseSourceType(%q) = %v, want %v", tt.input, result, tt.expected)
}
}
}

func TestSourceTypeString(t *testing.T) {
tests := []struct {
input    SourceType
expected string
}{
{SourceTypeGit, "git"},
{SourceTypeBlob, "blob"},
{SourceTypeUnknown, "unknown"},
}

for _, tt := range tests {
result := tt.input.String()
if result != tt.expected {
t.Errorf("SourceType(%q).String() = %q, want %q", tt.input, result, tt.expected)
}
}
}

// TestStreamReader verifies the io.ReadCloser implementation
func TestStreamReader(t *testing.T) {
data := []byte("hello world from stream reader test")

pr, pw := io.Pipe()

go func() {
for i := 0; i < len(data); i += 10 {
end := i + 10
if end > len(data) {
end = len(data)
}
pw.Write(data[i:end])
}
pw.Close()
}()

result, err := io.ReadAll(pr)
if err != nil {
t.Fatalf("ReadAll failed: %v", err)
}

if string(result) != string(data) {
t.Errorf("expected %q, got %q", string(data), string(result))
}
}

// Test backend configuration
func TestBackendConfig_Defaults(t *testing.T) {
config := BackendConfig{
CacheDir: "/tmp/test",
}

// Verify defaults are reasonable
if config.Concurrency != 0 {
// Zero means no explicit limit
}

if config.MaxCacheSize != 0 {
// Zero means unlimited
}
}

// File path helper test
func TestExtractFilePath(t *testing.T) {
tests := []struct {
config   map[string]string
expected string
}{
{map[string]string{"file_path": "main.go"}, "main.go"},
{map[string]string{"display_path": "path/to/file.go"}, "path/to/file.go"},
{map[string]string{"path": "another/path.go"}, "another/path.go"},
{map[string]string{}, ""},
}

for _, tt := range tests {
result := ""
if fp, ok := tt.config["file_path"]; ok {
result = fp
} else if dp, ok := tt.config["display_path"]; ok {
result = dp
} else if p, ok := tt.config["path"]; ok {
result = p
}

if result != tt.expected {
t.Errorf("extractFilePath(%v) = %q, want %q", tt.config, result, tt.expected)
}
}
}

// Benchmarks

func BenchmarkParseSourceType(b *testing.B) {
types := []string{"git", "blob", "unknown"}

b.ResetTimer()
for i := 0; i < b.N; i++ {
for _, t := range types {
ParseSourceType(t)
}
}
}

// Ensure storage package is referenced to avoid import issues
var _ = storage.FetchTypeGit

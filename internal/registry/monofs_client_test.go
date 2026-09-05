package registry

import (
	"testing"

	pb "github.com/radryc/monofs/api/proto"
)

func TestResolveWritePaths_NamespaceScoped(t *testing.T) {
	c := &Client{dataNS: "docker-registry"}

	tests := []struct {
		name        string
		path        string
		wantDP      string
		wantFileRel string
		wantBaseDP  string // namespace alone when path is _-prefixed
	}{
		{
			name:        "blob path",
			path:        "_blobs/sha256/abc123",
			wantDP:      "docker-registry",
			wantFileRel: "_blobs/sha256/abc123",
		},
		{
			name:        "catalog path",
			path:        "_catalog",
			wantDP:      "docker-registry",
			wantFileRel: "_catalog",
		},
		{
			name:        "simple key at namespace root",
			path:        "_index",
			wantDP:      "docker-registry",
			wantFileRel: "_index",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dp, _, fp := c.resolveWritePaths(tt.path)
			if dp != tt.wantDP {
				t.Errorf("displayPath = %q, want %q", dp, tt.wantDP)
			}
			if fp != tt.wantFileRel {
				t.Errorf("filePath = %q, want %q", fp, tt.wantFileRel)
			}
		})
	}
}

func TestResolveWritePaths_RepoSscoped(t *testing.T) {
	c := &Client{dataNS: "docker-registry"}

	tests := []struct {
		name        string
		path        string
		wantDP      string
		wantFileRel string
	}{
		{
			name:        "tag under repo",
			path:        "doctor-query/_tags/latest",
			wantDP:      "docker-registry/doctor-query",
			wantFileRel: "_tags/latest",
		},
		{
			name:        "tag under repo with multi-level ref",
			path:        "guardian-pusher-docker/_tags/v1.0.0",
			wantDP:      "docker-registry/guardian-pusher-docker",
			wantFileRel: "_tags/v1.0.0",
		},
		{
			name:        "deep nested tag path",
			path:        "monofs-fetcher/_tags/dev",
			wantDP:      "docker-registry/monofs-fetcher",
			wantFileRel: "_tags/dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dp, _, fp := c.resolveWritePaths(tt.path)
			if dp != tt.wantDP {
				t.Errorf("displayPath = %q, want %q", dp, tt.wantDP)
			}
			if fp != tt.wantFileRel {
				t.Errorf("filePath = %q, want %q", fp, tt.wantFileRel)
			}
		})
	}
}

func TestResolveWritePaths_StorageIDIsDeterministic(t *testing.T) {
	c := &Client{dataNS: "docker-registry"}

	_, id1, _ := c.resolveWritePaths("doctor-query/_tags/latest")
	_, id2, _ := c.resolveWritePaths("doctor-query/_tags/latest")
	if id1 != id2 {
		t.Errorf("storageID should be deterministic: %q != %q", id1, id2)
	}

	if id1 == "" {
		t.Error("storageID should not be empty")
	}

	// Different repos get different storage IDs
	_, id3, _ := c.resolveWritePaths("other-repo/_tags/latest")
	if id1 == id3 {
		t.Errorf("different repos should have different storageIDs, both got %q", id1)
	}
}

func TestResolveWritePaths_SameRepoAllTagsSameStorageID(t *testing.T) {
	c := &Client{dataNS: "docker-registry"}

	_, id1, _ := c.resolveWritePaths("doctor-query/_tags/latest")
	_, id2, _ := c.resolveWritePaths("doctor-query/_tags/v1.0")
	_, id3, _ := c.resolveWritePaths("doctor-query/_tags/edge")

	if id1 != id2 || id2 != id3 {
		t.Errorf("all tags in same repo should share the same storageID: %q, %q, %q", id1, id2, id3)
	}
}

func TestWriteMetadata_Fields(t *testing.T) {
	data := []byte("sha256:abcd1234")
	meta := writeMetadata("_tags/latest", "abc-storage", "docker-registry/doctor-query", data)

	if meta.Path != "_tags/latest" {
		t.Errorf("Path = %q, want _tags/latest", meta.Path)
	}
	if meta.StorageId != "abc-storage" {
		t.Errorf("StorageId = %q, want abc-storage", meta.StorageId)
	}
	if meta.DisplayPath != "docker-registry/doctor-query" {
		t.Errorf("DisplayPath = %q, want docker-registry/doctor-query", meta.DisplayPath)
	}
	if meta.Size != uint64(len(data)) {
		t.Errorf("Size = %d, want %d", meta.Size, len(data))
	}
	if meta.Mode != 0644 {
		t.Errorf("Mode = %o, want 0644", meta.Mode)
	}
	if meta.BlobHash == "" {
		t.Error("BlobHash should not be empty")
	}
	if meta.Source != "monofs-registry" {
		t.Errorf("Source = %q, want monofs-registry", meta.Source)
	}
	if !bytesEqual(meta.InlineContent, data) {
		t.Error("InlineContent mismatch")
	}
	if meta.SourceType != pb.IngestionType_INGESTION_FILE {
		t.Errorf("SourceType = %v, want INGESTION_FILE", meta.SourceType)
	}
	if meta.FetchType != pb.SourceType_SOURCE_TYPE_BLOB {
		t.Errorf("FetchType = %v, want SOURCE_TYPE_BLOB", meta.FetchType)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

package buildlayout

import (
	"fmt"
	"testing"
)

// mockMapper implements LayoutMapper for testing.
type mockMapper struct {
	typeStr    string
	matchAll   bool
	entries    []VirtualEntry
	shouldFail bool
}

func (m *mockMapper) Type() string               { return m.typeStr }
func (m *mockMapper) Matches(info RepoInfo) bool { return m.matchAll }
func (m *mockMapper) MapPaths(info RepoInfo, files []FileInfo) ([]VirtualEntry, error) {
	if m.shouldFail {
		return nil, fmt.Errorf("mock error")
	}
	return m.entries, nil
}
func (m *mockMapper) ParseDependencyFile(content []byte) ([]Dependency, error) {
	return nil, nil
}

func TestRegistryEmpty(t *testing.T) {
	reg := NewRegistry()
	if reg.HasMappers() {
		t.Error("empty registry should have no mappers")
	}
	entries, err := reg.MapAll(RepoInfo{}, nil)
	if err != nil {
		t.Errorf("MapAll on empty registry should not error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestRegistryMatchingMapper(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockMapper{
		typeStr:  "test",
		matchAll: true,
		entries: []VirtualEntry{
			{VirtualDisplayPath: "virtual/repo", VirtualFilePath: "file.go", OriginalFilePath: "file.go"},
		},
	})

	entries, err := reg.MapAll(RepoInfo{DisplayPath: "test"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].VirtualDisplayPath != "virtual/repo" {
		t.Errorf("wrong VirtualDisplayPath: %s", entries[0].VirtualDisplayPath)
	}
}

func TestRegistryNonMatchingMapper(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockMapper{
		typeStr:  "test",
		matchAll: false, // doesn't match
		entries:  []VirtualEntry{{VirtualDisplayPath: "should-not-appear"}},
	})

	entries, err := reg.MapAll(RepoInfo{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries from non-matching mapper, got %d", len(entries))
	}
}

func TestRegistryMapperError(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockMapper{
		typeStr:    "failing",
		matchAll:   true,
		shouldFail: true,
	})

	_, err := reg.MapAll(RepoInfo{}, nil)
	if err == nil {
		t.Error("expected error from failing mapper")
	}
}

func TestRegistryMultipleMappers(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockMapper{
		typeStr: "a", matchAll: true,
		entries: []VirtualEntry{{VirtualDisplayPath: "a/repo", VirtualFilePath: "f1", OriginalFilePath: "f1"}},
	})
	reg.Register(&mockMapper{
		typeStr: "b", matchAll: true,
		entries: []VirtualEntry{{VirtualDisplayPath: "b/repo", VirtualFilePath: "f2", OriginalFilePath: "f2"}},
	})

	entries, err := reg.MapAll(RepoInfo{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

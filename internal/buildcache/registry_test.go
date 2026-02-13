package buildcache

import "testing"

// stubGenerator is a minimal ArtifactGenerator for testing the registry.
type stubGenerator struct {
	buildSystem   string
	ingestionType string
}

func (s *stubGenerator) BuildSystem() string { return s.buildSystem }
func (s *stubGenerator) ShouldGenerate(ingestionType string) bool {
	return ingestionType == s.ingestionType
}
func (s *stubGenerator) GenerateEntries(params ArtifactParams) ([]ArtifactEntry, error) {
	return nil, nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	reg := NewRegistry()
	gen := &stubGenerator{buildSystem: "go", ingestionType: "go"}

	reg.Register(gen)

	got, err := reg.Get("go")
	if err != nil {
		t.Fatalf("Get(go) returned error: %v", err)
	}
	if got.BuildSystem() != "go" {
		t.Errorf("expected BuildSystem=go, got %q", got.BuildSystem())
	}
}

func TestRegistry_GetUnknown(t *testing.T) {
	reg := NewRegistry()

	_, err := reg.Get("unknown")
	if err == nil {
		t.Fatal("expected error for unknown build system")
	}
}

func TestRegistry_FindGenerator(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&stubGenerator{buildSystem: "go", ingestionType: "go"})
	reg.Register(&stubGenerator{buildSystem: "npm", ingestionType: "npm"})

	tests := []struct {
		ingestionType string
		expectSystem  string
		expectNil     bool
	}{
		{"go", "go", false},
		{"npm", "npm", false},
		{"cargo", "", true},
		{"unknown", "", true},
	}

	for _, tc := range tests {
		gen := reg.FindGenerator(tc.ingestionType)
		if tc.expectNil {
			if gen != nil {
				t.Errorf("FindGenerator(%q) expected nil, got %q", tc.ingestionType, gen.BuildSystem())
			}
		} else {
			if gen == nil {
				t.Fatalf("FindGenerator(%q) returned nil", tc.ingestionType)
			}
			if gen.BuildSystem() != tc.expectSystem {
				t.Errorf("FindGenerator(%q) got system %q, want %q", tc.ingestionType, gen.BuildSystem(), tc.expectSystem)
			}
		}
	}
}

func TestRegistry_Has(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&stubGenerator{buildSystem: "go", ingestionType: "go"})

	if !reg.Has("go") {
		t.Error("Has(go) should be true")
	}
	if reg.Has("npm") {
		t.Error("Has(npm) should be false")
	}
}

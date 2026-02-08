package buildlayout_test

import (
	"os"
	"testing"

	cargomapper "github.com/radryc/monofs/internal/buildlayout/cargo"
	mavenmapper "github.com/radryc/monofs/internal/buildlayout/maven"
	npmmapper "github.com/radryc/monofs/internal/buildlayout/npm"
)

func TestNpmParser(t *testing.T) {
	content, err := os.ReadFile("../../test/fixtures/package.json")
	if err != nil {
		t.Fatalf("Failed to read package.json: %v", err)
	}

	mapper := npmmapper.NewNpmMapper()
	deps, err := mapper.ParseDependencyFile(content)
	if err != nil {
		t.Fatalf("Failed to parse package.json: %v", err)
	}

	// Expect 5 dependencies (3 regular + 2 dev)
	expectedCount := 5
	if len(deps) != expectedCount {
		t.Errorf("Expected %d dependencies, got %d", expectedCount, len(deps))
	}

	t.Logf("npm: Found %d dependencies", len(deps))
	for _, dep := range deps {
		t.Logf("  - %s", dep.Source)
	}

	// Verify some expected dependencies
	found := make(map[string]bool)
	for _, dep := range deps {
		found[dep.Module] = true
	}

	expectedDeps := []string{"express", "lodash", "axios", "jest", "eslint"}
	for _, exp := range expectedDeps {
		if !found[exp] {
			t.Errorf("Expected to find dependency %s", exp)
		}
	}
}

func TestMavenParser(t *testing.T) {
	content, err := os.ReadFile("../../test/fixtures/pom.xml")
	if err != nil {
		t.Fatalf("Failed to read pom.xml: %v", err)
	}

	mapper := mavenmapper.NewMavenMapper()
	deps, err := mapper.ParseDependencyFile(content)
	if err != nil {
		t.Fatalf("Failed to parse pom.xml: %v", err)
	}

	// Expect 3 dependencies
	expectedCount := 3
	if len(deps) != expectedCount {
		t.Errorf("Expected %d dependencies, got %d", expectedCount, len(deps))
	}

	t.Logf("maven: Found %d dependencies", len(deps))
	for _, dep := range deps {
		t.Logf("  - %s", dep.Source)
	}

	// Verify expected dependencies
	found := make(map[string]bool)
	for _, dep := range deps {
		found[dep.Module] = true
	}

	expectedDeps := []string{"junit:junit", "com.google.guava:guava", "org.springframework.boot:spring-boot-starter-web"}
	for _, exp := range expectedDeps {
		if !found[exp] {
			t.Errorf("Expected to find dependency %s", exp)
		}
	}
}

func TestCargoParser(t *testing.T) {
	content, err := os.ReadFile("../../test/fixtures/Cargo.toml")
	if err != nil {
		t.Fatalf("Failed to read Cargo.toml: %v", err)
	}

	mapper := cargomapper.NewCargoMapper()
	deps, err := mapper.ParseDependencyFile(content)
	if err != nil {
		t.Fatalf("Failed to parse Cargo.toml: %v", err)
	}

	// Expect 4 dependencies (3 regular + 1 dev)
	expectedCount := 4
	if len(deps) != expectedCount {
		t.Errorf("Expected %d dependencies, got %d", expectedCount, len(deps))
	}

	t.Logf("cargo: Found %d dependencies", len(deps))
	for _, dep := range deps {
		t.Logf("  - %s", dep.Source)
	}

	// Verify expected dependencies
	found := make(map[string]bool)
	for _, dep := range deps {
		found[dep.Module] = true
	}

	expectedDeps := []string{"serde", "tokio", "reqwest", "mockito"}
	for _, exp := range expectedDeps {
		if !found[exp] {
			t.Errorf("Expected to find dependency %s", exp)
		}
	}
}

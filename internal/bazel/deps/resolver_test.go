package deps

import (
	"context"
	"testing"
)

func TestResolveGoImportSelf(t *testing.T) {
	r := NewImportResolver(manifestWithGoRepos(), NewLockfile())

	dep, err := r.ResolveGoImport(context.Background(),
		"github.com/org/platform-api/pkg/auth", "sre/platform-api")
	if err != nil {
		t.Fatalf("ResolveGoImport: %v", err)
	}
	if dep.Type != SelfDep {
		t.Errorf("type: got %q, want self", dep.Type)
	}
	if dep.Target != "//pkg/auth:auth" {
		t.Errorf("target: got %q, want //pkg/auth:auth", dep.Target)
	}
}

func TestResolveGoImportCrossRepo(t *testing.T) {
	r := NewImportResolver(manifestWithGoRepos(), NewLockfile())

	dep, err := r.ResolveGoImport(context.Background(),
		"github.com/org/infra-tooling/pkg/middleware", "sre/platform-api")
	if err != nil {
		t.Fatalf("ResolveGoImport: %v", err)
	}
	if dep.Type != CrossRepoDep {
		t.Errorf("type: got %q, want cross-repo", dep.Type)
	}
	if dep.Target != "@infra_tooling//pkg/middleware:middleware" {
		t.Errorf("target: got %q, want @infra_tooling//pkg/middleware:middleware", dep.Target)
	}
	if dep.DisplayPath != "sre/infra-tooling" {
		t.Errorf("display_path: got %q, want sre/infra-tooling", dep.DisplayPath)
	}
}

func TestResolveGoImportExternal(t *testing.T) {
	r := NewImportResolver(manifestWithGoRepos(), NewLockfile())

	dep, err := r.ResolveGoImport(context.Background(),
		"github.com/gorilla/mux", "sre/platform-api")
	if err != nil {
		t.Fatalf("ResolveGoImport: %v", err)
	}
	if dep.Type != ExternalDep {
		t.Errorf("type: got %q, want external", dep.Type)
	}
	if dep.Target == "" {
		t.Error("external target should not be empty")
	}
	// Gazelle target should be something like @com_github_gorilla_mux//:mux
	if dep.Target[0] != '@' {
		t.Errorf("external target should start with @, got %q", dep.Target)
	}
}

func TestResolveGoImportRootPackage(t *testing.T) {
	r := NewImportResolver(manifestWithGoRepos(), NewLockfile())

	// Import the module root itself.
	dep, err := r.ResolveGoImport(context.Background(),
		"github.com/org/infra-tooling", "sre/platform-api")
	if err != nil {
		t.Fatalf("ResolveGoImport: %v", err)
	}
	if dep.Type != CrossRepoDep {
		t.Errorf("type: got %q, want cross-repo", dep.Type)
	}
	// Root package should be "@infra_tooling//:infra_tooling"
	if dep.Target != "@infra_tooling//:infra_tooling" {
		t.Errorf("target: got %q, want @infra_tooling//:infra_tooling", dep.Target)
	}
}

func TestResolveAllGoImports(t *testing.T) {
	r := NewImportResolver(manifestWithGoRepos(), NewLockfile())

	imports := []string{
		"github.com/org/platform-api/pkg/auth",
		"github.com/org/infra-tooling/pkg/middleware",
		"github.com/gorilla/mux",
	}
	deps, err := r.ResolveAllGoImports(context.Background(), imports, "sre/platform-api")
	if err != nil {
		t.Fatalf("ResolveAllGoImports: %v", err)
	}
	if len(deps) != 3 {
		t.Fatalf("expected 3 resolved, got %d", len(deps))
	}
	if deps["github.com/org/platform-api/pkg/auth"].Type != SelfDep {
		t.Error("first import should be self-dep")
	}
	if deps["github.com/org/infra-tooling/pkg/middleware"].Type != CrossRepoDep {
		t.Error("second import should be cross-repo")
	}
	if deps["github.com/gorilla/mux"].Type != ExternalDep {
		t.Error("third import should be external")
	}
}

func TestGoImportToBazelTarget(t *testing.T) {
	tests := []struct {
		repo, suffix, expected string
	}{
		{"", "pkg/auth", "//pkg/auth:auth"},
		{"", "cmd/server/main", "//cmd/server/main:main"},
		{"infra_tooling", "pkg/auth", "@infra_tooling//pkg/auth:auth"},
		{"infra_tooling", "infra_tooling", "@infra_tooling//:infra_tooling"},
	}
	for _, tt := range tests {
		got := goImportToBazelTarget(tt.repo, tt.suffix)
		if got != tt.expected {
			t.Errorf("goImportToBazelTarget(%q, %q) = %q, want %q",
				tt.repo, tt.suffix, got, tt.expected)
		}
	}
}

func TestDetectCycles(t *testing.T) {
	lf := NewLockfile()
	lf.AddRepo("a", "", "x", "")
	lf.AddRepo("b", "", "x", "")
	lf.AddRepo("c", "", "x", "")
	lf.AddDep("a", "b", "x")
	lf.AddDep("b", "c", "x")
	lf.AddDep("c", "a", "x") // cycle: a→b→c→a

	r := NewImportResolver(manifestWithGoRepos(), lf)
	cycles := r.DetectCycles()
	if len(cycles) == 0 {
		t.Error("expected a cycle, found none")
	}
}

func TestDetectCyclesNoCycle(t *testing.T) {
	lf := NewLockfile()
	lf.AddRepo("a", "", "x", "")
	lf.AddRepo("b", "", "x", "")
	lf.AddRepo("c", "", "x", "")
	lf.AddDep("a", "b", "x")
	lf.AddDep("b", "c", "x")
	// No cycle: a→b→c

	r := NewImportResolver(manifestWithGoRepos(), lf)
	cycles := r.DetectCycles()
	if len(cycles) != 0 {
		t.Errorf("expected no cycles, got %d", len(cycles))
	}
}

func TestDetectCyclesNilLockfile(t *testing.T) {
	r := NewImportResolver(manifestWithGoRepos(), nil)
	cycles := r.DetectCycles()
	if cycles != nil {
		t.Errorf("nil lockfile: expected nil cycles, got %v", cycles)
	}
}

func TestResolveGoImportUnknownRepo(t *testing.T) {
	// Resolving from a repo not in the manifest.
	r := NewImportResolver(manifestWithGoRepos(), NewLockfile())
	dep, err := r.ResolveGoImport(context.Background(),
		"github.com/unknown/repo/pkg/x", "sre/unknown-repo")
	if err != nil {
		t.Fatalf("ResolveGoImport: %v", err)
	}
	if dep.Type != ExternalDep {
		t.Errorf("unknown import from unknown repo should be external, got %q", dep.Type)
	}
}

func TestBuildRepoModuleMap(t *testing.T) {
	r := NewImportResolver(manifestWithGoRepos(), NewLockfile())
	m := r.buildRepoModuleMap()

	// platform-api: github.com/org/platform-api
	if m["sre/platform-api"] != "github.com/org/platform-api" {
		t.Errorf("platform-api module: got %q", m["sre/platform-api"])
	}
	// infra-tooling: github.com/org/infra-tooling
	if m["sre/infra-tooling"] != "github.com/org/infra-tooling" {
		t.Errorf("infra-tooling module: got %q", m["sre/infra-tooling"])
	}
}

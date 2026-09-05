package deps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNewLockfile(t *testing.T) {
	lf := NewLockfile()
	if lf.Version != LockfileVersion {
		t.Errorf("version: got %d, want %d", lf.Version, LockfileVersion)
	}
	if lf.GeneratedAt == "" {
		t.Error("GeneratedAt should not be empty")
	}
	if lf.Repositories == nil {
		t.Error("Repositories should be initialized")
	}
}

func TestAddRepo(t *testing.T) {
	lf := NewLockfile()
	lf.AddRepo("sre/api", "https://github.com/org/api.git", "abc123", "main")
	lf.AddRepo("sre/lib", "https://github.com/org/lib.git", "def456", "main")

	if len(lf.Repositories) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(lf.Repositories))
	}

	repo := lf.GetRepo("sre/api")
	if repo == nil {
		t.Fatal("sre/api not found")
	}
	if repo.Commit != "abc123" {
		t.Errorf("commit: got %q, want abc123", repo.Commit)
	}
	if repo.DisplayPath != "sre/api" {
		t.Errorf("display_path: got %q, want sre/api", repo.DisplayPath)
	}
}

func TestAddRepoUpdate(t *testing.T) {
	lf := NewLockfile()
	lf.AddRepo("sre/api", "https://github.com/org/api.git", "abc123", "main")
	lf.AddDep("sre/api", "sre/lib", "lib-v1")
	lf.AddRepo("sre/api", "https://github.com/org/api.git", "new456", "main")

	repo := lf.GetRepo("sre/api")
	if repo.Commit != "new456" {
		t.Errorf("commit not updated: got %q", repo.Commit)
	}
	// Dependencies should be preserved.
	if _, ok := repo.Dependencies["sre/lib"]; !ok {
		t.Error("dependencies lost after update")
	}
}

func TestAddDep(t *testing.T) {
	lf := NewLockfile()
	lf.AddRepo("sre/api", "", "abc123", "")
	lf.AddDep("sre/api", "sre/lib", "lib-commit-1")
	lf.AddDep("sre/api", "sre/utils", "utils-commit-1")

	deps := lf.GetDeps("sre/api")
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps, got %d", len(deps))
	}
	if deps[0] != "sre/lib" || deps[1] != "sre/utils" {
		t.Errorf("deps: got %v, want [sre/lib sre/utils]", deps)
	}
}

func TestAddDepOnUnknownRepo(t *testing.T) {
	lf := NewLockfile()
	// Adding a dep on a repo that hasn't been AddRepo'd should auto-create.
	lf.AddDep("sre/new-repo", "sre/lib", "lib-commit")

	repo := lf.GetRepo("sre/new-repo")
	if repo == nil {
		t.Fatal("repo should be auto-created")
	}
	if len(repo.Dependencies) != 1 {
		t.Errorf("expected 1 dep, got %d", len(repo.Dependencies))
	}
}

func TestGetReverseDeps(t *testing.T) {
	lf := NewLockfile()
	lf.AddRepo("sre/api", "", "a1", "")
	lf.AddRepo("sre/lib", "", "l1", "")
	lf.AddRepo("sre/utils", "", "u1", "")
	lf.AddDep("sre/api", "sre/lib", "l1")
	lf.AddDep("sre/utils", "sre/lib", "l1")

	rev := lf.GetReverseDeps("sre/lib")
	if len(rev) != 2 {
		t.Fatalf("expected 2 reverse deps, got %d", len(rev))
	}
	if rev[0] != "sre/api" || rev[1] != "sre/utils" {
		t.Errorf("reverse deps: got %v", rev)
	}
}

func TestGetReverseDepsNone(t *testing.T) {
	lf := NewLockfile()
	lf.AddRepo("sre/alone", "", "x", "")
	if rev := lf.GetReverseDeps("sre/alone"); len(rev) != 0 {
		t.Errorf("expected no reverse deps, got %v", rev)
	}
}

func TestRepoPaths(t *testing.T) {
	lf := NewLockfile()
	lf.AddRepo("b", "", "", "")
	lf.AddRepo("a", "", "", "")
	lf.AddRepo("c", "", "", "")

	paths := lf.RepoPaths()
	if len(paths) != 3 || paths[0] != "a" || paths[1] != "b" || paths[2] != "c" {
		t.Errorf("RepoPaths: got %v", paths)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".monofs", "workspace.lock")

	lf := NewLockfile()
	lf.AddRepo("sre/api", "https://github.com/org/api.git", "abc123", "main")
	lf.AddDep("sre/api", "sre/lib", "lib-commit")
	lf.AddRepo("sre/lib", "https://github.com/org/lib.git", "lib-commit", "main")

	if err := lf.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadLockfile(path)
	if err != nil {
		t.Fatalf("LoadLockfile: %v", err)
	}

	if len(loaded.Repositories) != 2 {
		t.Fatalf("round-trip: expected 2 repos, got %d", len(loaded.Repositories))
	}
	api := loaded.GetRepo("sre/api")
	if api.Commit != "abc123" {
		t.Errorf("round-trip commit: got %q", api.Commit)
	}
	if api.Dependencies["sre/lib"].Commit != "lib-commit" {
		t.Errorf("round-trip dep commit: got %q", api.Dependencies["sre/lib"].Commit)
	}
}

func TestLoadLockfileNotExist(t *testing.T) {
	lf, err := LoadLockfile("/tmp/does-not-exist.lock")
	if err != nil {
		t.Fatalf("LoadLockfile should return empty, not error: %v", err)
	}
	if len(lf.Repositories) != 0 {
		t.Errorf("expected empty lockfile, got %d repos", len(lf.Repositories))
	}
}

func TestLoadLockfileInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.lock")
	os.WriteFile(path, []byte("not json"), 0644)
	_, err := LoadLockfile(path)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestMergeLockfiles(t *testing.T) {
	lf1 := NewLockfile()
	lf1.AddRepo("sre/api", "", "commit-api-1", "")
	lf1.AddDep("sre/api", "sre/lib", "lib-1")

	lf2 := NewLockfile()
	lf2.AddRepo("sre/api", "", "commit-api-2", "")
	lf2.AddDep("sre/api", "sre/utils", "utils-1")
	lf2.AddRepo("sre/new-repo", "", "new-1", "")

	merged := MergeLockfiles(lf1, lf2)

	// lf2's commit should win.
	api := merged.GetRepo("sre/api")
	if api.Commit != "commit-api-2" {
		t.Errorf("merge commit: got %q, want commit-api-2", api.Commit)
	}
	// Dependencies from both should be present.
	if len(api.Dependencies) != 2 {
		t.Errorf("merge deps: expected 2, got %d", len(api.Dependencies))
	}
	if _, ok := api.Dependencies["sre/lib"]; !ok {
		t.Error("merge lost lf1 dep")
	}
	if _, ok := api.Dependencies["sre/utils"]; !ok {
		t.Error("merge lost lf2 dep")
	}
	// lf2-only repo should be present.
	if merged.GetRepo("sre/new-repo") == nil {
		t.Error("merge lost lf2-only repo")
	}
}

func TestDiffRepos(t *testing.T) {
	lf1 := NewLockfile()
	lf1.AddRepo("a", "", "", "")
	lf1.AddRepo("b", "", "", "")
	lf1.AddRepo("c", "", "", "")

	lf2 := NewLockfile()
	lf2.AddRepo("b", "", "", "")
	lf2.AddRepo("c", "", "", "")
	lf2.AddRepo("d", "", "", "")

	added, removed := DiffRepos(lf1, lf2)
	if len(added) != 1 || added[0] != "a" {
		t.Errorf("added: got %v, want [a]", added)
	}
	if len(removed) != 1 || removed[0] != "d" {
		t.Errorf("removed: got %v, want [d]", removed)
	}
}

func TestGetDepsNil(t *testing.T) {
	lf := NewLockfile()
	// No repos added.
	if deps := lf.GetDeps("nonexistent"); deps != nil {
		t.Errorf("expected nil deps for nonexistent repo, got %v", deps)
	}
}

func TestGetRepoNil(t *testing.T) {
	var lf *WorkspaceLockfile
	if lf.GetRepo("x") != nil {
		t.Error("nil lockfile should return nil")
	}
	if lf.GetDeps("x") != nil {
		t.Error("nil lockfile GetDeps should return nil")
	}
}

func TestSavePrettyJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspace.lock")

	lf := NewLockfile()
	lf.AddRepo("sre/api", "https://github.com/org/api.git", "abc123", "main")
	lf.Save(path)

	data, _ := os.ReadFile(path)
	// Verify it's valid JSON.
	var check WorkspaceLockfile
	if err := json.Unmarshal(data, &check); err != nil {
		t.Fatalf("saved file is not valid JSON: %v", err)
	}
}

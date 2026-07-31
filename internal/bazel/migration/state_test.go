package migration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateTransitions(t *testing.T) {
	// Forward
	if StateNative.Next() != StateGenerating {
		t.Errorf("native.Next() = %s", StateNative.Next())
	}
	if StateGenerating.Next() != StatePartial {
		t.Errorf("generating.Next() = %s", StateGenerating.Next())
	}
	if StatePartial.Next() != StateActive {
		t.Errorf("partial.Next() = %s", StatePartial.Next())
	}
	if StateActive.Next() != StateHermetic {
		t.Errorf("active.Next() = %s", StateActive.Next())
	}
	if StateHermetic.Next() != "" {
		t.Error("hermetic.Next() should be empty")
	}

	// Backward
	if StateHermetic.Prev() != StateActive {
		t.Errorf("hermetic.Prev() = %s", StateHermetic.Prev())
	}
	if StateNative.Prev() != "" {
		t.Error("native.Prev() should be empty")
	}
}

func TestValidateStateTransition(t *testing.T) {
	// Valid forward steps
	if err := ValidateStateTransition(StateNative, StateGenerating); err != nil {
		t.Errorf("native→generating: %v", err)
	}
	if err := ValidateStateTransition(StateActive, StateHermetic); err != nil {
		t.Errorf("active→hermetic: %v", err)
	}

	// Valid backward steps
	if err := ValidateStateTransition(StateGenerating, StateNative); err != nil {
		t.Errorf("generating→native: %v", err)
	}

	// Invalid jumps
	if err := ValidateStateTransition(StateNative, StateActive); err == nil {
		t.Error("native→active should be invalid (skip generating+partial)")
	}
	if err := ValidateStateTransition(StateNative, StateHermetic); err == nil {
		t.Error("native→hermetic should be invalid")
	}
	if err := ValidateStateTransition(StateNative, StateNative); err == nil {
		t.Error("same state should be invalid (no-op)")
	}
}

func TestDefaultRepoConfig(t *testing.T) {
	cfg := DefaultRepoConfig(StateNative)
	if cfg.State != StateNative {
		t.Errorf("state: got %s", cfg.State)
	}
	if cfg.FallbackBuild == "" {
		t.Error("fallback build should not be empty")
	}
	if cfg.FallbackTest == "" {
		t.Error("fallback test should not be empty")
	}
}

func TestLoadSaveRepoConfig(t *testing.T) {
	dir := t.TempDir()

	// Load non-existent → native default.
	cfg, err := LoadRepoConfig(dir)
	if err != nil {
		t.Fatalf("LoadRepoConfig: %v", err)
	}
	if cfg.State != StateNative {
		t.Errorf("default state: got %s, want native", cfg.State)
	}

	// Change and save.
	cfg.State = StateActive
	cfg.FallbackBuild = "go build ./..."
	if err := SaveRepoConfig(dir, cfg); err != nil {
		t.Fatalf("SaveRepoConfig: %v", err)
	}

	// Reload.
	loaded, err := LoadRepoConfig(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.State != StateActive {
		t.Errorf("reloaded state: got %s, want active", loaded.State)
	}
	if loaded.FallbackBuild != "go build ./..." {
		t.Errorf("reloaded build: got %s", loaded.FallbackBuild)
	}
}

func TestDetectFallbackBuild(t *testing.T) {
	dir := t.TempDir()

	// No markers → default make.
	cmd := detectFallbackBuild(dir, "build")
	if cmd != "make build" {
		t.Errorf("empty dir: got %s, want make build", cmd)
	}

	// Go repo.
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0644)
	cmd = detectFallbackBuild(dir, "test")
	if cmd != "go test ./..." {
		t.Errorf("go dir: got %s, want go test ./...", cmd)
	}

	// TypeScript.
	os.Remove(filepath.Join(dir, "go.mod"))
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0644)
	cmd = detectFallbackBuild(dir, "build")
	if cmd != "npm run build" {
		t.Errorf("npm dir: got %s, want npm run build", cmd)
	}
}

func TestWorkspaceStatus(t *testing.T) {
	ws := &WorkspaceStatus{
		MountRoot:     "/mnt/monofs",
		TotalRepos:    4,
		ActiveCount:   2,
		HermeticCount: 1,
		Repos: []RepoStatus{
			{DisplayPath: "sre/api", State: StateActive, BuildSystem: "go"},
			{DisplayPath: "sre/lib", State: StateHermetic, BuildSystem: "go"},
			{DisplayPath: "sre/legacy", State: StateNative, BuildSystem: "make"},
			{DisplayPath: "sre/wip", State: StateGenerating, BuildSystem: "go"},
		},
	}

	// Adoption = (2+1)/4 = 75%
	if ws.AdoptionPercent() != 75.0 {
		t.Errorf("adoption: got %.1f, want 75.0", ws.AdoptionPercent())
	}

	// Status text
	txt := ws.Repos[0].StatusText()
	if !strings.Contains(txt, "active") {
		t.Errorf("status text: %s", txt)
	}
}

func TestValidatePrompt(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0644)

	warns, err := ValidatePrompt(dir, StateGenerating)
	if err != nil {
		t.Fatalf("ValidatePrompt: %v", err)
	}
	if len(warns) == 0 {
		t.Error("generating→partial should have validation warning")
	}

	warns, err = ValidatePrompt(dir, StateActive)
	if err != nil {
		t.Fatalf("ValidatePrompt: %v", err)
	}
	if len(warns) == 0 {
		t.Error("active→hermetic should have warning")
	}
}

func TestBuildSystemLabel(t *testing.T) {
	dir := t.TempDir()

	if label := BuildSystemLabel(dir); label != "make" {
		t.Errorf("empty dir: got %s, want make", label)
	}

	os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\n"), 0644)
	if label := BuildSystemLabel(dir); label != "cargo" {
		t.Errorf("cargo dir: got %s, want cargo", label)
	}
}

func TestStatusText(t *testing.T) {
	cases := []struct {
		state    State
		contains string
	}{
		{StateNative, "native"},
		{StateGenerating, "generating"},
		{StatePartial, "partial"},
		{StateActive, "active"},
		{StateHermetic, "hermetic"},
	}
	for _, c := range cases {
		r := RepoStatus{DisplayPath: "test", State: c.state, BuildSystem: "go"}
		txt := r.StatusText()
		if !strings.Contains(txt, c.contains) {
			t.Errorf("StatusText(%s): got %q, missing %q", c.state, txt, c.contains)
		}
	}
}

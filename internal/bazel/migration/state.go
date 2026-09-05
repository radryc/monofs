// Package migration provides incremental Bazel adoption tooling for
// MonoFS ingested repositories. Repos transition through states:
//
//	native → generating → partial → active → hermetic
//
// Each repo can have a bazel.yaml config file that controls generation.
package migration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// State represents a repo's Bazel adoption state.
type State string

const (
	// StateNative: no BUILD files, uses native build system (Makefile, go build, etc.).
	// The workspace-gen generates a genrule fallback so Bazel can still reference this repo.
	StateNative State = "native"

	// StateGenerating: BUILD files are being generated and validated.
	// Errors are reported but don't block. Bazel builds with --check_up_to_date (warn only).
	StateGenerating State = "generating"

	// StatePartial: some BUILD files exist, native build is still primary.
	// Bazel builds what it can, warns on gaps.
	StatePartial State = "partial"

	// StateActive: full BUILD coverage, Bazel is primary build system.
	// Native build fallback is removed.
	StateActive State = "active"

	// StateHermetic: Bazel-only, native build system removed entirely.
	// Only Bazel targets are built.
	StateHermetic State = "hermetic"
)

// ValidStates is the ordered migration path.
var ValidStates = []State{StateNative, StateGenerating, StatePartial, StateActive, StateHermetic}

// Valid reports whether the state is recognized.
func (s State) Valid() bool {
	for _, v := range ValidStates {
		if s == v {
			return true
		}
	}
	return false
}

// IsAtLeast reports whether this state is at or beyond the given state.
func (s State) IsAtLeast(other State) bool {
	for _, v := range ValidStates {
		if v == other {
			return true
		}
		if v == s {
			return false
		}
	}
	return false
}

// Next returns the next state in the adoption path, or empty if at hermetic.
func (s State) Next() State {
	for i, v := range ValidStates {
		if v == s && i+1 < len(ValidStates) {
			return ValidStates[i+1]
		}
	}
	return ""
}

// Prev returns the previous state, or empty if at native.
func (s State) Prev() State {
	for i, v := range ValidStates {
		if v == s && i > 0 {
			return ValidStates[i-1]
		}
	}
	return ""
}

// RepoConfig is the per-repo bazel.yaml configuration.
type RepoConfig struct {
	// State is the current adoption state.
	State State `yaml:"state"`

	// FallbackBuild is the native build command when state is not hermetic.
	// e.g. "make build", "go build ./...", "npm run build"
	FallbackBuild string `yaml:"fallback_build"`

	// FallbackTest is the native test command.
	FallbackTest string `yaml:"fallback_test"`

	// ExcludePatterns lists directories to skip during BUILD file generation.
	ExcludePatterns []string `yaml:"exclude_patterns"`

	// ManualTargets lists BUILD files that should never be overwritten.
	ManualTargets []string `yaml:"manual_targets"`
}

// DefaultRepoConfig returns a sensible default for a new repo.
func DefaultRepoConfig(state State) RepoConfig {
	return RepoConfig{
		State:         state,
		FallbackBuild: "make build",
		FallbackTest:  "make test",
	}
}

// LoadRepoConfig reads a bazel.yaml from a repo directory.
// If the file doesn't exist, returns a default native config.
func LoadRepoConfig(repoDir string) (*RepoConfig, error) {
	path := filepath.Join(repoDir, "bazel.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := DefaultRepoConfig(StateNative)
			// Try to auto-detect build system.
			cfg.FallbackBuild = detectFallbackBuild(repoDir, "build")
			cfg.FallbackTest = detectFallbackBuild(repoDir, "test")
			return &cfg, nil
		}
		return nil, fmt.Errorf("read bazel.yaml: %w", err)
	}

	var cfg RepoConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse bazel.yaml: %w", err)
	}
	if !cfg.State.Valid() {
		cfg.State = StateNative
	}
	return &cfg, nil
}

// SaveRepoConfig writes a bazel.yaml file for a repo.
func SaveRepoConfig(repoDir string, cfg *RepoConfig) error {
	path := filepath.Join(repoDir, "bazel.yaml")
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal bazel.yaml: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write bazel.yaml: %w", err)
	}
	return nil
}

// detectFallbackBuild detects the native build command for a repo.
func detectFallbackBuild(repoDir, action string) string {
	markers := []struct {
		file    string
		command string
	}{
		{"Makefile", "make " + action},
		{"go.mod", "go " + action + " ./..."},
		{"package.json", "npm run " + action},
		{"Cargo.toml", "cargo " + action},
		{"pom.xml", "mvn " + action},
		{"build.gradle", "gradle " + action},
	}

	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(repoDir, m.file)); err == nil {
			return m.command
		}
	}
	return "make " + action // safe default
}

// WorkspaceStatus summarizes Bazel adoption across all repos.
type WorkspaceStatus struct {
	MountRoot     string       `json:"mount_root"`
	Repos         []RepoStatus `json:"repos"`
	TotalRepos    int          `json:"total_repos"`
	ActiveCount   int          `json:"active_count"`
	HermeticCount int          `json:"hermetic_count"`
}

// RepoStatus is a single repo's Bazel state.
type RepoStatus struct {
	DisplayPath string `json:"display_path"`
	State       State  `json:"state"`
	BuildSystem string `json:"build_system"`
	TargetCount int    `json:"target_count,omitempty"`
	ErrorCount  int    `json:"error_count,omitempty"`
}

// StatusText returns a human-readable single-line summary.
func (r RepoStatus) StatusText() string {
	var icon string
	switch r.State {
	case StateNative:
		icon = "  native"
	case StateGenerating:
		icon = "  generating"
	case StatePartial:
		icon = "  partial"
	case StateActive:
		icon = "✓ active"
	case StateHermetic:
		icon = "✓ hermetic"
	}
	if r.TargetCount > 0 {
		return fmt.Sprintf("%-20s %s  %d targets", r.DisplayPath, icon, r.TargetCount)
	}
	return fmt.Sprintf("%-20s %s  (%s)", r.DisplayPath, icon, r.BuildSystem)
}

// AdoptionPercent returns the fraction of repos that are active or hermetic.
func (ws *WorkspaceStatus) AdoptionPercent() float64 {
	if ws.TotalRepos == 0 {
		return 0
	}
	return float64(ws.ActiveCount+ws.HermeticCount) / float64(ws.TotalRepos) * 100
}

// ValidateStateTransition checks if moving from oldState to newState is allowed.
func ValidateStateTransition(oldState, newState State) error {
	if !oldState.Valid() {
		return fmt.Errorf("invalid current state: %s", oldState)
	}
	if !newState.Valid() {
		return fmt.Errorf("invalid target state: %s", newState)
	}

	// Forward: must be the next state.
	if newState == oldState.Next() {
		return nil
	}
	// Backward: must be the previous state.
	if newState == oldState.Prev() {
		return nil
	}
	// Direct jump.
	if newState == oldState {
		return fmt.Errorf("already in state %s", oldState)
	}
	return fmt.Errorf("cannot jump from %s to %s (must go through intermediate states)", oldState, newState)
}

// BuildSystemLabel returns a human-readable build system name.
func BuildSystemLabel(repoDir string) string {
	markers := []string{"Makefile", "go.mod", "package.json", "Cargo.toml", "pom.xml", "build.gradle"}
	labels := []string{"make", "go", "npm", "cargo", "maven", "gradle"}
	for i, m := range markers {
		if _, err := os.Stat(filepath.Join(repoDir, m)); err == nil {
			return labels[i]
		}
	}
	return "make"
}

// ValidatePrompt checks whether a repo is ready to be promoted to the next state.
func ValidatePrompt(repoDir string, current State) ([]string, error) {
	var warnings []string

	switch current {
	case StateNative:
		// Check if BUILD files exist anywhere.
		hasBuildFiles := false
		filepath.WalkDir(repoDir, func(path string, d os.DirEntry, err error) error {
			if err != nil || hasBuildFiles {
				return err
			}
			if d.Name() == "BUILD.bazel" || d.Name() == "BUILD" {
				hasBuildFiles = true
			}
			return nil
		})
		if hasBuildFiles {
			warnings = append(warnings, "BUILD files already exist — consider 'generate' first")
		}
	case StateGenerating:
		// Remind to validate.
		warnings = append(warnings, "run 'validate' to check BUILD file correctness before promoting")
	case StatePartial:
		warnings = append(warnings, "some directories may lack BUILD files; run 'generate' to fill gaps")
	case StateActive:
		warnings = append(warnings, "promoting to hermetic removes native build fallback; ensure all targets are covered")
	}
	return warnings, nil
}

func init() {
	// Ensure yaml is imported.
	_ = strings.Join
}

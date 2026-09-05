// Package deps provides cross-repo dependency resolution for the
// virtual monorepo. It discovers which ingested repos depend on each
// other, pins versions in a lockfile, and resolves import paths to
// Bazel @repo_name//... targets.
package deps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// LockfileVersion is the current schema version of workspace.lock.
const LockfileVersion = 1

// WorkspaceLockfile represents the pinned dependency state of the
// entire virtual monorepo. It is stored at /.monofs/workspace.lock.
//
// Analogous to: Cargo.lock, go.sum, package-lock.json.
type WorkspaceLockfile struct {
	Version      int                   `json:"version"`
	GeneratedAt  string                `json:"generated_at"`
	Repositories map[string]LockedRepo `json:"repositories"`
}

// LockedRepo pins a single ingested repo to a specific state.
type LockedRepo struct {
	DisplayPath  string               `json:"display_path"`
	Source       string               `json:"source,omitempty"`
	Commit       string               `json:"commit"`
	Ref          string               `json:"ref,omitempty"`
	IngestedAt   string               `json:"ingested_at,omitempty"`
	Dependencies map[string]LockedDep `json:"dependencies,omitempty"`
}

// LockedDep records that display_path depends on dep_display_path
// at a specific commit.
type LockedDep struct {
	Commit     string `json:"commit"`
	ResolvedAt string `json:"resolved_at,omitempty"`
}

// NewLockfile creates an empty lockfile with the current timestamp.
func NewLockfile() *WorkspaceLockfile {
	return &WorkspaceLockfile{
		Version:      LockfileVersion,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Repositories: make(map[string]LockedRepo),
	}
}

// AddRepo ensures a repo entry exists in the lockfile. If the repo
// already exists, its dependencies are preserved (merged).
func (lf *WorkspaceLockfile) AddRepo(displayPath, source, commit, ref string) {
	existing, ok := lf.Repositories[displayPath]
	if !ok {
		lf.Repositories[displayPath] = LockedRepo{
			DisplayPath:  displayPath,
			Source:       source,
			Commit:       commit,
			Ref:          ref,
			IngestedAt:   time.Now().UTC().Format(time.RFC3339),
			Dependencies: make(map[string]LockedDep),
		}
		return
	}
	// Update mutable fields; preserve existing dependencies.
	existing.Source = source
	existing.Commit = commit
	existing.Ref = ref
	if existing.IngestedAt == "" {
		existing.IngestedAt = time.Now().UTC().Format(time.RFC3339)
	}
	lf.Repositories[displayPath] = existing
}

// AddDep records that displayPath depends on depDisplayPath at commit.
func (lf *WorkspaceLockfile) AddDep(displayPath, depDisplayPath, commit string) {
	repo, ok := lf.Repositories[displayPath]
	if !ok {
		repo = LockedRepo{
			DisplayPath:  displayPath,
			Dependencies: make(map[string]LockedDep),
		}
	}
	if repo.Dependencies == nil {
		repo.Dependencies = make(map[string]LockedDep)
	}
	repo.Dependencies[depDisplayPath] = LockedDep{
		Commit:     commit,
		ResolvedAt: time.Now().UTC().Format(time.RFC3339),
	}
	lf.Repositories[displayPath] = repo
}

// GetRepo returns the locked repo entry, or nil if not found.
func (lf *WorkspaceLockfile) GetRepo(displayPath string) *LockedRepo {
	if lf == nil {
		return nil
	}
	repo, ok := lf.Repositories[displayPath]
	if !ok {
		return nil
	}
	return &repo
}

// GetDeps returns the direct dependencies of a repo.
func (lf *WorkspaceLockfile) GetDeps(displayPath string) []string {
	repo := lf.GetRepo(displayPath)
	if repo == nil || repo.Dependencies == nil {
		return nil
	}
	var deps []string
	for dep := range repo.Dependencies {
		deps = append(deps, dep)
	}
	sort.Strings(deps)
	return deps
}

// GetReverseDeps returns repos that depend on displayPath.
func (lf *WorkspaceLockfile) GetReverseDeps(displayPath string) []string {
	var rev []string
	for repoPath, repo := range lf.Repositories {
		if repoPath == displayPath {
			continue
		}
		if _, ok := repo.Dependencies[displayPath]; ok {
			rev = append(rev, repoPath)
		}
	}
	sort.Strings(rev)
	return rev
}

// RepoPaths returns all known repo paths in sorted order.
func (lf *WorkspaceLockfile) RepoPaths() []string {
	var paths []string
	for p := range lf.Repositories {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// Save writes the lockfile to disk.
func (lf *WorkspaceLockfile) Save(path string) error {
	data, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal lockfile: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("mkdir for lockfile: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
		return fmt.Errorf("write lockfile: %w", err)
	}
	return nil
}

// LoadLockfile reads and parses a workspace.lock file.
func LoadLockfile(path string) (*WorkspaceLockfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewLockfile(), nil
		}
		return nil, fmt.Errorf("read lockfile: %w", err)
	}
	var lf WorkspaceLockfile
	if err := json.Unmarshal(data, &lf); err != nil {
		return nil, fmt.Errorf("parse lockfile: %w", err)
	}
	if lf.Repositories == nil {
		lf.Repositories = make(map[string]LockedRepo)
	}
	return &lf, nil
}

// MergeLockfiles merges `other` into `lf`. For repos present in both,
// `other`'s commit wins, but dependencies from both are combined.
func MergeLockfiles(lf, other *WorkspaceLockfile) *WorkspaceLockfile {
	result := &WorkspaceLockfile{
		Version:      lf.Version,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Repositories: make(map[string]LockedRepo),
	}

	// Start with lf's repos.
	for k, v := range lf.Repositories {
		repo := LockedRepo{
			DisplayPath:  v.DisplayPath,
			Source:       v.Source,
			Commit:       v.Commit,
			Ref:          v.Ref,
			IngestedAt:   v.IngestedAt,
			Dependencies: make(map[string]LockedDep),
		}
		for dk, dv := range v.Dependencies {
			repo.Dependencies[dk] = dv
		}
		result.Repositories[k] = repo
	}

	// Merge other: repos with newer commits win.
	for k, v := range other.Repositories {
		existing, ok := result.Repositories[k]
		if !ok {
			repo := LockedRepo{
				DisplayPath:  v.DisplayPath,
				Source:       v.Source,
				Commit:       v.Commit,
				Ref:          v.Ref,
				IngestedAt:   v.IngestedAt,
				Dependencies: make(map[string]LockedDep),
			}
			for dk, dv := range v.Dependencies {
				repo.Dependencies[dk] = dv
			}
			result.Repositories[k] = repo
			continue
		}
		// Merge dependencies.
		if existing.Dependencies == nil {
			existing.Dependencies = make(map[string]LockedDep)
		}
		for dk, dv := range v.Dependencies {
			if _, ok := existing.Dependencies[dk]; !ok {
				existing.Dependencies[dk] = dv
			}
		}
		// Prefer other's commit (assume newer).
		existing.Commit = v.Commit
		existing.Ref = v.Ref
		existing.Source = v.Source
		result.Repositories[k] = existing
	}

	return result
}

// DiffRepos returns repos present in `lf` but not in `other`.
func DiffRepos(lf, other *WorkspaceLockfile) (added, removed []string) {
	inLf := make(map[string]bool)
	for p := range lf.Repositories {
		inLf[p] = true
	}
	inOther := make(map[string]bool)
	for p := range other.Repositories {
		inOther[p] = true
	}
	for p := range inLf {
		if !inOther[p] {
			added = append(added, p)
		}
	}
	for p := range inOther {
		if !inLf[p] {
			removed = append(removed, p)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return
}

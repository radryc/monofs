package fuse

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/radryc/monofs/internal/client"
)

const (
	syntheticGitignoreName = ".gitignore"
	workspaceManifestTTL   = 30 * time.Second
	monorepoGitignore      = "/dependency/\n/guardian/\n/guardian-system/\n**/.git\n"
)

// WorkspaceExclusionReason explains why a repository or path is hidden from
// the synthetic source-root projection.
type WorkspaceExclusionReason string

const (
	WorkspaceExcludedNone            WorkspaceExclusionReason = ""
	WorkspaceExcludedSystemNamespace WorkspaceExclusionReason = "system-namespace"
	WorkspaceExcludedNestedGit       WorkspaceExclusionReason = "nested-git"
)

var hiddenWorkspaceRoots = map[string]WorkspaceExclusionReason{
	"dependency":      WorkspaceExcludedSystemNamespace,
	"guardian":        WorkspaceExcludedSystemNamespace,
	"guardian-system": WorkspaceExcludedSystemNamespace,
}

// WorkspaceManifestEntry describes a repository and whether it participates in
// the projected source-root view.
type WorkspaceManifestEntry struct {
	Repository      client.WorkspaceRepository
	Included        bool
	ExclusionReason WorkspaceExclusionReason
}

// WorkspacePathResolution describes how a path maps into the workspace.
type WorkspacePathResolution struct {
	Path            string
	Repository      *client.WorkspaceRepository
	Included        bool
	ExclusionReason WorkspaceExclusionReason
}

// WorkspaceManifest caches repository discovery so the FUSE layer can reason
// about the synthetic source-root view without repeatedly querying the cluster.
type WorkspaceManifest struct {
	provider client.WorkspaceMetadataProvider
	ttl      time.Duration

	mu        sync.RWMutex
	entries   []WorkspaceManifestEntry
	fetchedAt time.Time
}

func NewWorkspaceManifest(provider client.WorkspaceMetadataProvider) *WorkspaceManifest {
	return &WorkspaceManifest{
		provider: provider,
		ttl:      workspaceManifestTTL,
	}
}

// List returns the latest discovered repositories along with their inclusion
// status in the projected workspace.
func (m *WorkspaceManifest) List(ctx context.Context) ([]WorkspaceManifestEntry, error) {
	if m == nil || m.provider == nil {
		return nil, nil
	}

	m.mu.RLock()
	if len(m.entries) > 0 && time.Since(m.fetchedAt) < m.ttl {
		entries := append([]WorkspaceManifestEntry(nil), m.entries...)
		m.mu.RUnlock()
		return entries, nil
	}
	m.mu.RUnlock()

	repos, err := m.provider.ListWorkspaceRepositories(ctx)
	if err != nil {
		return nil, err
	}

	entries := make([]WorkspaceManifestEntry, 0, len(repos))
	for _, repo := range repos {
		reason, hidden := workspaceHiddenPath(repo.DisplayPath)
		entries = append(entries, WorkspaceManifestEntry{
			Repository:      repo,
			Included:        !hidden,
			ExclusionReason: reason,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Repository.DisplayPath == entries[j].Repository.DisplayPath {
			return entries[i].Repository.StorageID < entries[j].Repository.StorageID
		}
		return entries[i].Repository.DisplayPath < entries[j].Repository.DisplayPath
	})

	m.mu.Lock()
	m.entries = entries
	m.fetchedAt = time.Now()
	m.mu.Unlock()

	return append([]WorkspaceManifestEntry(nil), entries...), nil
}

// ResolvePath resolves a path against the current manifest and reports whether
// that path is part of the projected source-root.
func (m *WorkspaceManifest) ResolvePath(ctx context.Context, path string) (*WorkspacePathResolution, error) {
	trimmed := strings.Trim(path, "/")
	reason, hidden := workspaceHiddenPath(trimmed)
	resolution := &WorkspacePathResolution{
		Path:            trimmed,
		Included:        !hidden,
		ExclusionReason: reason,
	}

	if m == nil || m.provider == nil || trimmed == "" {
		return resolution, nil
	}

	repo, err := m.provider.ResolveWorkspacePath(ctx, trimmed)
	if err != nil {
		if errors.Is(err, client.ErrWorkspacePathNotFound) {
			return resolution, nil
		}
		return nil, err
	}

	resolution.Repository = repo
	return resolution, nil
}

func (m *WorkspaceManifest) ShouldHidePath(path string) bool {
	if m == nil {
		return false
	}
	_, hidden := workspaceHiddenPath(path)
	return hidden
}

func (m *WorkspaceManifest) ShouldHideChild(parentPath, name string) bool {
	if m == nil {
		return false
	}
	if parentPath == "" && name == syntheticGitignoreName {
		return false
	}
	_, hidden := workspaceHiddenPath(joinWorkspacePath(parentPath, name))
	return hidden
}

func (m *WorkspaceManifest) FilterDirEntries(path string, entries []fuse.DirEntry) []fuse.DirEntry {
	if m == nil {
		return entries
	}

	filtered := make([]fuse.DirEntry, 0, len(entries)+1)
	seen := make(map[string]struct{}, len(entries)+1)
	for _, entry := range entries {
		if m.ShouldHideChild(path, entry.Name) {
			continue
		}
		if _, exists := seen[entry.Name]; exists {
			continue
		}
		seen[entry.Name] = struct{}{}
		filtered = append(filtered, entry)
	}

	if path == "" {
		if _, exists := seen[syntheticGitignoreName]; !exists {
			filtered = append(filtered, fuse.DirEntry{
				Name: syntheticGitignoreName,
				Mode: 0444 | uint32(syscall.S_IFREG),
				Ino:  hashPathForNode(syntheticGitignoreName),
			})
		}
	}

	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Name < filtered[j].Name
	})

	return filtered
}

func (m *WorkspaceManifest) GitignoreContent() []byte {
	if m == nil {
		return nil
	}
	return []byte(monorepoGitignore)
}

func joinWorkspacePath(parentPath, name string) string {
	name = strings.Trim(name, "/")
	if parentPath == "" {
		return name
	}
	return parentPath + "/" + name
}

func workspaceHiddenPath(path string) (WorkspaceExclusionReason, bool) {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return WorkspaceExcludedNone, false
	}

	parts := strings.Split(trimmed, "/")
	if reason, exists := hiddenWorkspaceRoots[parts[0]]; exists {
		return reason, true
	}
	for _, part := range parts {
		if part == ".git" {
			return WorkspaceExcludedNestedGit, true
		}
	}

	return WorkspaceExcludedNone, false
}
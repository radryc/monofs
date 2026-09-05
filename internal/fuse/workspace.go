package fuse

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/radryc/monofs/internal/client"
)

const (
	syntheticWorkspaceGitName        = ".git"
	syntheticGitignoreName           = ".gitignore"
	syntheticWorkspaceControlDirName = ".monofs"
	syntheticWorkspaceSystemDirName  = "system"
	syntheticWorkspaceManifestName   = "workspace.json"
	syntheticWorkspaceControlPath    = ".monofs"
	syntheticWorkspaceSystemPath     = ".monofs/system"
	syntheticWorkspaceManifestPath   = ".monofs/workspace.json"
	workspaceManifestTTL             = 30 * time.Second
	monorepoGitignore                = "/.git\n/dependency/\n/doctor/\n/guardian/\n/guardian-system/\n/.monofs/\n**/.git\n"
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
	"doctor":          WorkspaceExcludedSystemNamespace,
	"guardian":        WorkspaceExcludedSystemNamespace,
	"guardian-system": WorkspaceExcludedSystemNamespace,
}

var reservedWorkspaceRoots = map[string]WorkspaceExclusionReason{
	"doctor":          WorkspaceExcludedSystemNamespace,
	"guardian":        WorkspaceExcludedSystemNamespace,
	"guardian-system": WorkspaceExcludedSystemNamespace,
}

var excludedWorkspaceRoots = map[string]WorkspaceExclusionReason{
	"dependency":      WorkspaceExcludedSystemNamespace,
	"doctor":          WorkspaceExcludedSystemNamespace,
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
		reason, hidden := workspaceExcludedPath(repo.DisplayPath)
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
	reason, hidden := workspaceExcludedPath(trimmed)
	resolution := &WorkspacePathResolution{
		Path:            trimmed,
		Included:        !hidden,
		ExclusionReason: reason,
	}

	if m == nil || m.provider == nil || trimmed == "" {
		return resolution, nil
	}

	entries, err := m.List(ctx)
	if err != nil {
		return nil, err
	}

	var match *WorkspaceManifestEntry
	for i := range entries {
		entry := entries[i]
		displayPath := entry.Repository.DisplayPath
		if trimmed != displayPath && !strings.HasPrefix(trimmed, displayPath+"/") {
			continue
		}
		if match == nil || len(displayPath) > len(match.Repository.DisplayPath) {
			candidate := entry
			match = &candidate
		}
	}

	if match == nil {
		return resolution, nil
	}

	repo := match.Repository
	resolution.Repository = &repo
	if !match.Included {
		resolution.Included = false
		if resolution.ExclusionReason == WorkspaceExcludedNone {
			resolution.ExclusionReason = match.ExclusionReason
		}
	}
	return resolution, nil
}

func (m *WorkspaceManifest) ShouldHidePath(path string) bool {
	if m == nil {
		return false
	}
	_, hidden := workspaceHiddenPath(path)
	return hidden
}

func (m *WorkspaceManifest) ShouldReserveRoot(name string) bool {
	if m == nil {
		return false
	}
	if name == syntheticWorkspaceControlDirName {
		return true
	}
	_, reserved := workspacePathExclusion(name, reservedWorkspaceRoots)
	return reserved
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
		if _, exists := seen[syntheticWorkspaceControlDirName]; !exists {
			filtered = append(filtered, fuse.DirEntry{
				Name: syntheticWorkspaceControlDirName,
				Mode: 0555 | uint32(syscall.S_IFDIR),
				Ino:  hashPathForNode(syntheticWorkspaceControlPath),
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

func (m *WorkspaceManifest) Invalidate() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.entries = nil
	m.fetchedAt = time.Time{}
	m.mu.Unlock()
}

func (m *WorkspaceManifest) JSONContent(ctx context.Context) ([]byte, error) {
	if m == nil {
		return []byte("{\"repositories\":[]}"), nil
	}

	entries, err := m.List(ctx)
	if err != nil {
		return nil, err
	}

	type manifestRepository struct {
		StorageID       string                   `json:"storage_id"`
		DisplayPath     string                   `json:"display_path"`
		Source          string                   `json:"source,omitempty"`
		Ref             string                   `json:"ref,omitempty"`
		CommitHash      string                   `json:"commit_hash,omitempty"`
		CommitTime      int64                    `json:"commit_time,omitempty"`
		CommitMessage   string                   `json:"commit_message,omitempty"`
		Included        bool                     `json:"included"`
		ExclusionReason WorkspaceExclusionReason `json:"exclusion_reason,omitempty"`
	}

	doc := struct {
		GeneratedAt  string               `json:"generated_at"`
		Repositories []manifestRepository `json:"repositories"`
	}{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}

	for _, entry := range entries {
		doc.Repositories = append(doc.Repositories, manifestRepository{
			StorageID:       entry.Repository.StorageID,
			DisplayPath:     entry.Repository.DisplayPath,
			Source:          entry.Repository.Source,
			Ref:             entry.Repository.Ref,
			CommitHash:      entry.Repository.CommitHash,
			CommitTime:      entry.Repository.CommitTime,
			CommitMessage:   entry.Repository.CommitMessage,
			Included:        entry.Included,
			ExclusionReason: entry.ExclusionReason,
		})
	}

	return json.MarshalIndent(doc, "", "  ")
}

func joinWorkspacePath(parentPath, name string) string {
	name = strings.Trim(name, "/")
	if parentPath == "" {
		return name
	}
	return parentPath + "/" + name
}

func isWorkspaceSystemPath(path string) bool {
	trimmed := strings.Trim(path, "/")
	return trimmed == syntheticWorkspaceSystemPath || strings.HasPrefix(trimmed, syntheticWorkspaceSystemPath+"/")
}

func isWorkspaceReadOnlyPath(path string) bool {
	trimmed := strings.Trim(path, "/")
	if trimmed == syntheticWorkspaceControlPath || trimmed == syntheticWorkspaceManifestPath {
		return true
	}
	return isWorkspaceSystemPath(trimmed)
}

func backendPathForSystemView(path string) (string, bool) {
	trimmed := strings.Trim(path, "/")
	if trimmed == syntheticWorkspaceSystemPath {
		return "", true
	}
	prefix := syntheticWorkspaceSystemPath + "/"
	if strings.HasPrefix(trimmed, prefix) {
		return strings.TrimPrefix(trimmed, prefix), true
	}
	return "", false
}

func workspaceHiddenPath(path string) (WorkspaceExclusionReason, bool) {
	return workspacePathExclusion(path, hiddenWorkspaceRoots)
}

func workspaceExcludedPath(path string) (WorkspaceExclusionReason, bool) {
	return workspacePathExclusion(path, excludedWorkspaceRoots)
}

func workspacePathExclusion(path string, roots map[string]WorkspaceExclusionReason) (WorkspaceExclusionReason, bool) {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return WorkspaceExcludedNone, false
	}
	if trimmed == syntheticWorkspaceGitName {
		return WorkspaceExcludedNone, false
	}
	if trimmed == syntheticWorkspaceControlPath || strings.HasPrefix(trimmed, syntheticWorkspaceControlPath+"/") {
		return WorkspaceExcludedNone, false
	}

	parts := strings.Split(trimmed, "/")
	if reason, exists := roots[parts[0]]; exists {
		return reason, true
	}
	for _, part := range parts {
		if part == ".git" {
			return WorkspaceExcludedNestedGit, true
		}
	}

	return WorkspaceExcludedNone, false
}

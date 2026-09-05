// Package bazel provides Bazel integration for MonoFS workspaces.
//
// It generates synthetic WORKSPACE, MODULE.bazel, .bazelrc, and BUILD.bazel
// files at the virtual monorepo mount root so that stock Bazel can build
// across all ingested repositories.
package bazel

import "strings"

// WorkspaceManifest mirrors the JSON document served at
// /.monofs/workspace.json inside a virtual-monorepo mount.
type WorkspaceManifest struct {
	GeneratedAt  string               `json:"generated_at"`
	Repositories []ManifestRepository `json:"repositories"`
}

// ManifestRepository describes one ingested repo visible in the workspace.
type ManifestRepository struct {
	StorageID       string `json:"storage_id"`
	DisplayPath     string `json:"display_path"`
	Source          string `json:"source,omitempty"`
	Ref             string `json:"ref,omitempty"`
	CommitHash      string `json:"commit_hash,omitempty"`
	CommitTime      int64  `json:"commit_time,omitempty"`
	CommitMessage   string `json:"commit_message,omitempty"`
	Included        bool   `json:"included"`
	ExclusionReason string `json:"exclusion_reason,omitempty"`
}

// IncludedRepos returns only repositories that participate in the
// projected source-root view (i.e. Included=true and display_path is not
// a system namespace).
func (m *WorkspaceManifest) IncludedRepos() []ManifestRepository {
	if m == nil {
		return nil
	}
	var out []ManifestRepository
	for _, r := range m.Repositories {
		if !r.Included {
			continue
		}
		out = append(out, r)
	}
	return out
}

// DisplayPathToModuleName converts a MonoFS display path (e.g.
// "sre/platform-api") into a valid Bazel module name ("platform_api").
// Module names must match [a-z][a-z0-9_]*.
func DisplayPathToModuleName(dp string) string {
	parts := strings.Split(dp, "/")
	last := parts[len(parts)-1]
	last = strings.ReplaceAll(last, "-", "_")
	last = strings.ReplaceAll(last, ".", "_")
	last = strings.ToLower(last)
	// Guard against empty or invalid leading characters.
	if last == "" {
		last = "repo"
	}
	if last[0] >= '0' && last[0] <= '9' {
		last = "r_" + last
	}
	return last
}

// Generator produces Bazel workspace files for a virtual monorepo.
type Generator struct {
	// MountRoot is the absolute path to the FUSE mount point (e.g. /mnt/monofs).
	MountRoot string

	// BazelVersion pins the Bazel version in .bazelversion.
	// When empty, the generator skips writing .bazelversion.
	BazelVersion string

	// CacheEnabled generates --remote_cache config in .bazelrc when true.
	// The cache address is read from CacheAddr.
	CacheEnabled bool
	CacheAddr    string

	// ExecutorEnabled generates --remote_executor config in .bazelrc when true.
	ExecutorEnabled bool
	ExecutorAddr    string
}

// DefaultBazelVersion is the pinned Bazel version used when none is configured.
const DefaultBazelVersion = "7.4.0"

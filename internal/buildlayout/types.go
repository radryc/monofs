// Package buildlayout defines the interface for build system layout mappers.
// Layout mappers transform ingested repo files into virtual layout paths that
// match build tool expectations (Go modules, npm, Maven, etc.).
package buildlayout

import "fmt"

// LayoutMapper transforms ingested repo files into virtual layout paths.
// Each build system implements this interface once.
//
// LIFECYCLE: Called by Router AFTER successful ingestion.
// INPUT:    Original repo info + file list collected during WalkFiles().
// OUTPUT:   List of VirtualEntry defining new displayPaths.
type LayoutMapper interface {
	// Type returns a unique identifier (e.g., "go", "bazel", "npm").
	Type() string

	// Matches returns true if this mapper should handle the given repo.
	// Called with the original ingestion parameters.
	Matches(info RepoInfo) bool

	// MapPaths generates virtual layout entries for a repo's files.
	// Each VirtualEntry creates a new displayPath in the server's NutsDB.
	// The virtual files reference the SAME blob_hash as the original.
	//
	// `files` is the complete list of files from the ingestion WalkFiles().
	// `info` contains displayPath, storageID, source, ref, ingestionType, fetchType.
	MapPaths(info RepoInfo, files []FileInfo) ([]VirtualEntry, error)

	// ParseDependencyFile reads a dependency manifest and returns dependencies.
	// Used by `ingest-deps` CLI to bulk-ingest all deps of a project.
	ParseDependencyFile(content []byte) ([]Dependency, error)
}

// RepoInfo contains the context about the repo being ingested.
type RepoInfo struct {
	DisplayPath   string            // e.g., "github.com/google/uuid@v1.6.0"
	StorageID     string            // SHA-256(DisplayPath)
	Source        string            // Original source URL/path
	Ref           string            // Branch, tag, or version
	IngestionType string            // "git", "go"
	FetchType     string            // "git", "gomod"
	Config        map[string]string // Backend-specific config
}

// FileInfo describes a file from WalkFiles().
type FileInfo struct {
	Path            string            // Relative path: "uuid.go", "internal/doc.go"
	BlobHash        string            // Content hash (from storage.FileMetadata.ContentHash)
	Size            uint64            // File size in bytes
	Mode            uint32            // Unix file mode
	Mtime           int64             // Modification time (Unix seconds)
	Source          string            // Source URL (copied from ingestion)
	BackendMetadata map[string]string // Backend-specific metadata (e.g., module_path, version)
}

// VirtualEntry is one virtual path the mapper wants to create.
type VirtualEntry struct {
	// VirtualDisplayPath is the new top-level repo path.
	// Example: "go-modules/pkg/mod/github.com/google/uuid@v1.6.0"
	// This becomes a new entry in bucketRepoLookup on server nodes.
	VirtualDisplayPath string

	// VirtualFilePath is the file path within the virtual repo.
	// Usually same as OriginalFilePath.
	VirtualFilePath string

	// OriginalFilePath is the file path in the original repo.
	// Used to look up BlobHash, Size, Mode from the collected FileInfo list.
	OriginalFilePath string
}

// Dependency represents one dependency from a manifest file.
type Dependency struct {
	Module  string // e.g., "github.com/google/uuid"
	Version string // e.g., "v1.6.0"
	Source  string // Full source for ingestion: "github.com/google/uuid@v1.6.0"
}

// LayoutMapperRegistry holds all registered layout mappers.
type LayoutMapperRegistry struct {
	mappers []LayoutMapper
}

// NewRegistry creates a new empty registry.
func NewRegistry() *LayoutMapperRegistry {
	return &LayoutMapperRegistry{}
}

// Register adds a layout mapper to the registry.
func (r *LayoutMapperRegistry) Register(m LayoutMapper) {
	r.mappers = append(r.mappers, m)
}

// MapAll runs all matching mappers and collects all virtual entries.
// Returns nil, nil if no mappers match (not an error).
func (r *LayoutMapperRegistry) MapAll(info RepoInfo, files []FileInfo) ([]VirtualEntry, error) {
	var allEntries []VirtualEntry
	for _, m := range r.mappers {
		if m.Matches(info) {
			entries, err := m.MapPaths(info, files)
			if err != nil {
				return nil, fmt.Errorf("layout mapper %q failed: %w", m.Type(), err)
			}
			allEntries = append(allEntries, entries...)
		}
	}
	return allEntries, nil
}

// HasMappers returns true if any mappers are registered.
func (r *LayoutMapperRegistry) HasMappers() bool {
	return len(r.mappers) > 0
}

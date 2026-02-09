package router

import (
	"fmt"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage"
)

// CargoIngestionHandler handles Cargo crate ingestion.
type CargoIngestionHandler struct{}

func (h *CargoIngestionHandler) Type() pb.IngestionType {
	return pb.IngestionType_INGESTION_CARGO
}

func (h *CargoIngestionHandler) NormalizeDisplayPath(source string, sourceID string) string {
	if sourceID != "" {
		return sourceID
	}
	// Return empty — the canonical GitHub path will be extracted from Cargo.toml
	// metadata via ExtractCanonicalPath() during WalkFiles.
	// The storageID for dedup is generated from sourceURL when displayPath is empty.
	return ""
}

func (h *CargoIngestionHandler) AddDirectoryPrefix(displayPath string, source string) string {
	// No prefix - crates are stored at their canonical GitHub path
	// The mapper creates .cargo/registry/src/ as a virtual hard link
	return displayPath
}

func (h *CargoIngestionHandler) GetDefaultRef() string {
	return "latest"
}

func (h *CargoIngestionHandler) ValidateSource(source string, ref string) error {
	if source == "" {
		return fmt.Errorf("crate name is required")
	}
	return nil
}

func (h *CargoIngestionHandler) GetStorageType() storage.IngestionType {
	return storage.IngestionTypeCargo
}

func (h *CargoIngestionHandler) GetFetchType() pb.SourceType {
	return pb.SourceType_SOURCE_TYPE_CARGO
}
func (h *CargoIngestionHandler) ExtractCanonicalPath(metadata map[string]string) string {
	if repoURL := metadata["repository_url"]; repoURL != "" {
		// Normalize GitHub URL to path format
		return normalizeGitURL(repoURL)
	}
	// Fallback to crates.io URL if no repository found
	if crateName := metadata["crate_name"]; crateName != "" {
		version := metadata["version"]
		if version != "" {
			return fmt.Sprintf("crates.io/%s@%s", crateName, version)
		}
		return "crates.io/" + crateName
	}
	return ""
}

package router

import (
	"fmt"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage"
)

// extractPackageAndVersion parses package name and version from source string.
// Handles both "package@version" and scoped "@scope/package@version" formats.
func extractPackageAndVersion(source string) (string, string) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", ""
	}

	// Handle scoped packages: @scope/package@version
	if strings.HasPrefix(source, "@") {
		// Find the second @ which separates package from version
		parts := strings.SplitN(source[1:], "@", 2) // Skip first @ and split on second
		if len(parts) == 2 {
			return "@" + parts[0], parts[1] // Return @scope/package, version
		}
		return "@" + parts[0], "" // Scoped package without version
	}

	// Handle regular packages: package@version
	parts := strings.SplitN(source, "@", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return parts[0], "" // Package without version
}

// NpmIngestionHandler handles npm package ingestion.
type NpmIngestionHandler struct{}

func (h *NpmIngestionHandler) Type() pb.IngestionType {
	return pb.IngestionType_INGESTION_NPM
}

func (h *NpmIngestionHandler) NormalizeDisplayPath(source string, sourceID string) string {
	if sourceID != "" {
		return sourceID
	}
	// Return empty — the canonical GitHub path will be extracted from package.json
	// metadata via ExtractCanonicalPath() during WalkFiles.
	// The storageID for dedup is generated from sourceURL when displayPath is empty.
	return ""
}

func (h *NpmIngestionHandler) AddDirectoryPrefix(displayPath string, source string) string {
	// No prefix - packages are stored at their canonical GitHub path
	// The mapper creates node_modules/ as a virtual hard link
	return displayPath
}

func (h *NpmIngestionHandler) GetDefaultRef() string {
	// npm packages MUST have an explicit version - no default
	return ""
}

func (h *NpmIngestionHandler) ValidateSource(source string, ref string) error {
	if source == "" {
		return fmt.Errorf("package name is required")
	}
	// Ensure version is explicitly provided
	if ref == "" && !strings.Contains(source, "@") {
		return fmt.Errorf("version is required for npm packages (use package@version format or specify ref)")
	}
	return nil
}

func (h *NpmIngestionHandler) GetStorageType() storage.IngestionType {
	return storage.IngestionTypeNpm
}

func (h *NpmIngestionHandler) GetFetchType() storage.FetchType {
	return storage.FetchTypeNpm
}
func (h *NpmIngestionHandler) ExtractCanonicalPath(metadata map[string]string) string {
	// Extract canonical GitHub path from package metadata.
	// This becomes the primary storage location during initial ingestion.
	// The mapper will then create a virtual hard link at node_modules/.
	if repoURL := metadata["repository_url"]; repoURL != "" {
		// Normalize GitHub URL to path format
		return normalizeGitURL(repoURL)
	}
	// Fallback to npm registry URL if no repository found
	if packageName := metadata["package_name"]; packageName != "" {
		version := metadata["version"]
		if version != "" {
			return fmt.Sprintf("registry.npmjs.org/%s@%s", packageName, version)
		}
		return "registry.npmjs.org/" + packageName
	}
	return ""
}

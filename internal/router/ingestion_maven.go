package router

import (
	"fmt"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage"
)

// MavenIngestionHandler handles Maven artifact ingestion.
type MavenIngestionHandler struct{}

func (h *MavenIngestionHandler) Type() pb.IngestionType {
	return pb.IngestionType_INGESTION_MAVEN
}

func (h *MavenIngestionHandler) NormalizeDisplayPath(source string, sourceID string) string {
	if sourceID != "" {
		return sourceID
	}
	// Return empty — the canonical GitHub path will be extracted from pom.xml
	// metadata via ExtractCanonicalPath() during WalkFiles.
	// The storageID for dedup is generated from sourceURL when displayPath is empty.
	return ""
}

func (h *MavenIngestionHandler) AddDirectoryPrefix(displayPath string, source string) string {
	// No prefix - artifacts are stored at their canonical GitHub path
	// The mapper creates .m2/repository/ as a virtual hard link
	return displayPath
}

func (h *MavenIngestionHandler) GetDefaultRef() string {
	return "" // Version is required in source
}

func (h *MavenIngestionHandler) ValidateSource(source string, ref string) error {
	if source == "" {
		return fmt.Errorf("maven coordinates are required")
	}
	if strings.Contains(source, ":") {
		parts := strings.Split(source, ":")
		if len(parts) != 3 {
			return fmt.Errorf("maven source must be in format groupId:artifactId:version")
		}
	}
	return nil
}

func (h *MavenIngestionHandler) GetStorageType() storage.IngestionType {
	return storage.IngestionTypeMaven
}

func (h *MavenIngestionHandler) GetFetchType() pb.SourceType {
	return pb.SourceType_SOURCE_TYPE_MAVEN
}
func (h *MavenIngestionHandler) ExtractCanonicalPath(metadata map[string]string) string {
	if repoURL := metadata["repository_url"]; repoURL != "" {
		// Normalize GitHub URL to path format
		return normalizeGitURL(repoURL)
	}
	// Fallback to Maven Central URL if no repository found
	if groupId := metadata["group_id"]; groupId != "" {
		if artifactId := metadata["artifact_id"]; artifactId != "" {
			// Convert groupId dots to slashes: com.google.guava -> com/google/guava
			groupPath := strings.ReplaceAll(groupId, ".", "/")
			version := metadata["version"]
			if version != "" {
				return fmt.Sprintf("repo.maven.apache.org/%s/%s@%s", groupPath, artifactId, version)
			}
			return "repo.maven.apache.org/" + groupPath + "/" + artifactId
		}
	}
	return ""
}

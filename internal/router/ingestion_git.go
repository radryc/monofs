package router

import (
	"fmt"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage"
)

// GitIngestionHandler handles Git repository ingestion.
type GitIngestionHandler struct{}

func (h *GitIngestionHandler) Type() pb.IngestionType {
	return pb.IngestionType_INGESTION_GIT
}

func (h *GitIngestionHandler) NormalizeDisplayPath(source string, sourceID string) string {
	if sourceID != "" {
		return sourceID
	}
	return normalizeGitURL(source)
}

func (h *GitIngestionHandler) AddDirectoryPrefix(displayPath string, source string) string {
	return displayPath // No prefix for Git repos
}

func (h *GitIngestionHandler) GetDefaultRef() string {
	return "main"
}

func (h *GitIngestionHandler) ValidateSource(source string, ref string) error {
	if source == "" {
		return fmt.Errorf("source URL is required")
	}
	return nil
}

func (h *GitIngestionHandler) GetStorageType() storage.IngestionType {
	return storage.IngestionTypeGit
}

func (h *GitIngestionHandler) GetFetchType() pb.SourceType {
	return pb.SourceType_SOURCE_TYPE_GIT
}
func (h *GitIngestionHandler) ExtractCanonicalPath(metadata map[string]string) string {
	return "" // Git repos are already at their canonical path
}

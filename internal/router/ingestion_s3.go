package router

import (
	"fmt"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage"
)

// S3IngestionHandler handles S3 bucket ingestion.
type S3IngestionHandler struct{}

func (h *S3IngestionHandler) Type() pb.IngestionType {
	return pb.IngestionType_INGESTION_S3
}

func (h *S3IngestionHandler) NormalizeDisplayPath(source string, sourceID string) string {
	if sourceID != "" {
		return sourceID
	}
	return source
}

func (h *S3IngestionHandler) AddDirectoryPrefix(displayPath string, source string) string {
	return displayPath // No prefix for S3
}

func (h *S3IngestionHandler) GetDefaultRef() string {
	return "" // Optional prefix
}

func (h *S3IngestionHandler) ValidateSource(source string, ref string) error {
	if source == "" {
		return fmt.Errorf("S3 bucket name is required")
	}
	return nil
}

func (h *S3IngestionHandler) GetStorageType() storage.IngestionType {
	return storage.IngestionTypeS3
}

func (h *S3IngestionHandler) GetFetchType() storage.FetchType {
	return storage.FetchTypeS3
}
func (h *S3IngestionHandler) ExtractCanonicalPath(metadata map[string]string) string {
	return "" // S3 buckets use bucket name as canonical path
}

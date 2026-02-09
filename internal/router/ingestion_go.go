package router

import (
	"fmt"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage"
)

// GoModIngestionHandler handles Go module ingestion.
type GoModIngestionHandler struct{}

func (h *GoModIngestionHandler) Type() pb.IngestionType {
	return pb.IngestionType_INGESTION_GO
}

func (h *GoModIngestionHandler) NormalizeDisplayPath(source string, sourceID string) string {
	if sourceID != "" {
		return sourceID
	}
	return source // Go modules are already in module@version format
}

func (h *GoModIngestionHandler) AddDirectoryPrefix(displayPath string, source string) string {
	// Add go-modules/pkg/mod prefix to match GOMODCACHE layout
	// The prefix is added during ingestion by the handler.
	// The build layout mapper (golang/mapper.go) should NOT add this prefix again.
	return "go-modules/pkg/mod/" + displayPath
}

func (h *GoModIngestionHandler) GetDefaultRef() string {
	return "" // Version is required in source
}

func (h *GoModIngestionHandler) ValidateSource(source string, ref string) error {
	if !strings.Contains(source, "@") {
		return fmt.Errorf("go module source must include version (e.g., module@v1.0.0)")
	}
	return nil
}

func (h *GoModIngestionHandler) GetStorageType() storage.IngestionType {
	return storage.IngestionTypeGo
}

func (h *GoModIngestionHandler) GetFetchType() pb.SourceType {
	return pb.SourceType_SOURCE_TYPE_GOMOD
}
func (h *GoModIngestionHandler) ExtractCanonicalPath(metadata map[string]string) string {
	return "" // Go modules use module path as canonical path
}

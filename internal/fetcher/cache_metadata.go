package fetcher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
)

// generateCacheMetadata generates cache metadata content on-the-fly for Go modules.
// This is called when a request contains cache_type metadata, indicating the content
// should be generated rather than fetched from a backend.
func (s *Service) generateCacheMetadata(ctx context.Context, req *pb.FetchBlobRequest, fetchReq *FetchRequest) (io.ReadCloser, int64, error) {
	cacheType := req.SourceConfig["cache_type"]

	switch cacheType {
	case "go-module":
		return s.generateGoModuleCacheMetadata(ctx, req, fetchReq)
	default:
		return nil, 0, fmt.Errorf("unsupported cache_type: %s", cacheType)
	}
}

// generateGoModuleCacheMetadata generates Go module cache metadata files (.info, .mod, list).
func (s *Service) generateGoModuleCacheMetadata(ctx context.Context, req *pb.FetchBlobRequest, fetchReq *FetchRequest) (io.ReadCloser, int64, error) {
	modulePath := req.SourceConfig["module_path"]
	version := req.SourceConfig["version"]
	commitTime := req.SourceConfig["commit_time"]
	gomodHash := req.SourceConfig["gomod_hash"]
	filePath := req.SourceConfig["file_path"]

	// Determine which file to generate based on file_path from metadata
	if filePath == "" {
		return nil, 0, fmt.Errorf("file_path not specified in cache metadata")
	}

	var content []byte
	var err error

	if strings.HasSuffix(filePath, ".info") {
		// Generate version.info: {"Version":"v1.6.0","Time":"2024-01-01T00:00:00Z"}
		content = []byte(fmt.Sprintf(`{"Version":"%s","Time":"%s"}`, version, commitTime))

	} else if strings.HasSuffix(filePath, ".mod") {
		// Generate version.mod: copy of go.mod or minimal version
		if gomodHash != "" {
			// Fetch the actual go.mod from the backend
			content, err = s.fetchGoMod(ctx, req, gomodHash)
			if err != nil {
				// Fallback to minimal go.mod
				s.logger.Warn("failed to fetch go.mod, using minimal version",
					"module_path", modulePath,
					"error", err)
				content = []byte(fmt.Sprintf("module %s\n\ngo 1.24\n", modulePath))
			}
		} else {
			// Generate minimal go.mod
			content = []byte(fmt.Sprintf("module %s\n\ngo 1.24\n", modulePath))
		}

	} else if filePath == "list" {
		// Generate list file: just the version
		content = []byte(version + "\n")

	} else {
		return nil, 0, fmt.Errorf("unknown cache metadata file: %s", filePath)
	}

	reader := io.NopCloser(bytes.NewReader(content))
	size := int64(len(content))

	s.logger.Debug("generated cache metadata",
		"file_path", filePath,
		"module_path", modulePath,
		"version", version,
		"size", size,
	)

	return reader, size, nil
}

// fetchGoMod fetches the actual go.mod file content using its blob hash.
// This is used when generating version.mod cache metadata.
func (s *Service) fetchGoMod(ctx context.Context, req *pb.FetchBlobRequest, gomodHash string) ([]byte, error) {
	// Create a fetch request for the go.mod file
	gomodReq := &FetchRequest{
		ContentID:    gomodHash,
		SourceKey:    getSourceKey(req),
		SourceConfig: req.SourceConfig,
		RequestID:    req.RequestId + "-gomod",
		Priority:     int(req.Priority),
	}

	backend, ok := s.registry.Get(req.SourceType)
	if !ok {
		return nil, fmt.Errorf("backend not found for source type: %s", req.SourceType)
	}

	reader, _, err := backend.FetchBlobStream(ctx, gomodReq)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch go.mod: %w", err)
	}
	defer reader.Close()

	// Read entire content
	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read go.mod: %w", err)
	}

	return content, nil
}

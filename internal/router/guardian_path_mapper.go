package router

import (
	"fmt"
	"path"
	"strings"

	"github.com/radryc/monofs/internal/sharding"
)

type guardianPhysicalPath struct {
	LogicalPath  string
	DisplayPath  string
	RelativePath string
	StorageID    string
}

func normalizeGuardianLogicalPath(input string) (string, error) {
	cleaned := strings.TrimSpace(input)
	if cleaned == "" {
		return "", fmt.Errorf("logical_path is required")
	}
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	cleaned = path.Clean(cleaned)
	if cleaned == "." || cleaned == "/" {
		return "", fmt.Errorf("logical_path must target a Guardian path")
	}
	if strings.Contains(cleaned, "..") {
		return "", fmt.Errorf("logical_path %q must not contain '..'", input)
	}
	return cleaned, nil
}

func mapGuardianLogicalPath(logicalPath string) (guardianPhysicalPath, error) {
	cleaned, err := normalizeGuardianLogicalPath(logicalPath)
	if err != nil {
		return guardianPhysicalPath{}, err
	}

	trimmed := strings.TrimPrefix(cleaned, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 {
		return guardianPhysicalPath{}, fmt.Errorf("invalid logical_path %q", logicalPath)
	}

	switch parts[0] {
	case "partitions":
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			return guardianPhysicalPath{}, fmt.Errorf("partition logical_path %q is missing partition name", logicalPath)
		}
		displayPath := "guardian/" + parts[1]
		relativePath := cleanGuardianRelativePath(strings.Join(parts[2:], "/"))
		return guardianPhysicalPath{
			LogicalPath:  cleaned,
			DisplayPath:  displayPath,
			RelativePath: relativePath,
			StorageID:    sharding.GenerateStorageID(displayPath),
		}, nil
	case ".queues", ".archive":
		displayPath := "guardian-system"
		relativePath := cleanGuardianRelativePath(trimmed)
		return guardianPhysicalPath{
			LogicalPath:  cleaned,
			DisplayPath:  displayPath,
			RelativePath: relativePath,
			StorageID:    sharding.GenerateStorageID(displayPath),
		}, nil
	default:
		return guardianPhysicalPath{}, fmt.Errorf("logical_path %q is outside the Guardian namespace", logicalPath)
	}
}

func guardianLogicalPathFromPhysical(displayPath, relativePath string) (string, error) {
	displayPath = strings.TrimSpace(displayPath)
	relativePath = cleanGuardianRelativePath(relativePath)

	switch {
	case displayPath == "guardian-system":
		if relativePath == "" {
			return "", fmt.Errorf("guardian-system root cannot be converted without a relative path")
		}
		if strings.HasPrefix(relativePath, ".queues/") || strings.HasPrefix(relativePath, ".archive/") {
			return "/" + relativePath, nil
		}
	case strings.HasPrefix(displayPath, "guardian/"):
		partition := strings.TrimPrefix(displayPath, "guardian/")
		if partition == "" {
			return "", fmt.Errorf("invalid display_path %q", displayPath)
		}
		base := "/partitions/" + partition
		if relativePath == "" {
			return base, nil
		}
		return base + "/" + relativePath, nil
	}

	return "", fmt.Errorf("display_path %q is not a Guardian path", displayPath)
}

func guardianDisplayPathJoin(displayPath, relativePath string) string {
	if relativePath == "" {
		return displayPath
	}
	return displayPath + "/" + relativePath
}

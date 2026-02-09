package maven

import (
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/radryc/monofs/internal/buildlayout"
)

const (
	// MavenRepositoryPrefix is the virtual mount prefix for Maven artifacts
	MavenRepositoryPrefix = ".m2/repository"
)

// MavenMapper implements LayoutMapper for Maven artifacts.
type MavenMapper struct{}

// NewMavenMapper creates a new Maven layout mapper.
func NewMavenMapper() *MavenMapper {
	return &MavenMapper{}
}

func (m *MavenMapper) Type() string { return "maven" }

// Matches returns true for Maven artifact ingestions.
func (m *MavenMapper) Matches(info buildlayout.RepoInfo) bool {
	return info.IngestionType == "maven"
}

// MapPaths creates virtual hard link entries for Maven artifacts.
//
// For "junit:junit:4.13.2" from "github.com/junit-team/junit4":
// 1. Files stored at canonical path: github.com/junit-team/junit4/ (primary storage)
// 2. Hard link created: .m2/repository/junit/junit/4.13.2/ -> github.com/junit-team/junit4/ (same storage_id)
//
// Output includes entries for BOTH paths, sharing the same storage_id.
func (m *MavenMapper) MapPaths(info buildlayout.RepoInfo, files []buildlayout.FileInfo) ([]buildlayout.VirtualEntry, error) {
	if len(files) == 0 {
		return nil, nil
	}

	// Check for pom.xml to confirm this is a Maven artifact
	hasPom := false
	for _, f := range files {
		if f.Path == "pom.xml" || strings.HasSuffix(f.Path, ".pom") {
			hasPom = true
			break
		}
	}

	if !hasPom {
		return nil, nil // Not a Maven artifact
	}

	// Parse Maven coordinates
	coords := parseMavenCoordinates(info)
	// Get additional metadata from files if available
	if coords.GroupId == "" && len(files) > 0 && files[0].BackendMetadata != nil {
		coords.GroupId = files[0].BackendMetadata["group_id"]
	}
	if coords.ArtifactId == "" && len(files) > 0 && files[0].BackendMetadata != nil {
		coords.ArtifactId = files[0].BackendMetadata["artifact_id"]
	}
	if coords.Version == "" && len(files) > 0 && files[0].BackendMetadata != nil {
		coords.Version = files[0].BackendMetadata["version"]
	}

	if coords.GroupId == "" || coords.ArtifactId == "" || coords.Version == "" {
		return nil, fmt.Errorf("invalid Maven coordinates")
	}

	// Extract canonical GitHub repository path from metadata
	canonicalPath := ""
	if len(files) > 0 && files[0].BackendMetadata != nil {
		canonicalPath = normalizeGitURL(files[0].BackendMetadata["repository_url"])
	}
	if canonicalPath == "" {
		// Fallback: use Maven Central path if no repository found
		groupPath := strings.ReplaceAll(coords.GroupId, ".", "/")
		canonicalPath = "repo1.maven.org/" + groupPath + "/" + coords.ArtifactId + "@" + coords.Version
	}
	_ = canonicalPath // TODO: use canonical path for virtual entry deduplication

	// Initial ingestion already created files at the canonical GitHub path.
	// Now create virtual hard link entries at .m2/repository/ for Maven compatibility.
	// This matches the npm/Go module pattern: primary at canonical path, virtual link at tool-specific path.

	var entries []buildlayout.VirtualEntry

	// Create entries for .m2/repository/ path (virtual hard link to canonical GitHub path)
	groupPath := strings.ReplaceAll(coords.GroupId, ".", "/")
	mavenPath := fmt.Sprintf("%s/%s/%s/%s",
		MavenRepositoryPrefix, groupPath, coords.ArtifactId, coords.Version)
	for _, f := range files {
		entries = append(entries, buildlayout.VirtualEntry{
			VirtualDisplayPath: mavenPath,
			VirtualFilePath:    f.Path,
			OriginalFilePath:   f.Path,
		})
	}

	return entries, nil
}

// MavenCoordinates represents Maven artifact coordinates
type MavenCoordinates struct {
	GroupId    string
	ArtifactId string
	Version    string
}

// parseMavenCoordinates extracts Maven coordinates from RepoInfo
func parseMavenCoordinates(info buildlayout.RepoInfo) MavenCoordinates {
	// Try to parse from displayPath: "com.google.guava:guava:31.0-jre"
	displayPath := strings.TrimPrefix(info.DisplayPath, "maven:")
	parts := strings.Split(displayPath, ":")

	coords := MavenCoordinates{}

	if len(parts) >= 3 {
		coords.GroupId = parts[0]
		coords.ArtifactId = parts[1]
		coords.Version = parts[2]
	} else if len(parts) == 2 {
		coords.GroupId = parts[0]
		coords.ArtifactId = parts[1]
		coords.Version = info.Ref
	}

	// Try to get from config if not in displayPath
	if coords.GroupId == "" {
		coords.GroupId = info.Config["group_id"]
	}
	if coords.ArtifactId == "" {
		coords.ArtifactId = info.Config["artifact_id"]
	}
	if coords.Version == "" {
		coords.Version = info.Config["version"]
	}
	if coords.Version == "" {
		coords.Version = info.Ref
	}

	return coords
}

// POM represents Maven pom.xml metadata
type POM struct {
	XMLName    xml.Name `xml:"project"`
	GroupId    string   `xml:"groupId"`
	ArtifactId string   `xml:"artifactId"`
	Version    string   `xml:"version"`
	SCM        struct {
		URL        string `xml:"url"`
		Connection string `xml:"connection"`
	} `xml:"scm"`
}

// normalizeGitURL converts a Git URL to canonical path format
func normalizeGitURL(url string) string {
	if url == "" {
		return ""
	}

	// Remove git+ prefix and .git suffix
	url = strings.TrimPrefix(url, "git+")
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimSuffix(url, "/")

	// Remove scheme
	for _, prefix := range []string{"https://", "http://", "git://", "ssh://"} {
		url = strings.TrimPrefix(url, prefix)
	}

	// Handle git@github.com:user/repo format
	if strings.HasPrefix(url, "git@") {
		url = strings.TrimPrefix(url, "git@")
		url = strings.Replace(url, ":", "/", 1)
	}

	// Validate it looks like a proper path
	if strings.Contains(url, "/") && !strings.Contains(url, " ") {
		return url
	}

	return ""
}

// pomXML represents a minimal pom.xml structure for dependency parsing
type pomXML struct {
	XMLName      xml.Name        `xml:"project"`
	Dependencies []pomDependency `xml:"dependencies>dependency"`
}

type pomDependency struct {
	GroupId    string `xml:"groupId"`
	ArtifactId string `xml:"artifactId"`
	Version    string `xml:"version"`
}

// ParseDependencyFile parses pom.xml and returns all dependencies.
func (m *MavenMapper) ParseDependencyFile(content []byte) ([]buildlayout.Dependency, error) {
	var pom pomXML
	if err := xml.Unmarshal(content, &pom); err != nil {
		return nil, fmt.Errorf("parse pom.xml: %w", err)
	}

	deps := make([]buildlayout.Dependency, 0, len(pom.Dependencies))
	for _, dep := range pom.Dependencies {
		if dep.GroupId == "" || dep.ArtifactId == "" {
			continue
		}

		version := dep.Version
		if version == "" {
			version = "LATEST"
		}

		deps = append(deps, buildlayout.Dependency{
			Module:  fmt.Sprintf("%s:%s", dep.GroupId, dep.ArtifactId),
			Version: version,
			Source:  fmt.Sprintf("%s:%s:%s", dep.GroupId, dep.ArtifactId, version),
		})
	}

	return deps, nil
}

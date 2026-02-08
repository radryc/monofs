package maven

import (
	"strings"
	"testing"

	"github.com/radryc/monofs/internal/buildlayout"
)

func TestMavenMapper_Type(t *testing.T) {
	mapper := NewMavenMapper()
	if got := mapper.Type(); got != "maven" {
		t.Errorf("Type() = %v, want %v", got, "maven")
	}
}

func TestMavenMapper_Matches(t *testing.T) {
	mapper := NewMavenMapper()

	tests := []struct {
		name        string
		info        buildlayout.RepoInfo
		wantMatches bool
	}{
		{
			name: "matches maven ingestion type",
			info: buildlayout.RepoInfo{
				IngestionType: "maven",
			},
			wantMatches: true,
		},
		{
			name: "does not match npm ingestion type",
			info: buildlayout.RepoInfo{
				IngestionType: "npm",
			},
			wantMatches: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapper.Matches(tt.info); got != tt.wantMatches {
				t.Errorf("Matches() = %v, want %v", got, tt.wantMatches)
			}
		})
	}
}

func TestMavenMapper_MapPaths_RealJUnitStructure(t *testing.T) {
	mapper := NewMavenMapper()

	// Simulate real junit:junit:4.13.2 artifact structure
	info := buildlayout.RepoInfo{
		DisplayPath:   "junit:junit:4.13.2",
		StorageID:     "test-storage-id",
		Source:        "junit:junit:4.13.2",
		Ref:           "4.13.2",
		IngestionType: "maven",
		FetchType:     "maven",
	}

	files := []buildlayout.FileInfo{
		{Path: "junit-4.13.2.jar", BlobHash: "hash1", Size: 384581, Mode: 0644},
		{Path: "junit-4.13.2.pom", BlobHash: "hash2", Size: 24445, Mode: 0644},
		{Path: "junit-4.13.2-sources.jar", BlobHash: "hash3", Size: 207421, Mode: 0644},
		{Path: "junit-4.13.2-javadoc.jar", BlobHash: "hash4", Size: 581234, Mode: 0644},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths() error = %v", err)
	}

	// With hard links, we get 2x entries: canonical path + .m2/ path
	expectedEntries := len(files) * 2
	if len(entries) != expectedEntries {
		t.Errorf("MapPaths() returned %d entries, want %d (2x for hard links)", len(entries), expectedEntries)
	}

	// Verify we have both canonical and .m2 paths
	hasCanonical := false
	hasM2 := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.VirtualDisplayPath, "repo1.maven.org/") {
			hasCanonical = true
		}
		if entry.VirtualDisplayPath == ".m2/repository/junit/junit/4.13.2" {
			hasM2 = true
		}
	}
	if !hasCanonical {
		t.Error("Missing canonical path entries (repo1.maven.org/)")
	}
	if !hasM2 {
		t.Error("Missing .m2/repository/ hard link entries")
	}
}

func TestMavenMapper_MapPaths_SpringBootStarter(t *testing.T) {
	mapper := NewMavenMapper()

	// Simulate real org.springframework.boot:spring-boot-starter-web:3.0.0
	info := buildlayout.RepoInfo{
		DisplayPath:   "org.springframework.boot:spring-boot-starter-web:3.0.0",
		StorageID:     "test-storage-id",
		Source:        "org.springframework.boot:spring-boot-starter-web:3.0.0",
		Ref:           "3.0.0",
		IngestionType: "maven",
		FetchType:     "maven",
	}

	files := []buildlayout.FileInfo{
		{Path: "spring-boot-starter-web-3.0.0.jar", BlobHash: "hash1", Size: 5000, Mode: 0644},
		{Path: "spring-boot-starter-web-3.0.0.pom", BlobHash: "hash2", Size: 3500, Mode: 0644},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths() error = %v", err)
	}

	// With hard links, we get 2x entries: canonical path + .m2/ path
	expectedEntries := len(files) * 2
	if len(entries) != expectedEntries {
		t.Errorf("MapPaths() returned %d entries, want %d (2x for hard links)", len(entries), expectedEntries)
	}

	// Verify complex groupId path conversion: org.springframework.boot → org/springframework/boot
	hasCanonical := false
	hasM2 := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.VirtualDisplayPath, "repo1.maven.org/") {
			hasCanonical = true
		}
		if entry.VirtualDisplayPath == ".m2/repository/org/springframework/boot/spring-boot-starter-web/3.0.0" {
			hasM2 = true
		}
	}
	if !hasCanonical {
		t.Error("Missing canonical path entries (repo1.maven.org/)")
	}
	if !hasM2 {
		t.Error("Missing .m2/repository/ hard link entries with correct path conversion")
	}
}

func TestMavenMapper_MapPaths_NoPom(t *testing.T) {
	mapper := NewMavenMapper()

	info := buildlayout.RepoInfo{
		DisplayPath:   "com.example:test:1.0.0",
		IngestionType: "maven",
	}

	// Files without pom.xml or .pom file
	files := []buildlayout.FileInfo{
		{Path: "README.md", BlobHash: "hash1", Size: 100, Mode: 0644},
		{Path: "test.jar", BlobHash: "hash2", Size: 1000, Mode: 0644},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths() error = %v", err)
	}

	// Should return nil when no pom is found
	if entries != nil {
		t.Errorf("MapPaths() returned entries for non-Maven artifact, want nil")
	}
}

func TestMavenMapper_ParseDependencyFile_RealPom(t *testing.T) {
	mapper := NewMavenMapper()

	// Real pom.xml from a Spring Boot project
	content := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0"
         xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
         xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 
         http://maven.apache.org/xsd/maven-4.0.0.xsd">
    <modelVersion>4.0.0</modelVersion>

    <groupId>com.example</groupId>
    <artifactId>demo-app</artifactId>
    <version>1.0.0-SNAPSHOT</version>
    <packaging>jar</packaging>

    <dependencies>
        <dependency>
            <groupId>org.springframework.boot</groupId>
            <artifactId>spring-boot-starter-web</artifactId>
            <version>3.0.0</version>
        </dependency>
        <dependency>
            <groupId>org.springframework.boot</groupId>
            <artifactId>spring-boot-starter-data-jpa</artifactId>
            <version>3.0.0</version>
        </dependency>
        <dependency>
            <groupId>com.h2database</groupId>
            <artifactId>h2</artifactId>
            <version>2.1.214</version>
            <scope>runtime</scope>
        </dependency>
        <dependency>
            <groupId>org.projectlombok</groupId>
            <artifactId>lombok</artifactId>
            <version>1.18.26</version>
            <scope>provided</scope>
        </dependency>
        <dependency>
            <groupId>org.springframework.boot</groupId>
            <artifactId>spring-boot-starter-test</artifactId>
            <version>3.0.0</version>
            <scope>test</scope>
        </dependency>
    </dependencies>
</project>`)

	deps, err := mapper.ParseDependencyFile(content)
	if err != nil {
		t.Fatalf("ParseDependencyFile() error = %v", err)
	}

	// Should parse all 5 dependencies
	expectedCount := 5
	if len(deps) != expectedCount {
		t.Errorf("ParseDependencyFile() returned %d dependencies, want %d", len(deps), expectedCount)
	}

	// Verify dependency format
	depMap := make(map[string]buildlayout.Dependency)
	for _, dep := range deps {
		depMap[dep.Module] = dep
	}

	// Check Spring Boot starter
	if dep, ok := depMap["org.springframework.boot:spring-boot-starter-web"]; ok {
		if dep.Version != "3.0.0" {
			t.Errorf("spring-boot-starter-web version = %v, want 3.0.0", dep.Version)
		}
		expectedSource := "org.springframework.boot:spring-boot-starter-web:3.0.0"
		if dep.Source != expectedSource {
			t.Errorf("spring-boot-starter-web source = %v, want %v", dep.Source, expectedSource)
		}
	} else {
		t.Error("spring-boot-starter-web dependency not found")
	}

	// Check H2 database
	if dep, ok := depMap["com.h2database:h2"]; ok {
		if dep.Version != "2.1.214" {
			t.Errorf("h2 version = %v, want 2.1.214", dep.Version)
		}
	} else {
		t.Error("h2 dependency not found")
	}
}

func TestMavenMapper_ParseDependencyFile_MissingVersions(t *testing.T) {
	mapper := NewMavenMapper()

	content := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<project>
    <dependencies>
        <dependency>
            <groupId>com.example</groupId>
            <artifactId>no-version</artifactId>
        </dependency>
        <dependency>
            <groupId>com.example</groupId>
            <artifactId>with-version</artifactId>
            <version>1.0.0</version>
        </dependency>
    </dependencies>
</project>`)

	deps, err := mapper.ParseDependencyFile(content)
	if err != nil {
		t.Fatalf("ParseDependencyFile() error = %v", err)
	}

	if len(deps) != 2 {
		t.Fatalf("ParseDependencyFile() returned %d dependencies, want 2", len(deps))
	}

	// Check that missing version defaults to LATEST
	for _, dep := range deps {
		if dep.Module == "com.example:no-version" {
			if dep.Version != "LATEST" {
				t.Errorf("no-version dependency version = %v, want LATEST", dep.Version)
			}
		}
	}
}

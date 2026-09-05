package pipeline

import (
	"testing"
)

func TestDetectAffectedPackages(t *testing.T) {
	meta := &PackageMeta{
		Packages: map[string]PackageInfo{
			"server": {
				Path: "cmd/monofs-server",
				Deps: []string{"internal/server", "internal/storage", "internal/sharding", "api/proto"},
			},
			"router": {
				Path: "cmd/monofs-router",
				Deps: []string{"internal/router", "internal/sharding", "api/proto"},
			},
			"fetcher": {
				Path: "cmd/monofs-fetcher",
				Deps: []string{"internal/fetcher", "api/proto"},
			},
			"pipeline-worker": {
				Path: "cmd/monofs-pipeline-worker",
				Deps: []string{"internal/router/pipeline", "internal/worker"},
			},
			"docs": {
				Path: "docs",
				Deps: []string{"docs"},
			},
		},
	}

	tests := []struct {
		name         string
		changedFiles []string
		wantAffected []string
	}{
		{
			name:         "server source change",
			changedFiles: []string{"internal/server/storage.go"},
			wantAffected: []string{"server"},
		},
		{
			name:         "proto change affects multiple",
			changedFiles: []string{"api/proto/monofs.proto"},
			wantAffected: []string{"server", "router", "fetcher"},
		},
		{
			name:         "shared dep change",
			changedFiles: []string{"internal/sharding/hrw.go"},
			wantAffected: []string{"server", "router"},
		},
		{
			name:         "direct cmd change",
			changedFiles: []string{"cmd/monofs-pipeline-worker/main.go"},
			wantAffected: []string{"pipeline-worker"},
		},
		{
			name:         "unrelated change",
			changedFiles: []string{"docs/architecture.md"},
			wantAffected: []string{"docs"},
		},
		{
			name:         "multiple files across packages",
			changedFiles: []string{"internal/server/handler.go", "internal/fetcher/blob.go"},
			wantAffected: []string{"server", "fetcher"},
		},
		{
			name:         "no changed files",
			changedFiles: []string{},
			wantAffected: nil,
		},
		{
			name:         "pipeline internal change",
			changedFiles: []string{"internal/router/pipeline/config.go"},
			wantAffected: []string{"router", "pipeline-worker"},
		},
		{
			name:         "worker internal change",
			changedFiles: []string{"internal/worker/handler.go"},
			wantAffected: []string{"pipeline-worker"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			affected := DetectAffectedPackages(meta, tt.changedFiles)
			if !stringSliceEqual(affected, tt.wantAffected) {
				t.Errorf("affected = %v, want %v", affected, tt.wantAffected)
			}
		})
	}
}

func TestIsPathAffected(t *testing.T) {
	tests := []struct {
		file   string
		prefix string
		want   bool
	}{
		{"cmd/monofs-server/main.go", "cmd/monofs-server", true},
		{"internal/server/storage.go", "internal/server", true},
		{"internal/server", "internal/server", true},
		{"internal/server/nested/deep/file.go", "internal/server", true},
		{"internal/storage/blob.go", "internal/server", false},
		{"cmd/monofs-server", "cmd/monofs-router", false},
		{"cmd/monofs", "cmd/monofs-server", false},
		{"api/proto/monofs.proto", "api/proto", true},
	}

	for _, tt := range tests {
		t.Run(tt.file+"_"+tt.prefix, func(t *testing.T) {
			if got := isPathAffected(tt.file, tt.prefix); got != tt.want {
				t.Errorf("isPathAffected(%q, %q) = %v, want %v", tt.file, tt.prefix, got, tt.want)
			}
		})
	}
}

func TestResolveAffectedBuildTargets(t *testing.T) {
	meta := &PackageMeta{
		Packages: map[string]PackageInfo{
			"server":          {Path: "cmd/monofs-server", Build: "make build-server"},
			"router":          {Path: "cmd/monofs-router", Build: "make build-router"},
			"pipeline-worker": {Path: "cmd/monofs-pipeline-worker", Build: "make build-pipeline-worker"},
			"docs":            {Path: "docs", Build: ""},
		},
	}

	affected := []string{"server", "pipeline-worker", "docs"}
	targets := ResolveAffectedBuildTargets(meta, affected)

	if len(targets) != 2 {
		t.Fatalf("expected 2 build targets, got %d: %v", len(targets), targets)
	}
	if targets[0] != "make build-server" {
		t.Errorf("target[0] = %q, want make build-server", targets[0])
	}
	if targets[1] != "make build-pipeline-worker" {
		t.Errorf("target[1] = %q, want make build-pipeline-worker", targets[1])
	}
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		if !set[s] {
			return false
		}
	}
	return true
}

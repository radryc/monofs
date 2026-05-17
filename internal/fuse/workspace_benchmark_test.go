package fuse

import (
	"context"
	"fmt"
	"testing"

	monoclient "github.com/radryc/monofs/internal/client"
)

func BenchmarkWorkspaceManifestResolvePath(b *testing.B) {
	for _, repoCount := range []int{64, 256, 1024} {
		b.Run(fmt.Sprintf("%drepos", repoCount), func(b *testing.B) {
			manifest := NewWorkspaceManifest(&mockClient{workspaceRepos: benchmarkManifestRepositories(repoCount)})
			ctx := context.Background()
			if _, err := manifest.List(ctx); err != nil {
				b.Fatalf("List() error = %v", err)
			}

			paths := make([]string, 0, repoCount)
			for repoIndex := 0; repoIndex < repoCount; repoIndex++ {
				paths = append(paths, fmt.Sprintf("github.com/acme/repo%04d/pkg/file.go", repoIndex))
			}

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				path := paths[i%len(paths)]
				resolution, err := manifest.ResolvePath(ctx, path)
				if err != nil {
					b.Fatalf("ResolvePath(%q) error = %v", path, err)
				}
				if resolution.Repository == nil {
					b.Fatalf("ResolvePath(%q) repository = nil", path)
				}
			}
		})
	}
}

func benchmarkManifestRepositories(repoCount int) []monoclient.WorkspaceRepository {
	repos := make([]monoclient.WorkspaceRepository, 0, repoCount)
	for repoIndex := 0; repoIndex < repoCount; repoIndex++ {
		repos = append(repos, monoclient.WorkspaceRepository{
			StorageID:   fmt.Sprintf("repo-%04d", repoIndex),
			DisplayPath: fmt.Sprintf("github.com/acme/repo%04d", repoIndex),
		})
	}
	return repos
}

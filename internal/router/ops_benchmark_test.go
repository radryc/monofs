package router

import (
	"fmt"
	"testing"
	"time"
)

var benchRepoURLs = []struct {
	name string
	url  string
}{
	{"https", "https://github.com/owner/repo"},
	{"https_git", "https://github.com/owner/repo.git"},
	{"bare", "github.com/owner/repo"},
	{"go_module", "github.com/google/uuid@v1.3.0"},
	{"long_path", "https://github.com/org/very-long-repository-name/with/deep/path"},
}

func BenchmarkNormalizeRepoID(b *testing.B) {
	for _, tc := range benchRepoURLs {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = normalizeRepoID(tc.url)
			}
		})
	}
}

func BenchmarkGuardianVersionCommit(b *testing.B) {
	store, err := newGuardianVersionStore("") // in-memory, no persist path
	if err != nil {
		b.Fatalf("newGuardianVersionStore: %v", err)
	}
	defer store.close()

	content := make([]byte, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = store.commit(guardianVersionCommit{
			LogicalPath:     fmt.Sprintf("/config/file-%d.yaml", i),
			DisplayPath:     fmt.Sprintf("config/file-%d.yaml", i),
			StorageID:       "storage-abc",
			BatchRevisionID: fmt.Sprintf("rev-%d", i),
			PrincipalID:     "agent-1",
			Content:         content,
			CommittedAt:     time.Now().UnixNano(),
		})
	}
}

func BenchmarkGuardianVersionRead(b *testing.B) {
	store, err := newGuardianVersionStore("")
	if err != nil {
		b.Fatalf("newGuardianVersionStore: %v", err)
	}
	defer store.close()

	// Pre-populate 100 paths
	for i := 0; i < 100; i++ {
		_, _ = store.commit(guardianVersionCommit{
			LogicalPath:     fmt.Sprintf("/config/file-%d.yaml", i),
			DisplayPath:     fmt.Sprintf("config/file-%d.yaml", i),
			StorageID:       "storage-abc",
			BatchRevisionID: fmt.Sprintf("rev-%d", i),
			PrincipalID:     "agent-1",
			CommittedAt:     time.Now().UnixNano(),
		})
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		path := fmt.Sprintf("/config/file-%d.yaml", i%100)
		store.mu.RLock()
		_ = store.records[path]
		store.mu.RUnlock()
	}
}

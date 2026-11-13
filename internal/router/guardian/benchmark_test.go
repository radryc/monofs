package guardian

import (
	"fmt"
	"testing"
	"time"
)

func BenchmarkGuardianVersionCommit(b *testing.B) {
	store, err := NewStore("") // in-memory, no persist path
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	content := make([]byte, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = store.Commit(Commit{
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
	store, err := NewStore("")
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	// Pre-populate 100 paths
	for i := 0; i < 100; i++ {
		_, _ = store.Commit(Commit{
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

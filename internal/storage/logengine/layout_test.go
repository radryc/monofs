package logengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/parquet/file"
)

func TestPartitionLayoutAndRowGroups(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{
		LocalCacheDir: filepath.Join(dir, "cache"),
		ChunkDuration: 5 * time.Minute,
	})
	engine.ingester.rowGroupRows = 2

	base := time.Date(2026, 11, 2, 12, 34, 56, 0, time.UTC)
	logs := make([]LogRecord, 5)
	for idx := range logs {
		logs[idx] = LogRecord{
			Timestamp:  base.Add(time.Duration(idx) * time.Second),
			Level:      "info",
			Service:    "guardian",
			RawMessage: "row",
			Labels:     map[string]string{"guardian.partition": "prod-eu"},
		}
	}
	chunkID := "chunk-layout-test"
	if err := engine.IngestLogs(ctx, chunkID, logs); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}

	paths, err := engine.store.ListChunkPaths(ctx, SignalLogs, base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil || len(paths) != 1 {
		t.Fatalf("ListChunkPaths() = %v, %v", paths, err)
	}
	wantPrefix := "partition=prod-eu/date=2026-11-02/hour=12/"
	if !strings.HasPrefix(paths[0], wantPrefix) {
		t.Fatalf("chunk path = %q, want prefix %q", paths[0], wantPrefix)
	}

	manifest, err := engine.store.ReadManifest(ctx, SignalLogs, paths[0])
	if err != nil {
		t.Fatalf("ReadManifest() error = %v", err)
	}
	if len(manifest.Partitions) != 1 || manifest.Partitions[0] != "prod-eu" {
		t.Fatalf("manifest partitions = %v", manifest.Partitions)
	}
	if manifest.RowGroups != 3 {
		t.Fatalf("manifest row groups = %d, want 3", manifest.RowGroups)
	}
	if manifest.RecordCount != int64(len(logs)) {
		t.Fatalf("manifest record count = %d, want %d", manifest.RecordCount, len(logs))
	}

	local, err := engine.store.EnsureLocalParquet(ctx, chunkDataPath(SignalLogs, paths[0]))
	if err != nil {
		t.Fatalf("EnsureLocalParquet() error = %v", err)
	}
	readerFile, err := os.Open(local)
	if err != nil {
		t.Fatalf("open parquet: %v", err)
	}
	defer readerFile.Close()
	reader, err := file.NewParquetReader(readerFile)
	if err != nil {
		t.Fatalf("NewParquetReader() error = %v", err)
	}
	defer reader.Close()
	if got := reader.NumRowGroups(); got != 3 {
		t.Fatalf("parquet row groups = %d, want 3", got)
	}
}

func TestMixedPartitionsUseMixedLayout(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{LocalCacheDir: filepath.Join(dir, "cache")})
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestLogs(ctx, "chunk-mixed", []LogRecord{
		{Timestamp: base, Service: "guardian", RawMessage: "a", Labels: map[string]string{"guardian.partition": "prod-eu"}},
		{Timestamp: base.Add(time.Second), Service: "guardian", RawMessage: "b", Labels: map[string]string{"guardian.partition": "prod-us"}},
	}); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}
	paths, err := engine.store.ListChunkPaths(ctx, SignalLogs, base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil || len(paths) != 1 {
		t.Fatalf("ListChunkPaths() = %v, %v", paths, err)
	}
	if !strings.HasPrefix(paths[0], "partition=_mixed/") {
		t.Fatalf("chunk path = %q, want partition=_mixed", paths[0])
	}
}

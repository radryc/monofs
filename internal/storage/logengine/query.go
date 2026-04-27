package logengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/RoaringBitmap/roaring"
	"github.com/blugelabs/bluge"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
)

// QueryEngine handles parsing and executing LogQL queries against the storage.
type QueryEngine struct {
	store *CachedStore
}

// NewQueryEngine creates a new QueryEngine.
func NewQueryEngine(store *CachedStore) *QueryEngine {
	return &QueryEngine{
		store: store,
	}
}

// Query executes a LogQL query over the given time range.
func (q *QueryEngine) Query(ctx context.Context, queryStr string) ([]LogRecord, error) {
	// 1. Parse Query using Loki's logql syntax
	expr, err := syntax.ParseExpr(queryStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse LogQL: %w", err)
	}

	var textFilter string
	var serviceFilter string

	var extract func(e syntax.Expr)
	extract = func(e syntax.Expr) {
		switch node := e.(type) {
		case *syntax.PipelineExpr:
			extract(node.Left)
			for _, stage := range node.MultiStages {
				if lf, ok := stage.(*syntax.LineFilterExpr); ok {
					textFilter = lf.Match
				}
			}
		case *syntax.MatchersExpr:
			for _, m := range node.Matchers() {
				if m.Name == "service" {
					serviceFilter = m.Value
				}
			}
		}
	}
	extract(expr)

	// 2. Fetch all metadata manifests (Time Pruning)
	chunkIDs, err := q.store.ListChunks(ctx, "chunks/")
	if err != nil {
		return nil, fmt.Errorf("failed to list chunks: %w", err)
	}

	var results []LogRecord

	for _, chunkID := range chunkIDs {
		// Load Metadata
		manifestPath := filepath.Join("chunks", chunkID, "metadata.json")
		rc, err := q.store.remote.Read(ctx, manifestPath)
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				// Handle "Ghost Chunks" silently
				continue
			}
			return nil, err
		}

		var manifest ChunkManifest
		if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
			rc.Close()
			return nil, err
		}
		rc.Close()

		// (Time pruning would happen here comparing manifest.MinTime / MaxTime)

		// 3. Free-Text Phase (Bluge)
		var validRows *roaring.Bitmap

		if textFilter != "" {
			indexPath := filepath.Join("chunks", chunkID, "text.index.tar.gz")

			// Trigger local cache download if necessary
			localIndexPath, err := q.store.GetLocalIndexPath(ctx, indexPath)
			if err != nil {
				if errors.Is(err, ErrGhostChunk) {
					continue
				}
				return nil, err
			}

			// Query Bluge
			config := bluge.DefaultConfig(localIndexPath)
			reader, err := bluge.OpenReader(config)
			if err != nil {
				return nil, err
			}

			blugeQuery := bluge.NewMatchQuery(textFilter).SetField("raw_message")
			req := bluge.NewTopNSearch(10000, blugeQuery)

			documentMatchIterator, err := reader.Search(ctx, req)
			if err != nil {
				reader.Close()
				return nil, err
			}

			validRows = roaring.New()
			match, err := documentMatchIterator.Next()
			for err == nil && match != nil {
				validRows.Add(uint32(match.Number))
				match, err = documentMatchIterator.Next()
			}
			reader.Close()

			if validRows.IsEmpty() {
				// No matches in this chunk
				continue
			}
		}

		// 4. Structured Phase (Parquet)
		parquetPath := filepath.Join("chunks", chunkID, "data.parquet")

		chunkResults, err := q.readParquet(ctx, parquetPath, validRows, serviceFilter)
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return nil, err
		}

		results = append(results, chunkResults...)
	}

	return results, nil
}

func (q *QueryEngine) readParquet(ctx context.Context, path string, validRows *roaring.Bitmap, serviceFilter string) ([]LogRecord, error) {
	// In a complete implementation, this would:
	// 1. Issue Seek() commands (HTTP Range requests on S3)
	// 2. Fetch only Zstd-compressed blocks containing rows in validRows
	// 3. Decompress and apply stream selectors (e.g., serviceFilter)

	// Mock implementation
	rc, err := q.store.remote.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	// Simulate returning a log record
	mockLog := LogRecord{
		RawMessage: "connection timeout in payment service",
		Service:    "payment",
	}

	return []LogRecord{mockLog}, nil
}

package logengine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/schema"
	"github.com/blugelabs/bluge"
)

// Ingester handles the chunking and dual-write architecture.
type Ingester struct {
	store    ObjectStoreBackend
	chunkDur time.Duration
}

// NewIngester creates a new Ingester.
func NewIngester(store ObjectStoreBackend, chunkDur time.Duration) *Ingester {
	return &Ingester{
		store:    store,
		chunkDur: chunkDur,
	}
}

// FlushChunk writes a buffer of logs to the dual-write architecture.
func (i *Ingester) FlushChunk(ctx context.Context, chunkID string, logs []LogRecord) error {
	if len(logs) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 3)

	var minTime, maxTime time.Time
	minTime = logs[0].Timestamp
	maxTime = logs[0].Timestamp

	for _, l := range logs {
		if l.Timestamp.Before(minTime) {
			minTime = l.Timestamp
		}
		if l.Timestamp.After(maxTime) {
			maxTime = l.Timestamp
		}
	}

	manifest := ChunkManifest{
		ChunkID: chunkID,
		MinTime: minTime,
		MaxTime: maxTime,
		// TraceBloom: ... build bloom filter
	}

	chunkPrefix := filepath.Join("chunks", chunkID)

	// Stream A: Parquet Write
	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- i.writeParquet(ctx, filepath.Join(chunkPrefix, "data.parquet"), logs)
	}()

	// Stream B: Bluge Indexing
	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- i.writeBlugeIndex(ctx, filepath.Join(chunkPrefix, "text.index.tar.gz"), logs)
	}()

	// Stream C: Metadata
	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- i.writeMetadata(ctx, filepath.Join(chunkPrefix, "metadata.json"), manifest)
	}()

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			return err
		}
	}

	return nil
}

func (i *Ingester) writeParquet(ctx context.Context, path string, logs []LogRecord) error {
	fields := schema.FieldList{
		schema.NewInt64Node("timestamp", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("level", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("service", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("trace_id", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("raw_message", parquet.Repetitions.Required, -1),
	}
	parquetSchema, err := schema.NewGroupNode("log_record", parquet.Repetitions.Required, fields, -1)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	props := parquet.NewWriterProperties(
		parquet.WithCompression(compress.Codecs.Zstd),
		parquet.WithDictionaryDefault(true),
	)

	writer := file.NewParquetWriter(&buf, parquetSchema, file.WithWriterProps(props))
	defer writer.Close()

	rgw := writer.AppendRowGroup()

	// Timestamp
	cw, err := rgw.NextColumn()
	if err != nil {
		return err
	}
	tsWriter := cw.(*file.Int64ColumnChunkWriter)
	timestamps := make([]int64, len(logs))
	for idx, l := range logs {
		timestamps[idx] = l.Timestamp.UnixNano()
	}
	tsWriter.WriteBatch(timestamps, nil, nil)
	tsWriter.Close()

	// Level
	cw, err = rgw.NextColumn()
	if err != nil {
		return err
	}
	lvlWriter := cw.(*file.ByteArrayColumnChunkWriter)
	levels := make([]parquet.ByteArray, len(logs))
	for idx, l := range logs {
		levels[idx] = parquet.ByteArray(l.Level)
	}
	lvlWriter.WriteBatch(levels, nil, nil)
	lvlWriter.Close()

	// Service
	cw, err = rgw.NextColumn()
	if err != nil {
		return err
	}
	svcWriter := cw.(*file.ByteArrayColumnChunkWriter)
	services := make([]parquet.ByteArray, len(logs))
	for idx, l := range logs {
		services[idx] = parquet.ByteArray(l.Service)
	}
	svcWriter.WriteBatch(services, nil, nil)
	svcWriter.Close()

	// TraceID
	cw, err = rgw.NextColumn()
	if err != nil {
		return err
	}
	traceWriter := cw.(*file.ByteArrayColumnChunkWriter)
	traces := make([]parquet.ByteArray, len(logs))
	for idx, l := range logs {
		traces[idx] = parquet.ByteArray(l.TraceID)
	}
	traceWriter.WriteBatch(traces, nil, nil)
	traceWriter.Close()

	// RawMessage
	cw, err = rgw.NextColumn()
	if err != nil {
		return err
	}
	msgWriter := cw.(*file.ByteArrayColumnChunkWriter)
	messages := make([]parquet.ByteArray, len(logs))
	for idx, l := range logs {
		messages[idx] = parquet.ByteArray(l.RawMessage)
	}
	msgWriter.WriteBatch(messages, nil, nil)
	msgWriter.Close()

	if err := rgw.Close(); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	return i.store.Write(ctx, path, &buf)
}

func (i *Ingester) writeBlugeIndex(ctx context.Context, path string, logs []LogRecord) error {
	// Create a temporary directory for the Bluge index
	tmpDir, err := os.MkdirTemp("", "bluge-index-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	config := bluge.DefaultConfig(tmpDir)
	writer, err := bluge.OpenWriter(config)
	if err != nil {
		return err
	}

	for idx, log := range logs {
		doc := bluge.NewDocument(fmt.Sprintf("%d", idx)) // Use row ID as document ID
		// Index the raw message text
		doc.AddField(bluge.NewTextField("raw_message", log.RawMessage).StoreValue())

		if err := writer.Update(doc.ID(), doc); err != nil {
			writer.Close()
			return err
		}
	}

	if err := writer.Close(); err != nil {
		return err
	}

	// Tar and gzip the directory
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	err = filepath.Walk(tmpDir, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(fi, fi.Name())
		if err != nil {
			return err
		}

		relPath, _ := filepath.Rel(tmpDir, file)
		if relPath == "." {
			return nil
		}
		header.Name = relPath

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !fi.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = io.Copy(tw, f)
		return err
	})

	if err != nil {
		return err
	}

	if err := tw.Close(); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}

	return i.store.Write(ctx, path, &buf)
}

func (i *Ingester) writeMetadata(ctx context.Context, path string, manifest ChunkManifest) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(manifest); err != nil {
		return err
	}
	return i.store.Write(ctx, path, &buf)
}

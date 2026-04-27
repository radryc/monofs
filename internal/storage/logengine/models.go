package logengine

import "time"

// LogRecord represents a single structured log entry.
type LogRecord struct {
	Timestamp  time.Time
	Level      string
	Service    string
	TraceID    string
	RawMessage string
}

// ChunkManifest stores lightweight metadata for a single chunk.
type ChunkManifest struct {
	ChunkID    string    `json:"chunk_id"`
	MinTime    time.Time `json:"min_time"`
	MaxTime    time.Time `json:"max_time"`
	TraceBloom []byte    `json:"trace_bloom,omitempty"` // Serialized bloom filter for trace_id
}

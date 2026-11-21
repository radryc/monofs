// Package layout defines the content-addressed blob layout v2 used by the
// fetcher: deterministic shard/pack naming, a versioned per-shard manifest,
// the ingest batching policy, and the compaction planner.
//
// The layout is designed so that a read never lists an object store: the
// shard is derived from the digest, and the manifest lists a bounded number
// of packs that may contain it.
package layout

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// Version is the on-object layout version. It is part of every key so a
	// migration can run side by side with the legacy _loose/_batch layout.
	Version = "v2"

	// DigestPrefix is the algorithm prefix for blob digests.
	DigestPrefix = "sha256"

	// PackSuffix and IndexSuffix are appended to a pack ID.
	PackSuffix  = ".pack"
	IndexSuffix = ".idx"

	// ManifestName is the per-shard manifest object.
	ManifestName = "MANIFEST.json"

	// ShardCount is the number of digest shards (first two hex chars).
	ShardCount = 256
)

// ErrInvalidDigest is returned when a digest is not a valid sha256 digest.
var ErrInvalidDigest = errors.New("invalid digest")

// ErrInvalidShard is returned when a shard is not two lowercase hex chars.
var ErrInvalidShard = errors.New("invalid shard")

// NormalizeDigest accepts "sha256:<hex>" or bare "<hex>", case-insensitively,
// and returns the bare lowercase 64-character hex digest.
func NormalizeDigest(digest string) (string, error) {
	d := strings.TrimSpace(strings.ToLower(digest))
	d = strings.TrimPrefix(d, DigestPrefix+":")
	if len(d) != 64 {
		return "", fmt.Errorf("%w: expected 64 hex characters, got %d", ErrInvalidDigest, len(d))
	}
	for _, c := range d {
		if !isHex(byte(c)) {
			return "", fmt.Errorf("%w: non-hex character %q", ErrInvalidDigest, c)
		}
	}
	return d, nil
}

// Shard returns the two-character shard for a digest.
func Shard(digest string) (string, error) {
	d, err := NormalizeDigest(digest)
	if err != nil {
		return "", err
	}
	return d[:2], nil
}

// ValidateShard reports whether shard is exactly two lowercase hex characters.
func ValidateShard(shard string) error {
	if len(shard) != 2 {
		return fmt.Errorf("%w: %q", ErrInvalidShard, shard)
	}
	for i := 0; i < len(shard); i++ {
		c := shard[i]
		if !isHex(c) || (c >= 'A' && c <= 'F') {
			return fmt.Errorf("%w: %q", ErrInvalidShard, shard)
		}
	}
	return nil
}

// ShardPrefix returns the object prefix for a shard, including the trailing
// slash, e.g. "packs/v2/sha256/ab/".
func ShardPrefix(shard string) string {
	return fmt.Sprintf("packs/%s/%s/%s/", Version, DigestPrefix, shard)
}

// ManifestKey returns the manifest object key for a shard.
func ManifestKey(shard string) string {
	return ShardPrefix(shard) + ManifestName
}

// PackKey returns the pack object key for a pack.
func PackKey(shard, packID string) string {
	return ShardPrefix(shard) + packID + PackSuffix
}

// IndexKey returns the sidecar index object key for a pack. The index is a
// sibling of the pack (matching the legacy "<pack>.idx" convention), so the
// same key can be uploaded and deleted with the pack.
func IndexKey(shard, packID string) string {
	return ShardPrefix(shard) + packID + PackSuffix + IndexSuffix
}

// NewPackID returns a lexicographically time-ordered pack identifier.
func NewPackID(t time.Time, seq int) string {
	if seq < 0 {
		seq = 0
	}
	return fmt.Sprintf("%d-%04d", t.UTC().UnixNano(), seq)
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

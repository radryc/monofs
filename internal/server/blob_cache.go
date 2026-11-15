package server

import (
	"os"
	"strings"
	"time"
)

const (
	defaultBlobNegativeTTL = 30 * time.Second
	maxBlobNegativeEntries = 200_000
)

// blobNegativeTTL returns the TTL for negative blob lookups. Missing objects
// are usually permanent until the next ingest, but a short TTL lets a repaired
// blob become readable without restarting the node.
func (s *Server) blobNegativeTTL() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("MONOFS_BLOB_NEGATIVE_TTL")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return defaultBlobNegativeTTL
}

func (s *Server) blobIsNegative(hash string) bool {
	if hash == "" {
		return false
	}
	now := time.Now()
	s.blobNegMu.Lock()
	defer s.blobNegMu.Unlock()
	expires, ok := s.blobNeg[hash]
	if !ok {
		return false
	}
	if now.After(expires) {
		delete(s.blobNeg, hash)
		return false
	}
	return true
}

func (s *Server) markBlobNegative(hash string) {
	if hash == "" {
		return
	}
	now := time.Now()
	s.blobNegMu.Lock()
	defer s.blobNegMu.Unlock()
	if s.blobNeg == nil {
		s.blobNeg = make(map[string]time.Time)
	}
	if len(s.blobNeg) >= maxBlobNegativeEntries {
		for key, expires := range s.blobNeg {
			if now.After(expires) {
				delete(s.blobNeg, key)
			}
		}
		if len(s.blobNeg) >= maxBlobNegativeEntries {
			s.blobNeg = make(map[string]time.Time)
		}
	}
	s.blobNeg[hash] = now.Add(s.blobNegativeTTL())
}

// isBlobNotFoundError reports whether the fetcher explicitly said the content
// does not exist. Transient errors (timeouts, unavailable) must not be cached.
func isBlobNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "blob not found") ||
		(strings.Contains(msg, "not found") && strings.Contains(msg, "blob")) ||
		strings.Contains(msg, "no such object") ||
		strings.Contains(msg, "nosuchkey")
}

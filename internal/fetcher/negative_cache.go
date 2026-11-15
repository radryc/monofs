package fetcher

import (
	"os"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

const (
	defaultNegativeTTL = 30 * time.Second
	maxNegativeEntries = 200_000
)

func (s *Service) negativeTTL() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("MONOFS_FETCHER_NEGATIVE_TTL")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return defaultNegativeTTL
}

func negativeKey(req *pb.FetchBlobRequest) string {
	return getSourceKey(req) + "|" + req.ContentId
}

func (s *Service) negativeCached(req *pb.FetchBlobRequest) bool {
	if req == nil || req.ContentId == "" {
		return false
	}
	key := negativeKey(req)
	now := time.Now()
	s.negMu.Lock()
	defer s.negMu.Unlock()
	expires, ok := s.negCache[key]
	if !ok {
		return false
	}
	if now.After(expires) {
		delete(s.negCache, key)
		return false
	}
	return true
}

func (s *Service) markNegative(req *pb.FetchBlobRequest) {
	if req == nil || req.ContentId == "" {
		return
	}
	key := negativeKey(req)
	now := time.Now()
	s.negMu.Lock()
	defer s.negMu.Unlock()
	if s.negCache == nil {
		s.negCache = make(map[string]time.Time)
	}
	if len(s.negCache) >= maxNegativeEntries {
		for k, expires := range s.negCache {
			if now.After(expires) {
				delete(s.negCache, k)
			}
		}
		if len(s.negCache) >= maxNegativeEntries {
			s.negCache = make(map[string]time.Time)
		}
	}
	s.negCache[key] = now.Add(s.negativeTTL())
}

// isNegativeCacheableError only caches explicit not-found responses. Timeouts,
// cancellations and transient cloud errors must be retried normally.
func isNegativeCacheableError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "blob not found") ||
		(strings.Contains(msg, "not found") && strings.Contains(msg, "blob")) ||
		strings.Contains(msg, "no such object") ||
		strings.Contains(msg, "nosuchkey") ||
		strings.Contains(msg, "does not exist")
}

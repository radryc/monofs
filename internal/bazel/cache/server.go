package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Server implements the Bazel Remote Cache HTTP/1.1 protocol.
//
// Endpoints:
//
//	GET  /ac/<hash>/<size>   → 200 + ActionResult bytes  |  404
//	PUT  /ac/<hash>/<size>   → 200 (store ActionResult)
//	GET  /cas/<hash>/<size>  → 200 + blob bytes          |  404
//	PUT  /cas/<hash>/<size>  → 200 (store blob)
//	GET  /status             → {"ok":true,...}
//
// Bazel sends GET requests with the digest in the URL path.
// PUT requests have the body containing the data.
type Server struct {
	store  *Store
	logger *slog.Logger
	mux    *http.ServeMux

	// Metrics
	acHits         atomic.Int64
	acMisses       atomic.Int64
	acPuts         atomic.Int64
	casHits        atomic.Int64
	casMisses      atomic.Int64
	casPuts        atomic.Int64
	casBytesServed atomic.Int64
	casBytesStored atomic.Int64
	startTime      time.Time
}

// NewServer creates a cache HTTP server.
func NewServer(store *Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		store:     store,
		logger:    logger,
		mux:       http.NewServeMux(),
		startTime: time.Now(),
	}
	s.mux.HandleFunc("/ac/", s.handleAC)
	s.mux.HandleFunc("/cas/", s.handleCAS)
	s.mux.HandleFunc("/status", s.handleStatus)
	s.mux.HandleFunc("/", s.handleNotFound)
	return s
}

// Handler returns the HTTP handler for this server.
func (s *Server) Handler() http.Handler { return s.mux }

// handleAC serves Action Cache requests.
func (s *Server) handleAC(w http.ResponseWriter, r *http.Request) {
	digest := strings.TrimPrefix(r.URL.Path, "/ac/")
	if digest == "" {
		http.Error(w, "missing digest", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		data, err := s.store.GetAC(r.Context(), digest)
		if err == ErrNotFound {
			s.acMisses.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err != nil {
			s.logger.Error("ac get", "digest", digest, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		s.acHits.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write(data)

	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024)) // 10 MB max AC
		r.Body.Close()
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if err := s.store.PutAC(r.Context(), digest, body); err != nil {
			s.logger.Error("ac put", "digest", digest, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		s.acPuts.Add(1)
		w.WriteHeader(http.StatusOK)

	case http.MethodHead:
		exists, err := s.store.HasAC(r.Context(), digest)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if exists {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleCAS serves Content-Addressable Storage requests.
func (s *Server) handleCAS(w http.ResponseWriter, r *http.Request) {
	digest := strings.TrimPrefix(r.URL.Path, "/cas/")
	if digest == "" {
		http.Error(w, "missing digest", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		rc, size, err := s.store.GetCASStream(r.Context(), digest)
		if err == ErrNotFound {
			s.casMisses.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err != nil {
			s.logger.Error("cas get", "digest", digest, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		defer rc.Close()
		s.casHits.Add(1)
		s.casBytesServed.Add(size)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
		w.WriteHeader(http.StatusOK)
		io.Copy(w, rc)

	case http.MethodPut:
		data, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		// Bazel sends the full data in the PUT body.
		// We validate the digest matches.
		if err := s.store.PutCAS(r.Context(), digest, data); err != nil {
			if err == ErrDigestMismatch {
				s.logger.Warn("cas put digest mismatch", "digest", digest)
				http.Error(w, "digest mismatch", http.StatusBadRequest)
				return
			}
			if err == ErrBlobTooLarge {
				http.Error(w, "blob too large", http.StatusRequestEntityTooLarge)
				return
			}
			s.logger.Error("cas put", "digest", digest, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		s.casPuts.Add(1)
		s.casBytesStored.Add(int64(len(data)))
		w.WriteHeader(http.StatusOK)

	case http.MethodHead:
		exists, err := s.store.HasCAS(r.Context(), digest)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if exists {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleStatus returns server health and statistics.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	acEntries, acBytes, _ := s.store.ACStats(r.Context())
	casBlobs, casBytes, _ := s.store.CASStats(r.Context())

	status := map[string]interface{}{
		"ok":               true,
		"uptime_secs":      time.Since(s.startTime).Seconds(),
		"ac_entries":       acEntries,
		"ac_bytes":         acBytes,
		"cas_blobs":        casBlobs,
		"cas_bytes":        casBytes,
		"ac_hits":          s.acHits.Load(),
		"ac_misses":        s.acMisses.Load(),
		"ac_puts":          s.acPuts.Load(),
		"cas_hits":         s.casHits.Load(),
		"cas_misses":       s.casMisses.Load(),
		"cas_puts":         s.casPuts.Load(),
		"cas_bytes_served": s.casBytesServed.Load(),
		"cas_bytes_stored": s.casBytesStored.Load(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not found", http.StatusNotFound)
}

// --- Eviction ---

// EvictionConfig controls background CAS eviction.
type EvictionConfig struct {
	// MaxAge is how long a CAS blob can exist without being accessed.
	MaxAge time.Duration

	// Interval is how often the eviction job runs.
	Interval time.Duration

	// MaxBytes is a soft limit on total CAS bytes. When exceeded,
	// oldest blobs are evicted first.
	MaxBytes int64
}

// DefaultEvictionConfig returns sensible defaults.
func DefaultEvictionConfig() EvictionConfig {
	return EvictionConfig{
		MaxAge:   30 * 24 * time.Hour, // 30 days
		Interval: 6 * time.Hour,
		MaxBytes: 100 * 1024 * 1024 * 1024, // 100 GB
	}
}

// RunEviction starts a background goroutine that evicts old CAS blobs.
// It returns a function that stops the eviction loop.
func (s *Server) RunEviction(ctx context.Context, cfg EvictionConfig) func() {
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				s.evict(ctx, cfg)
			case <-ctx.Done():
				return
			}
		}
	}()

	return cancel
}

func (s *Server) evict(ctx context.Context, cfg EvictionConfig) {
	digests, err := s.store.ListCAS(ctx)
	if err != nil {
		s.logger.Error("eviction list", "error", err)
		return
	}

	cutoff := time.Now().Add(-cfg.MaxAge)
	var evicted int64

	for _, digest := range digests {
		age, err := s.store.CASBlobAge(ctx, digest)
		if err != nil {
			continue
		}
		if age.Before(cutoff) {
			if err := s.store.DeleteCAS(ctx, digest); err != nil {
				s.logger.Warn("eviction delete", "digest", digest, "error", err)
				continue
			}
			evicted++
		}
	}

	if evicted > 0 {
		s.logger.Info("eviction complete", "evicted", evicted)
	}
}

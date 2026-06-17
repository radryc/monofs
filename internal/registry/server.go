package registry

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/opencontainers/go-digest"
)

type Server struct {
	blobs    *BlobStore
	tags     *TagStore
	uploads  *UploadManager
	proxy    *Proxy
	stats    *Stats
	client   *Client
	logger   *slog.Logger
	dataNS   string

	blobDownloadCount atomic.Int64
	blobUploadCount   atomic.Int64
}

func NewServer(client *Client, nextProxy *Proxy, logger *slog.Logger, dataNS string) *Server {
	blobs := NewBlobStore(client)
	tags := NewTagStore(client, blobs)
	uploads := NewUploadManager()
	stats := &Stats{}

	if nextProxy == nil {
		nextProxy = NewProxy(UpstreamConfig{
			Default:  "https://registry-1.docker.io",
			Cooldown: 10 * time.Minute,
		}, blobs, tags, stats, logger)
	}

	s := &Server{
		blobs:   blobs,
		tags:    tags,
		uploads: uploads,
		proxy:   nextProxy,
		stats:   stats,
		client:  client,
		logger:  logger,
		dataNS:  dataNS,
	}
	go s.uploadCleanupLoop()
	return s
}

func (s *Server) uploadCleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		removed := s.uploads.Cleanup(30 * time.Minute)
		if removed > 0 {
			s.logger.Debug("cleaned up stale uploads", "count", removed)
		}
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/v2/", s.handleV2)
	mux.HandleFunc("/api/v1/stats", s.handleStats)
	mux.HandleFunc("/api/v1/repos", s.handleRepos)
	mux.HandleFunc("/health", s.handleHealth)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})

	return s.middleware(mux)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.logger.Debug("registry request", "method", r.Method, "path", r.URL.Path)
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleV2(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v2/")
	if path == "" || path == "/" {
		w.WriteHeader(http.StatusOK)
		return
	}

	parts := strings.SplitN(path, "/", 4)

	switch {
	case len(parts) == 1 && r.Method == "GET":
		repo := parts[0]
		if strings.HasSuffix(path, "/tags/list") {
			s.handleListTags(w, r, repo)
			return
		}

	case len(parts) >= 2 && parts[1] == "manifests":
		repo := parts[0]
		ref := strings.Join(parts[2:], "/")
		switch r.Method {
		case "GET", "HEAD":
			s.handleGetManifest(w, r, repo, ref)
		case "PUT":
			s.handlePutManifest(w, r, repo, ref)
		case "DELETE":
			s.handleDeleteManifest(w, r, repo, ref)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return

	case len(parts) >= 2 && parts[1] == "blobs":
		repo := parts[0]
		if len(parts) == 3 {
			dig := parts[2]
			switch r.Method {
			case "GET", "HEAD":
				s.handleGetBlob(w, r, repo, dig)
			case "DELETE":
				s.handleDeleteBlob(w, r, repo, dig)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}
		if len(parts) == 4 && parts[2] == "uploads" {
			uuid := parts[3]
			// Handle POST to /v2/<repo>/blobs/uploads/ (trailing slash gives empty parts[3])
			if uuid == "" && r.Method == "POST" {
				s.handleStartUpload(w, r, repo)
				return
			}
			switch r.Method {
			case "PATCH":
				s.handleUploadChunk(w, r, repo, uuid)
			case "PUT":
				s.handleUploadComplete(w, r, repo, uuid)
			case "DELETE":
				s.handleUploadCancel(w, r, repo, uuid)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

	case len(parts) >= 3 && parts[1] == "blobs" && parts[2] == "uploads":
		repo := parts[0]
		if r.Method == "POST" {
			s.handleStartUpload(w, r, repo)
			return
		}
	}

	http.Error(w, "not found", http.StatusNotFound)
}

func (s *Server) handleGetManifest(w http.ResponseWriter, r *http.Request, repo, ref string) {
	ctx := r.Context()

	data, dgst, err := s.tags.GetManifest(ctx, repo, ref)
	if err == nil {
		s.stats.Pulls.Add(1)
		s.stats.BytesServed.Add(int64(len(data)))
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.Header().Set("Docker-Content-Digest", dgst)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method == "HEAD" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		return
	}

	if s.proxy != nil {
		data, dgst, err := s.proxy.FetchManifest(ctx, repo, ref)
		if err == nil {
			s.stats.Pulls.Add(1)
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Header().Set("Docker-Content-Digest", dgst)
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			if r.Method == "HEAD" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}
		s.logger.Warn("manifest not found locally or upstream", "repo", repo, "ref", ref, "error", err)
	}

	writeOCIError(w, "MANIFEST_UNKNOWN", "manifest not found")
}

func (s *Server) handlePutManifest(w http.ResponseWriter, r *http.Request, repo, ref string) {
	ctx := r.Context()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeOCIError(w, "BLOB_UNKNOWN", "failed to read manifest body")
		return
	}
	defer r.Body.Close()

	dgst, err := s.tags.PutManifest(ctx, repo, ref, data)
	if err != nil {
		writeOCIError(w, "MANIFEST_INVALID", err.Error())
		return
	}

	s.stats.Pushes.Add(1)
	s.stats.BytesServed.Add(int64(len(data)))
	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/manifests/%s", repo, dgst))
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleDeleteManifest(w http.ResponseWriter, r *http.Request, repo, ref string) {
	ctx := r.Context()
	if err := s.tags.DeleteManifest(ctx, repo, ref); err != nil {
		writeOCIError(w, "MANIFEST_UNKNOWN", "manifest not found")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleGetBlob(w http.ResponseWriter, r *http.Request, repo, digestStr string) {
	ctx := r.Context()
	dgst := digestStr
	if !strings.HasPrefix(dgst, "sha256:") && strings.Contains(dgst, ":") {
		dgst = dgst
	}

	data, err := s.blobs.Get(ctx, dgst)
	if err == nil {
		s.stats.Pulls.Add(1)
		s.stats.BytesServed.Add(int64(len(data)))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", dgst)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method == "HEAD" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		return
	}

	if s.proxy != nil {
		data, err := s.proxy.FetchBlob(ctx, repo, dgst)
		if err == nil {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Docker-Content-Digest", dgst)
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			if r.Method == "HEAD" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}
		s.logger.Warn("blob not found locally or upstream", "repo", repo, "digest", dgst, "error", err)
	}

	writeOCIError(w, "BLOB_UNKNOWN", "blob unknown")
}

func (s *Server) handleDeleteBlob(w http.ResponseWriter, r *http.Request, repo, digestStr string) {
	ctx := r.Context()
	if err := s.blobs.Delete(ctx, digestStr); err != nil && !os.IsNotExist(err) {
		s.logger.Error("failed to delete blob", "digest", digestStr, "error", err)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleStartUpload(w http.ResponseWriter, r *http.Request, repo string) {
	session := s.uploads.Start(repo)
	w.Header().Set("Docker-Upload-UUID", session.ID)
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", repo, session.ID))
	w.Header().Set("Range", "0-0")
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleUploadChunk(w http.ResponseWriter, r *http.Request, repo, uuid string) {
	session, ok := s.uploads.Get(uuid)
	if !ok || session.Repo != repo {
		writeOCIError(w, "BLOB_UPLOAD_UNKNOWN", "upload session not found")
		return
	}

	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeOCIError(w, "BLOB_UPLOAD_INVALID", "failed to read chunk")
		return
	}
	defer r.Body.Close()

	updated, err := s.uploads.Append(uuid, data)
	if err != nil {
		writeOCIError(w, "BLOB_UPLOAD_INVALID", err.Error())
		return
	}

	rangeEnd := updated.Size - 1
	w.Header().Set("Docker-Upload-UUID", uuid)
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", repo, uuid))
	w.Header().Set("Range", fmt.Sprintf("0-%d", rangeEnd))
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleUploadComplete(w http.ResponseWriter, r *http.Request, repo, uuid string) {
	session, ok := s.uploads.Get(uuid)
	if !ok || session.Repo != repo {
		writeOCIError(w, "BLOB_UPLOAD_UNKNOWN", "upload session not found")
		return
	}

	dgstParam := r.URL.Query().Get("digest")
	if dgstParam == "" {
		writeOCIError(w, "DIGEST_INVALID", "digest parameter required")
		return
	}
	d := digest.Digest(dgstParam)
	if err := d.Validate(); err != nil {
		writeOCIError(w, "DIGEST_INVALID", err.Error())
		return
	}
	dgst := string(d)

	verifiedDgst := digestContent(session.Data)
	if dgst != verifiedDgst {
		s.uploads.Remove(uuid)
		writeOCIError(w, "DIGEST_INVALID", fmt.Sprintf("digest mismatch: expected %s, got %s", dgst, verifiedDgst))
		return
	}

	ctx := r.Context()
	if err := s.blobs.PutUnchecked(ctx, dgst, session.Data); err != nil {
		writeOCIError(w, "BLOB_UPLOAD_INVALID", err.Error())
		return
	}

	s.uploads.Remove(uuid)
	s.stats.Pushes.Add(1)
	s.stats.BytesServed.Add(int64(session.Size))
	s.stats.BlobCount.Add(1)

	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", repo, dgst))
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleUploadCancel(w http.ResponseWriter, r *http.Request, repo, uuid string) {
	s.uploads.Remove(uuid)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListTags(w http.ResponseWriter, r *http.Request, _ string) {
	path := strings.TrimPrefix(r.URL.Path, "/v2/")
	repo := strings.TrimSuffix(path, "/tags/list")

	ctx := r.Context()
	tags, err := s.tags.ListTags(ctx, repo)
	if err != nil {
		if s.proxy != nil {
			tags, err = s.proxy.FetchTags(ctx, repo)
			if err != nil {
				writeOCIError(w, "NAME_UNKNOWN", "repository not found")
				return
			}
			tagList := map[string]interface{}{
				"name": repo,
				"tags": tags,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tagList)
			return
		}
		writeOCIError(w, "NAME_UNKNOWN", "repository not found")
		return
	}

	tagList := map[string]interface{}{
		"name": repo,
		"tags": tags,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tagList)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	snap := s.stats.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snap)
}

func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repos, err := s.tags.ListRepos(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if repos == nil {
		repos = []string{}
	}
	result := map[string]interface{}{
		"repositories": repos,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func writeOCIError(w http.ResponseWriter, code, message string) {
	errResp := map[string]interface{}{
		"errors": []map[string]string{
			{
				"code":    code,
				"message": message,
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	var statusCode int
	switch code {
	case "BLOB_UNKNOWN", "MANIFEST_UNKNOWN":
		statusCode = http.StatusNotFound
	case "NAME_INVALID", "DIGEST_INVALID":
		statusCode = http.StatusBadRequest
	case "MANIFEST_INVALID", "BLOB_UPLOAD_INVALID":
		statusCode = http.StatusBadRequest
	case "BLOB_UPLOAD_UNKNOWN":
		statusCode = http.StatusNotFound
	case "UNAUTHORIZED":
		statusCode = http.StatusUnauthorized
	default:
		statusCode = http.StatusInternalServerError
	}
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(errResp)
}

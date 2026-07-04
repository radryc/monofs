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
	blobs   *BlobStore
	tags    *TagStore
	uploads *UploadManager
	proxy   *Proxy
	stats   *Stats
	client  *Client
	logger  *slog.Logger
	dataNS  string

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
	mux.HandleFunc("/api/v1/repos/", s.handleRepoDetail)
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

	// Split path into segments and find the action boundary.
	// Repo names can contain slashes (e.g. "library/alpine", "grafana/grafana").
	// We look for known action keywords: "manifests", "blobs", "tags".
	segs := strings.Split(path, "/")
	repoEnd := -1
	for i, s := range segs {
		if s == "manifests" || s == "blobs" || s == "tags" || s == "referrers" {
			repoEnd = i
			break
		}
	}
	if repoEnd < 0 {
		// Single-segment repo with no action (e.g. "/v2/guardian" → tags list probing)
		repo := segs[0]
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		_ = repo
		return
	}

	repo := strings.Join(segs[:repoEnd], "/")
	action := segs[repoEnd]
	rest := segs[repoEnd+1:]

	switch action {
	case "manifests":
		ref := strings.Join(rest, "/")
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

	case "blobs":
		if len(rest) == 1 {
			dig := rest[0]
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
		if len(rest) >= 2 && rest[0] == "uploads" {
			uuid := rest[1]
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
		if len(rest) == 1 && rest[0] == "uploads" && r.Method == "POST" {
			s.handleStartUpload(w, r, repo)
			return
		}

	case "tags":
		if len(rest) == 1 && rest[0] == "list" {
			s.handleListTags(w, r, repo)
			return
		}
	case "referrers":
		if len(rest) == 1 {
			s.handleReferrers(w, r, repo, rest[0])
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
		contentType := detectManifestContentType(data)
		w.Header().Set("Content-Type", contentType)
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
			contentType := detectManifestContentType(data)
			w.Header().Set("Content-Type", contentType)
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

	reader, err := s.blobs.GetReader(ctx, dgst)
	if err == nil {
		defer reader.Close()
		s.stats.Pulls.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", dgst)
		if r.Method == "HEAD" {
			blobSize, sizeErr := s.blobs.Size(ctx, dgst)
			if sizeErr == nil && blobSize > 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(blobSize, 10))
				w.WriteHeader(http.StatusOK)
				return
			}
			// Blob exists in index but size metadata is missing/corrupt.
			// Return 404 so the client re-uploads instead of failing on size mismatch.
			writeOCIError(w, "BLOB_UNKNOWN", "blob content unavailable")
			return
		}
		w.WriteHeader(http.StatusOK)
		written, copyErr := io.Copy(w, reader)
		s.stats.BytesServed.Add(written)
		if copyErr != nil {
			s.logger.Warn("blob copy error", "digest", dgst, "error", copyErr)
		}
		return
	}

	if s.proxy != nil {
		_, err := s.proxy.FetchBlob(ctx, repo, dgst)
		if err == nil {
			reader, readErr := s.blobs.GetReader(ctx, dgst)
			if readErr == nil {
				defer reader.Close()
				s.stats.Pulls.Add(1)
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Docker-Content-Digest", dgst)
				if r.Method == "HEAD" {
					blobSize, sizeErr := s.blobs.Size(ctx, dgst)
					if sizeErr == nil && blobSize > 0 {
						w.Header().Set("Content-Length", strconv.FormatInt(blobSize, 10))
					}
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(http.StatusOK)
				written, copyErr := io.Copy(w, reader)
				s.stats.BytesServed.Add(written)
				if copyErr != nil {
					s.logger.Warn("proxy blob copy error", "digest", dgst, "error", copyErr)
				}
				return
			}
			s.logger.Warn("blob fetched from upstream but local read failed", "digest", dgst, "error", readErr)
		}
		s.logger.Warn("blob not found locally or upstream", "repo", repo, "digest", dgst, "error", err)
	}

	writeOCIError(w, "BLOB_UNKNOWN", "blob unknown")
}

func (s *Server) handleDeleteBlob(w http.ResponseWriter, r *http.Request, repo, digestStr string) {
	ctx := r.Context()
	size, _ := s.blobs.Size(ctx, digestStr)
	if err := s.blobs.Delete(ctx, digestStr); err != nil && !os.IsNotExist(err) {
		s.logger.Error("failed to delete blob", "digest", digestStr, "error", err)
	}
	if size > 0 {
		s.stats.BytesStored.Add(-size)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleStartUpload(w http.ResponseWriter, r *http.Request, repo string) {
	session, err := s.uploads.Start(repo)
	if err != nil {
		s.logger.Error("failed to start upload session", "error", err)
		writeOCIError(w, "BLOB_UPLOAD_INVALID", "failed to start upload")
		return
	}
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

	defer r.Body.Close()
	updated, _, err := s.uploads.Append(uuid, r.Body)
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

	if r.ContentLength > 0 {
		defer r.Body.Close()
		_, _, err := s.uploads.Append(uuid, r.Body)
		if err != nil {
			writeOCIError(w, "BLOB_UPLOAD_INVALID", "failed to read final chunk")
			return
		}
	}

	verifiedDgst, err := session.Digest()
	if err != nil {
		s.uploads.Remove(uuid)
		writeOCIError(w, "BLOB_UPLOAD_INVALID", "failed to verify digest")
		return
	}
	if dgst != verifiedDgst {
		s.uploads.Remove(uuid)
		writeOCIError(w, "DIGEST_INVALID", fmt.Sprintf("digest mismatch: expected %s, got %s", dgst, verifiedDgst))
		return
	}

	ctx := r.Context()
	if err := s.blobs.PutUncheckedFromReader(ctx, dgst, session.File, session.Size); err != nil {
		writeOCIError(w, "BLOB_UPLOAD_INVALID", err.Error())
		return
	}

	s.uploads.Remove(uuid)
	s.stats.Pushes.Add(1)
	s.stats.BytesServed.Add(session.Size)
	s.stats.BytesStored.Add(session.Size)
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

func (s *Server) handleRepoDetail(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/")
	if repo == "" {
		http.Error(w, "repo name required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	tags, err := s.tags.ListTags(ctx, repo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if tags == nil {
		tags = []string{}
	}

	type tagInfo struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
	}
	tagInfos := make([]tagInfo, 0, len(tags))
	for _, tag := range tags {
		dgst, _ := s.tags.GetTag(ctx, repo, tag)
		if dgst == "" {
			dgst = "-"
		}
		tagInfos = append(tagInfos, tagInfo{Name: tag, Digest: dgst})
	}

	result := map[string]interface{}{
		"name": repo,
		"tags": tagInfos,
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

// handleReferrers returns an empty OCI referrers index.
func (s *Server) handleReferrers(w http.ResponseWriter, r *http.Request, repo, ref string) {
	w.Header().Set("Content-Type", ociIndexMediaType)
	if r.Method == "HEAD" {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`))
}

var (
	ociIndexMediaType    = "application/vnd.oci.image.index.v1+json"
	ociManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	dockerManifestV2     = "application/vnd.docker.distribution.manifest.v2+json"
	dockerManifestList   = "application/vnd.docker.distribution.manifest.list.v2+json"
)

type manifestHeader struct {
	MediaType string `json:"mediaType"`
}

func detectManifestContentType(data []byte) string {
	var h manifestHeader
	if err := json.Unmarshal(data, &h); err == nil {
		switch h.MediaType {
		case ociIndexMediaType:
			return ociIndexMediaType
		case ociManifestMediaType:
			return ociManifestMediaType
		case dockerManifestList:
			return dockerManifestList
		}
	}
	return dockerManifestV2
}

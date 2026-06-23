package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

type UpstreamConfig struct {
	Default  string
	Mappings map[string]string
	Username string
	Password string
	Cooldown time.Duration
}

type Proxy struct {
	config  UpstreamConfig
	blobs   *BlobStore
	tags    *TagStore
	stats   *Stats
	logger  *slog.Logger
	client  *http.Client
}

func NewProxy(config UpstreamConfig, blobs *BlobStore, tags *TagStore, stats *Stats, logger *slog.Logger) *Proxy {
	return &Proxy{
		config: config,
		blobs:  blobs,
		tags:   tags,
		stats:  stats,
		logger: logger,
		client: &http.Client{
			Timeout: 10 * time.Minute,
		},
	}
}

type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
}

func (p *Proxy) getToken(ctx context.Context, upstream, repo string) string {
	checkURL := fmt.Sprintf("%s/v2/", upstream)
	req, err := http.NewRequestWithContext(ctx, "GET", checkURL, nil)
	if err != nil {
		return ""
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return ""
	}
	authHeader := resp.Header.Get("Www-Authenticate")
	if authHeader == "" {
		return ""
	}
	realm, service, scope := parseAuthHeader(authHeader)
	if realm == "" {
		return ""
	}
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	tokenURL := fmt.Sprintf("%s?service=%s&scope=%s", realm, service, scope)
	tokenReq, err := http.NewRequestWithContext(ctx, "GET", tokenURL, nil)
	if err != nil {
		return ""
	}
	tokenResp, err := p.client.Do(tokenReq)
	if err != nil {
		return ""
	}
	defer tokenResp.Body.Close()
	var tr tokenResponse
	body, _ := io.ReadAll(tokenResp.Body)
	json.Unmarshal(body, &tr)
	if tr.Token != "" {
		return tr.Token
	}
	return tr.AccessToken
}

func parseAuthHeader(header string) (realm, service, scope string) {
	header = strings.TrimPrefix(header, "Bearer ")
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		val := strings.Trim(kv[1], `"`)
		switch kv[0] {
		case "realm":
			realm = val
		case "service":
			service = val
		case "scope":
			scope = val
		}
	}
	return
}

// FetchBlob streams a blob from upstream and stores it directly in MonoFS.
// Returns the blob size on success. The caller should then serve from local storage.
func (p *Proxy) FetchBlob(ctx context.Context, repo, digest string) (int64, error) {
	exists, err := p.blobs.Exists(ctx, digest)
	if err == nil && exists {
		size, _ := p.blobs.Size(ctx, digest)
		p.stats.CacheHits.Add(1)
		return size, nil
	}
	p.stats.CacheMisses.Add(1)

	upstream := p.resolveUpstream(repo)
	p.logger.Debug("fetching blob from upstream", "repo", repo, "digest", digest, "upstream", upstream)

	size, err := p.fetchBlobFromUpstream(ctx, upstream, repo, digest)
	if err != nil {
		p.logger.Error("upstream blob fetch failed", "repo", repo, "digest", digest, "error", err)
		return 0, err
	}

	p.stats.BytesFetched.Add(size)
	p.stats.BytesStored.Add(size)
	p.stats.BlobCount.Add(1)
	return size, nil
}

func (p *Proxy) FetchManifest(ctx context.Context, repo, ref string) ([]byte, string, error) {
	data, dgst, err := p.tags.GetManifest(ctx, repo, ref)
	if err == nil {
		p.stats.CacheHits.Add(1)
		p.stats.BytesServed.Add(int64(len(data)))
		return data, dgst, nil
	}
	p.stats.CacheMisses.Add(1)

	upstream := p.resolveUpstream(repo)
	p.logger.Debug("fetching manifest from upstream", "repo", repo, "ref", ref, "upstream", upstream)

	data, contentType, err := p.fetchManifestFromUpstream(ctx, upstream, repo, ref)
	if err != nil {
		p.logger.Error("upstream manifest fetch failed", "repo", repo, "ref", ref, "error", err)
		return nil, "", err
	}

	dgst, err = p.tags.PutManifest(ctx, repo, ref, data)
	if err != nil {
		p.logger.Warn("failed to cache manifest in monofs", "repo", repo, "ref", ref, "error", err)
	}
	p.stats.BytesFetched.Add(int64(len(data)))

	_ = contentType
	return data, dgst, nil
}

func (p *Proxy) FetchTags(ctx context.Context, repo string) ([]string, error) {
	upstream := p.resolveUpstream(repo)
	url := fmt.Sprintf("%s/v2/%s/tags/list", upstream, repo)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	var result struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		if token := p.doAuth(ctx, upstream, repo); token != "" {
			req2, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
			req2.Header.Set("Authorization", "Bearer "+token)
			resp.Body.Close()
			resp2, err := p.client.Do(req2)
			if err != nil {
				return nil, err
			}
			defer resp2.Body.Close()
			resp = resp2
		}
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, os.ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result.Tags, nil
}

func (p *Proxy) resolveUpstream(repo string) string {
	for prefix, upstream := range p.config.Mappings {
		if strings.HasPrefix(repo, prefix+"/") || repo == prefix {
			return upstream
		}
	}
	if p.config.Default != "" {
		return p.config.Default
	}
	return "https://registry-1.docker.io"
}

func (p *Proxy) doAuth(ctx context.Context, upstream, repo string) string {
	// Check if we need a token
	token := p.getToken(ctx, upstream, repo)
	return token
}

func (p *Proxy) fetchBlobFromUpstream(ctx context.Context, upstream, repo, digest string) (int64, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", upstream, repo, digest)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		if token := p.doAuth(ctx, upstream, repo); token != "" {
			req2, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
			req2.Header.Set("Authorization", "Bearer "+token)
			resp.Body.Close()
			resp2, err := p.client.Do(req2)
			if err != nil {
				return 0, err
			}
			defer resp2.Body.Close()
			resp = resp2
		}
	}

	if resp.StatusCode == http.StatusNotFound {
		return 0, os.ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	// Stream directly from upstream response body to MonoFS chunks.
	// No intermediate buffering — data goes straight from HTTP body to chunked gRPC writes.
	if err := p.blobs.putChunkedFromReader(ctx, digest, resp.Body, resp.ContentLength); err != nil {
		return 0, fmt.Errorf("store blob from upstream: %w", err)
	}
	return resp.ContentLength, nil
}

func (p *Proxy) fetchManifestFromUpstream(ctx context.Context, upstream, repo, ref string) ([]byte, string, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", upstream, repo, ref)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.index.v1+json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		if token := p.doAuth(ctx, upstream, repo); token != "" {
			req2, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
			req2.Header.Set("Accept", req.Header.Get("Accept"))
			req2.Header.Set("Authorization", "Bearer "+token)
			resp.Body.Close()
			resp2, err := p.client.Do(req2)
			if err != nil {
				return nil, "", err
			}
			defer resp2.Body.Close()
			resp = resp2
		}
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, "", os.ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	contentType := resp.Header.Get("Content-Type")
	return data, contentType, nil
}


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
	config   UpstreamConfig
	blobs    *BlobStore
	tags     *TagStore
	stats    *Stats
	logger   *slog.Logger
	client   *http.Client
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

func (p *Proxy) FetchBlob(ctx context.Context, repo, digest string) ([]byte, error) {
	exists, err := p.blobs.Exists(ctx, digest)
	if err == nil && exists {
		data, err := p.blobs.Get(ctx, digest)
		if err == nil {
			p.stats.CacheHits.Add(1)
			p.stats.BytesServed.Add(int64(len(data)))
			return data, nil
		}
	}
	p.stats.CacheMisses.Add(1)

	upstream := p.resolveUpstream(repo)
	p.logger.Debug("fetching blob from upstream", "repo", repo, "digest", digest, "upstream", upstream)

	data, err := p.fetchBlobFromUpstream(ctx, upstream, repo, digest)
	if err != nil {
		p.logger.Error("upstream blob fetch failed", "repo", repo, "digest", digest, "error", err)
		return nil, err
	}

	if err := p.blobs.PutUnchecked(ctx, digest, data); err != nil {
		p.logger.Warn("failed to cache blob in monofs", "digest", digest, "error", err)
	}
	p.stats.BytesFetched.Add(int64(len(data)))
	p.stats.BlobCount.Add(1)
	return data, nil
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

func (p *Proxy) fetchBlobFromUpstream(ctx context.Context, upstream, repo, digest string) ([]byte, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", upstream, repo, digest)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, os.ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
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


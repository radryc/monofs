package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
)

type TagStore struct {
	client *Client
	blobs  *BlobStore

	// in-memory caches avoid MonoFS directory-listing inconsistency
	repoCache    map[string]bool
	tagCache     map[string][]string
	cacheExpiry  map[string]time.Time
	mu           sync.RWMutex
	stopCleanup  chan struct{}
}

const cacheTTL = 30 * time.Minute

func NewTagStore(client *Client, blobs *BlobStore) *TagStore {
	ts := &TagStore{
		client:      client,
		blobs:       blobs,
		repoCache:   map[string]bool{},
		tagCache:    map[string][]string{},
		cacheExpiry: map[string]time.Time{},
		stopCleanup: make(chan struct{}),
	}
	go ts.cacheCleanupLoop()
	return ts
}

func (t *TagStore) cacheCleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-t.stopCleanup:
			return
		case <-ticker.C:
			t.evictExpired()
		}
	}
}

func (t *TagStore) evictExpired() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for repo := range t.cacheExpiry {
		if now.After(t.cacheExpiry[repo]) {
			delete(t.repoCache, repo)
			delete(t.tagCache, repo)
			delete(t.cacheExpiry, repo)
		}
	}
}

func (t *TagStore) touchRepo(repo string) {
	t.mu.Lock()
	t.cacheExpiry[repo] = time.Now().Add(cacheTTL)
	t.mu.Unlock()
}

func (t *TagStore) Close() error {
	close(t.stopCleanup)
	return nil
}

func tagPath(repo, tag string) string {
	return repo + "/_tags/" + tag
}

var catalogPath = "_catalog"

type catalogEntry struct {
	Repos []string `json:"repos"`
}

func (t *TagStore) GetTag(ctx context.Context, repo, tag string) (string, error) {
	path := tagPath(repo, tag)
	data, err := t.client.Read(ctx, path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (t *TagStore) PutTag(ctx context.Context, repo, tag, manifestDigest string) error {
	path := tagPath(repo, tag)
	dir := repo + "/_tags"
	if err := t.client.CreateDir(ctx, dir); err != nil {
		return err
	}
	if err := t.client.Write(ctx, path, []byte(manifestDigest)); err != nil {
		return err
	}
	// Update in-memory cache
	t.mu.Lock()
	t.repoCache[repo] = true
	if cached, ok := t.tagCache[repo]; ok {
		for _, existing := range cached {
			if existing == tag {
				t.mu.Unlock()
				return t.addToCatalog(ctx, repo)
			}
		}
	}
	t.tagCache[repo] = append(t.tagCache[repo], tag)
	t.cacheExpiry[repo] = time.Now().Add(cacheTTL)
	t.mu.Unlock()
	return t.addToCatalog(ctx, repo)
}

func (t *TagStore) DeleteTag(ctx context.Context, repo, tag string) error {
	path := tagPath(repo, tag)
	return t.client.Delete(ctx, path)
}

func (t *TagStore) ListTags(ctx context.Context, repo string) ([]string, error) {
	t.mu.RLock()
	if cached, ok := t.tagCache[repo]; ok {
		t.mu.RUnlock()
		t.touchRepo(repo)
		sort.Strings(cached)
		return cached, nil
	}
	t.mu.RUnlock()

	path := repo + "/_tags"
	entries, err := t.client.ListDir(ctx, path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Strings(entries)
	t.mu.Lock()
	t.tagCache[repo] = entries
	t.cacheExpiry[repo] = time.Now().Add(cacheTTL)
	t.mu.Unlock()
	return entries, nil
}

func (t *TagStore) ListRepos(ctx context.Context) ([]string, error) {
	t.mu.RLock()
	if len(t.repoCache) > 0 {
		repos := make([]string, 0, len(t.repoCache))
		for r := range t.repoCache {
			repos = append(repos, r)
		}
		t.mu.RUnlock()
		sort.Strings(repos)
		return repos, nil
	}
	t.mu.RUnlock()

	t.mu.Lock()
	defer t.mu.Unlock()

	// Populate cache: try directory listing first, then catalog file
	entries, err := t.client.ListDir(ctx, "")
	if err == nil && len(entries) > 0 {
		for _, entry := range entries {
			if !strings.HasPrefix(entry, "_") {
				t.repoCache[entry] = true
				t.cacheExpiry[entry] = time.Now().Add(cacheTTL)
			}
		}
	}
	// Also try the catalog file as a fallback/merge
	data, err2 := t.client.Read(ctx, catalogPath)
	if err2 == nil {
		var cat catalogEntry
		if err2 := json.Unmarshal(data, &cat); err2 == nil {
			for _, r := range cat.Repos {
				t.repoCache[r] = true
				t.cacheExpiry[r] = time.Now().Add(cacheTTL)
			}
		}
	}
	if len(t.repoCache) == 0 {
		return nil, nil
	}
	repos := make([]string, 0, len(t.repoCache))
	for r := range t.repoCache {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	return repos, nil
}

func (t *TagStore) addToCatalog(ctx context.Context, repo string) error {
	repos, _ := t.ListRepos(ctx)
	for _, r := range repos {
		if r == repo {
			return nil
		}
	}
	repos = append(repos, repo)
	sort.Strings(repos)
	data, err := json.Marshal(catalogEntry{Repos: repos})
	if err != nil {
		return err
	}
	return t.client.Write(ctx, catalogPath, data)
}

func (t *TagStore) GetManifest(ctx context.Context, repo, ref string) ([]byte, string, error) {
	dgst := digest.Digest(ref)
	if err := dgst.Validate(); err == nil {
		data, err := t.blobs.Get(ctx, string(dgst))
		return data, string(dgst), err
	}

	tagDigest, err := t.GetTag(ctx, repo, ref)
	if err != nil {
		return nil, "", err
	}
	data, err := t.blobs.Get(ctx, tagDigest)
	return data, tagDigest, err
}

func (t *TagStore) PutManifest(ctx context.Context, repo, ref string, content []byte) (string, error) {
	dgst := "sha256:" + fmt.Sprintf("%x", sha256Hash(content))
	if err := t.blobs.PutUnchecked(ctx, dgst, content); err != nil {
		return "", fmt.Errorf("store manifest blob: %w", err)
	}

	d := digest.Digest(ref)
	if err := d.Validate(); err == nil {
		dgst = ref
		return dgst, nil
	}

	if err := t.PutTag(ctx, repo, ref, dgst); err != nil {
		return "", fmt.Errorf("put tag: %w", err)
	}
	return dgst, nil
}

func (t *TagStore) DeleteManifest(ctx context.Context, repo, ref string) error {
	dgst := digest.Digest(ref)
	if err := dgst.Validate(); err == nil {
		return t.blobs.Delete(ctx, string(dgst))
	}
	return t.DeleteTag(ctx, repo, ref)
}

package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/opencontainers/go-digest"
)

type TagStore struct {
	client *Client
	blobs  *BlobStore
}

func NewTagStore(client *Client, blobs *BlobStore) *TagStore {
	return &TagStore{client: client, blobs: blobs}
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
	return t.addToCatalog(ctx, repo)
}

func (t *TagStore) DeleteTag(ctx context.Context, repo, tag string) error {
	path := tagPath(repo, tag)
	return t.client.Delete(ctx, path)
}

func (t *TagStore) ListTags(ctx context.Context, repo string) ([]string, error) {
	path := repo + "/_tags"
	entries, err := t.client.ListDir(ctx, path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Strings(entries)
	return entries, nil
}

func (t *TagStore) ListRepos(ctx context.Context) ([]string, error) {
	data, err := t.client.Read(ctx, catalogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		entries, err := t.client.ListDir(ctx, "")
		if err != nil {
			return nil, err
		}
		var repos []string
		for _, entry := range entries {
			if strings.HasPrefix(entry, "_") {
				continue
			}
			repos = append(repos, entry)
		}
		sort.Strings(repos)
		return repos, nil
	}
	var cat catalogEntry
	if err := json.Unmarshal(data, &cat); err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}
	sort.Strings(cat.Repos)
	return cat.Repos, nil
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
	dgst := "sha256:" + hexEncode(sha256Hash(content))
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

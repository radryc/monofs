package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Cache artifact type constants matching buildcache.ArtifactType values.
// Duplicated here to avoid import cycle between fetcher and buildcache.
const (
	artifactGoModInfo    = "go_mod_info"
	artifactGoModFile    = "go_mod_file"
	artifactGoModZip     = "go_mod_zip"
	artifactGoModZipHash = "go_mod_ziphash"
	artifactGoModList    = "go_mod_list"
	artifactGoModLock    = "go_mod_lock"
)

// fetchCacheArtifact handles requests where artifact_type is set in SourceConfig.
// Called from GoModBackend.FetchBlob() when the request targets a download cache file.
//
// Data flow:
//
//	Server node has metadata: {artifact_type: "go_mod_info", module: "...", version: "..."}
//	Server calls fetcher with these fields in SourceConfig
//	This function generates the actual bytes on-demand
func (gb *GoModBackend) fetchCacheArtifact(ctx context.Context, req *FetchRequest) (*FetchResult, error) {
	artifactType := req.SourceConfig["artifact_type"]
	modulePath := req.SourceConfig["module"]
	version := req.SourceConfig["version"]

	if modulePath == "" || version == "" {
		return nil, fmt.Errorf("module and version required for cache artifact %q", artifactType)
	}

	switch artifactType {
	case artifactGoModInfo:
		return gb.generateModInfo(ctx, modulePath, version)
	case artifactGoModFile:
		return gb.generateModFile(ctx, modulePath, version)
	case artifactGoModZip:
		return gb.generateModZip(ctx, modulePath, version)
	case artifactGoModZipHash:
		return gb.generateModZipHash(ctx, modulePath, version)
	case artifactGoModList:
		return gb.generateModList(ctx, modulePath, version)
	case artifactGoModLock:
		return gb.generateModLock(ctx, modulePath, version)
	default:
		return nil, fmt.Errorf("unknown Go cache artifact type: %q", artifactType)
	}
}

// generateModInfo produces the .info JSON for a module version.
// Fetches from Go module proxy, or generates minimal fallback.
// Example content: {"Version":"v1.6.0","Time":"2024-01-15T10:30:00Z"}
func (gb *GoModBackend) generateModInfo(ctx context.Context, modulePath, version string) (*FetchResult, error) {
	escaped := escapeModulePath(modulePath)
	url := fmt.Sprintf("%s/%s/@v/%s.info", gb.proxyURL, escaped, version)

	content, err := gb.proxyGet(ctx, url)
	if err != nil {
		// Fallback: generate minimal .info that satisfies the Go toolchain
		info := struct {
			Version string    `json:"Version"`
			Time    time.Time `json:"Time"`
		}{
			Version: version,
			Time:    time.Now().UTC(),
		}
		content, _ = json.Marshal(info)
	}

	return &FetchResult{
		Content: content,
		Size:    int64(len(content)),
	}, nil
}

// generateModFile extracts go.mod from the cached module zip.
// Falls back to fetching from the Go module proxy.
func (gb *GoModBackend) generateModFile(ctx context.Context, modulePath, version string) (*FetchResult, error) {
	// Try reading from cached zip first (no extra network call)
	cached, err := gb.getOrDownloadModule(ctx, modulePath, version)
	if err == nil {
		content, readErr := gb.readFileFromModule(cached, "go.mod")
		if readErr == nil {
			return &FetchResult{Content: content, Size: int64(len(content)), FromCache: true}, nil
		}
	}

	// Fallback: fetch .mod directly from proxy
	escaped := escapeModulePath(modulePath)
	url := fmt.Sprintf("%s/%s/@v/%s.mod", gb.proxyURL, escaped, version)
	content, fetchErr := gb.proxyGet(ctx, url)
	if fetchErr != nil {
		return nil, fmt.Errorf("failed to get go.mod: zip=%v, proxy=%v", err, fetchErr)
	}
	return &FetchResult{Content: content, Size: int64(len(content))}, nil
}

// generateModZip serves the module.zip from the fetcher's local cache.
// This is the same zip that the fetcher downloads via getOrDownloadModule().
func (gb *GoModBackend) generateModZip(ctx context.Context, modulePath, version string) (*FetchResult, error) {
	cached, err := gb.getOrDownloadModule(ctx, modulePath, version)
	if err != nil {
		return nil, fmt.Errorf("failed to get module for .zip: %w", err)
	}

	data, err := os.ReadFile(cached.zipPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read cached zip: %w", err)
	}

	return &FetchResult{
		Content:   data,
		Size:      int64(len(data)),
		FromCache: true,
	}, nil
}

// generateModZipHash computes the h1: hash for the module zip.
// Uses golang.org/x/mod/sumdb/dirhash.HashZip for correct output.
// Example output: "h1:ZvwS0R+56ePWxUNi+Atn9dWONBPp/AUETXlHW0DxSjE="
func (gb *GoModBackend) generateModZipHash(ctx context.Context, modulePath, version string) (*FetchResult, error) {
	cached, err := gb.getOrDownloadModule(ctx, modulePath, version)
	if err != nil {
		return nil, fmt.Errorf("failed to get module for .ziphash: %w", err)
	}

	// Use the canonical Go library for correct h1: hash computation.
	// dirhash.HashZip produces the same hash the Go toolchain expects.
	hashStr, err := hashZipFile(cached.zipPath)
	if err != nil {
		return nil, fmt.Errorf("failed to compute ziphash: %w", err)
	}

	content := []byte(hashStr)
	return &FetchResult{
		Content:   content,
		Size:      int64(len(content)),
		FromCache: true,
	}, nil
}

// generateModList returns the version list file.
// Contains a single version line for this module.
func (gb *GoModBackend) generateModList(ctx context.Context, modulePath, version string) (*FetchResult, error) {
	content := []byte(version + "\n")
	return &FetchResult{
		Content:   content,
		Size:      int64(len(content)),
		FromCache: true,
	}, nil
}

// generateModLock returns an empty file (download-complete sentinel).
func (gb *GoModBackend) generateModLock(ctx context.Context, modulePath, version string) (*FetchResult, error) {
	return &FetchResult{
		Content:   []byte{},
		Size:      0,
		FromCache: true,
	}, nil
}

// proxyGet performs an HTTP GET to the Go module proxy.
func (gb *GoModBackend) proxyGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := gb.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

// hashZipFile computes the h1: dirhash of a module zip file.
// This wraps dirhash.HashZip from golang.org/x/mod.
// If golang.org/x/mod is not available, it falls back to a simple SHA-256.
func hashZipFile(zipPath string) (string, error) {
	// Use golang.org/x/mod/sumdb/dirhash for correct h1: hash.
	// Import is in a separate file to isolate the dependency:
	// see gomod_artifacts_hash.go
	return computeDirHash(zipPath)
}

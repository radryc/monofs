// Package executor provides a Bazel-compatible remote execution worker.
// It fetches actions from the CAS (monofs-cache), resolves input trees,
// executes commands in isolated workdirs, and uploads outputs.
//
// The executor can run standalone (monofs-executor binary) or as part of
// a pipeline worker (--executor-port flag on monofs-pipeline-worker).
package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Config configures an executor.
type Config struct {
	// ID is a unique worker identifier.
	ID string

	// CacheAddr is the monofs-cache HTTP endpoint for fetching/storing blobs.
	CacheAddr string

	// WorkDir is the root for action workspaces.
	WorkDir string

	// MaxJobs is the maximum concurrent action executions.
	MaxJobs int

	// Platform describes this worker's capabilities.
	Platform Platform

	// Logger is used for execution logging.
	Logger *slog.Logger

	// HTTPClient for fetching/storing blobs in monofs-cache.
	HTTPClient *http.Client
}

// Platform describes execution environment capabilities.
type Platform struct {
	OS   string
	Arch string
	Pool string
}

// Executor runs build actions.
type Executor struct {
	cfg        Config
	logger     *slog.Logger
	httpClient *http.Client

	// Metrics
	totalExecs  atomic.Int64
	failedExecs atomic.Int64
	activeExecs atomic.Int64
	startTime   time.Time

	sem chan struct{} // concurrency limiter
}

// New creates an Executor.
func New(cfg Config) *Executor {
	if cfg.MaxJobs <= 0 {
		cfg.MaxJobs = 4
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 120 * time.Second}
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = os.TempDir()
	}
	e := &Executor{
		cfg:        cfg,
		logger:     cfg.Logger.With("component", "executor", "worker", cfg.ID),
		httpClient: cfg.HTTPClient,
		startTime:  time.Now(),
		sem:        make(chan struct{}, cfg.MaxJobs),
	}
	return e
}

// ExecuteRequest is a simplified execution request.
type ExecuteRequest struct {
	// Arguments is the command line.
	Arguments []string
	// EnvironmentVariables are key=value pairs.
	EnvironmentVariables []string
	// InputFiles maps relative paths to their content digests.
	InputFiles []InputFile
	// OutputFiles lists expected output file paths.
	OutputFiles []string
	// OutputDirectories lists expected output directory paths.
	OutputDirectories []string
	// WorkingDirectory is the subdirectory under WorkDir to use.
	WorkingDirectory string
	// Timeout is the maximum execution duration.
	Timeout time.Duration
}

// InputFile describes a single input file.
type InputFile struct {
	Path   string
	Digest string // "hash/size" format
}

// ExecuteResult is the result of executing an action.
type ExecuteResult struct {
	ExitCode          int
	Stdout            []byte
	Stderr            []byte
	OutputFiles       []OutputFileResult
	OutputDirectories []OutputDirResult
	Duration          time.Duration
	Error             string
}

// OutputFileResult describes a single output file with its digest.
type OutputFileResult struct {
	Path   string
	Digest string // "hash/size"
	Size   int64
}

// OutputDirResult describes an output directory with its tree digest.
type OutputDirResult struct {
	Path       string
	TreeDigest string
}

// Execute runs an action.
func (e *Executor) Execute(ctx context.Context, req *ExecuteRequest) (*ExecuteResult, error) {
	// Acquire concurrency slot.
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	e.activeExecs.Add(1)
	e.totalExecs.Add(1)
	defer e.activeExecs.Add(-1)

	start := time.Now()
	result := &ExecuteResult{}

	// Create isolated workdir.
	workDir := filepath.Join(e.cfg.WorkDir, req.WorkingDirectory)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		result.Error = fmt.Sprintf("mkdir workdir: %v", err)
		e.failedExecs.Add(1)
		return result, nil
	}
	defer os.RemoveAll(workDir) // clean up after execution.

	// Fetch input files from cache.
	for _, f := range req.InputFiles {
		dst := filepath.Join(workDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			result.Error = fmt.Sprintf("mkdir for input %s: %v", f.Path, err)
			e.failedExecs.Add(1)
			return result, nil
		}
		data, err := e.fetchCAS(ctx, f.Digest)
		if err != nil {
			result.Error = fmt.Sprintf("fetch input %s (%s): %v", f.Path, f.Digest, err)
			e.failedExecs.Add(1)
			return result, nil
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			result.Error = fmt.Sprintf("write input %s: %v", f.Path, err)
			e.failedExecs.Add(1)
			return result, nil
		}
	}

	// Set up timeout.
	execCtx := ctx
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	// Build command.
	cmd := exec.CommandContext(execCtx, req.Arguments[0], req.Arguments[1:]...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), req.EnvironmentVariables...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Execute.
	runErr := cmd.Run()
	result.Duration = time.Since(start)
	result.Stdout = stdout.Bytes()
	result.Stderr = stderr.Bytes()

	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
			result.Error = runErr.Error()
			e.failedExecs.Add(1)
			return result, nil
		}
	}

	// Capture and upload output files.
	for _, outPath := range req.OutputFiles {
		fullPath := filepath.Join(workDir, outPath)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			// Output file not produced — not necessarily an error.
			e.logger.Debug("output file not found", "path", outPath)
			continue
		}
		digest := e.storeCAS(ctx, data)
		result.OutputFiles = append(result.OutputFiles, OutputFileResult{
			Path:   outPath,
			Digest: digest,
			Size:   int64(len(data)),
		})
	}

	// Capture and upload output directories.
	for _, outDir := range req.OutputDirectories {
		fullDir := filepath.Join(workDir, outDir)
		treeDigest, err := e.storeDirectory(ctx, fullDir)
		if err != nil {
			e.logger.Debug("output dir not found", "path", outDir, "error", err)
			continue
		}
		result.OutputDirectories = append(result.OutputDirectories, OutputDirResult{
			Path:       outDir,
			TreeDigest: treeDigest,
		})
	}

	e.logger.Debug("execution complete",
		"args", req.Arguments,
		"exit", result.ExitCode,
		"duration", result.Duration,
	)
	return result, nil
}

// fetchCAS retrieves a blob from monofs-cache by digest.
func (e *Executor) fetchCAS(ctx context.Context, digest string) ([]byte, error) {
	if e.cfg.CacheAddr == "" {
		return nil, fmt.Errorf("no cache address configured")
	}
	url := fmt.Sprintf("http://%s/cas/%s", e.cfg.CacheAddr, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch CAS %s: %w", digest, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch CAS %s: HTTP %d", digest, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// storeCAS uploads a blob to monofs-cache and returns its digest.
func (e *Executor) storeCAS(ctx context.Context, data []byte) string {
	h := sha256.Sum256(data)
	digest := fmt.Sprintf("%s/%d", hex.EncodeToString(h[:]), len(data))

	if e.cfg.CacheAddr == "" {
		return digest
	}

	url := fmt.Sprintf("http://%s/cas/%s", e.cfg.CacheAddr, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		e.logger.Warn("store CAS request failed", "digest", digest, "error", err)
		return digest
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		e.logger.Warn("store CAS failed", "digest", digest, "error", err)
		return digest
	}
	resp.Body.Close()
	return digest
}

// storeDirectory walks a directory tree, uploads each file to CAS,
// and returns a tree digest (hash of a manifest listing all files).
func (e *Executor) storeDirectory(ctx context.Context, dir string) (string, error) {
	var manifest strings.Builder

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := e.storeCAS(ctx, data)
		manifest.WriteString(fmt.Sprintf("%s %s\n", rel, digest))
		return nil
	})
	if err != nil {
		return "", err
	}

	h := sha256.Sum256([]byte(manifest.String()))
	return fmt.Sprintf("%s/%d", hex.EncodeToString(h[:]), manifest.Len()), nil
}

// Status returns executor health and metrics.
func (e *Executor) Status() map[string]interface{} {
	return map[string]interface{}{
		"worker_id":    e.cfg.ID,
		"platform":     e.cfg.Platform,
		"uptime_secs":  time.Since(e.startTime).Seconds(),
		"total_execs":  e.totalExecs.Load(),
		"failed_execs": e.failedExecs.Load(),
		"active_execs": e.activeExecs.Load(),
		"max_jobs":     e.cfg.MaxJobs,
	}
}

// Close releases resources.
func (e *Executor) Close() error { return nil }

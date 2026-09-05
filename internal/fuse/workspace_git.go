package fuse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	workspaceGitBranchName   = "main"
	workspaceGitCommitTitle  = "MonoFS workspace baseline"
	workspaceGitSyncEnvKey   = "MONOFS_WORKSPACE_GIT_SYNC"
	workspaceGitSyncEnvValue = "1"
)

type workspaceGitProjection interface {
	GitFileContent() []byte
	Sync(ctx context.Context) error
}

// WorkspaceGitProjection maintains a local gitdir that mirrors the mounted
// virtual monorepo root so Git-aware tooling can treat the mount as a worktree.
type WorkspaceGitProjection struct {
	mountPoint string
	gitDir     string
	sessionMgr *SessionManager
	logger     *slog.Logger
	owner      nodeOwner

	mu sync.Mutex
}

func NewWorkspaceGitProjection(mountPoint, stateDir string, sessionMgr *SessionManager, logger *slog.Logger, uid, gid uint32) (*WorkspaceGitProjection, error) {
	if strings.TrimSpace(mountPoint) == "" {
		return nil, fmt.Errorf("workspace git projection requires a mount point")
	}
	if strings.TrimSpace(stateDir) == "" {
		return nil, fmt.Errorf("workspace git projection requires a state directory")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("workspace git projection requires git in PATH: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return nil, fmt.Errorf("create workspace git state dir: %w", err)
	}

	gitDir := filepath.Join(stateDir, fmt.Sprintf("workspace-%s.git", workspaceGitProjectionKey(mountPoint)))
	return &WorkspaceGitProjection{
		mountPoint: mountPoint,
		gitDir:     gitDir,
		sessionMgr: sessionMgr,
		owner:      nodeOwner{uid: uid, gid: gid},
		logger:     logger.With("component", "workspace-git", "mount", mountPoint),
	}, nil
}

func (p *WorkspaceGitProjection) SetOwner(uid, gid uint32) {
	if p == nil {
		return
	}
	p.owner = nodeOwner{uid: uid, gid: gid}
}

func workspaceGitProjectionKey(mountPoint string) string {
	sum := sha256.Sum256([]byte(mountPoint))
	return hex.EncodeToString(sum[:6])
}

func (p *WorkspaceGitProjection) GitFileContent() []byte {
	return []byte(fmt.Sprintf("gitdir: %s\n", p.gitDir))
}

func (p *WorkspaceGitProjection) Sync(ctx context.Context) error {
	if p == nil {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.ensureRepo(ctx); err != nil {
		return err
	}
	if err := p.writeInfoExclude(); err != nil {
		return err
	}
	if err := p.ensureOwnership(); err != nil {
		return err
	}
	if err := p.removeExcludedPathsFromIndex(ctx); err != nil {
		return err
	}
	if err := p.runGit(ctx, "add", "-A"); err != nil {
		return err
	}

	statusOutput, err := p.gitOutput(ctx, "status", "--porcelain")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(statusOutput)) == 0 {
		if _, err := p.gitOutput(ctx, "rev-parse", "--verify", "HEAD"); err == nil {
			return nil
		}
	}

	if err := p.runGitWithEnv(ctx, map[string]string{workspaceGitSyncEnvKey: workspaceGitSyncEnvValue},
		"-c", "user.name=MonoFS Workspace",
		"-c", "user.email=monofs-workspace@local",
		"commit", "--allow-empty", "-m", workspaceGitCommitTitle,
	); err != nil {
		if strings.Contains(err.Error(), "nothing to commit") {
			return nil
		}
		return err
	}

	return nil
}

func (p *WorkspaceGitProjection) ensureRepo(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(p.gitDir, "HEAD")); err == nil {
		return p.configureRepo(ctx)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat workspace git dir: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(p.gitDir), 0755); err != nil {
		return fmt.Errorf("create workspace git parent dir: %w", err)
	}
	if err := p.runGitBare(ctx, "init", "--bare", p.gitDir); err != nil {
		return err
	}
	if err := p.ensureOwnership(); err != nil {
		return err
	}
	return p.configureRepo(ctx)
}

func (p *WorkspaceGitProjection) ensureOwnership() error {
	if p == nil {
		return nil
	}
	for _, path := range []string{filepath.Dir(p.gitDir), p.gitDir} {
		if err := ensurePathOwner(path, p.owner); err != nil {
			return fmt.Errorf("ensure workspace git ownership for %q: %w", path, err)
		}
	}
	return nil
}

func (p *WorkspaceGitProjection) configureRepo(ctx context.Context) error {
	if err := p.runGitBare(ctx, "config", "core.bare", "false"); err != nil {
		return err
	}
	if err := p.runGitBare(ctx, "config", "core.worktree", p.mountPoint); err != nil {
		return err
	}
	if err := p.runGitBare(ctx, "symbolic-ref", "HEAD", "refs/heads/"+workspaceGitBranchName); err != nil {
		return err
	}
	if err := p.installCommitGuardHook(); err != nil {
		return err
	}
	return nil
}

func (p *WorkspaceGitProjection) installCommitGuardHook() error {
	hooksDir := filepath.Join(p.gitDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		return fmt.Errorf("create workspace git hooks dir: %w", err)
	}
	guardPath := filepath.Join(hooksDir, "pre-commit")
	guard := "#!/bin/sh\n" +
		"if [ \"$" + workspaceGitSyncEnvKey + "\" = \"" + workspaceGitSyncEnvValue + "\" ]; then\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo \"MonoFS root Git metadata is synthetic. Use 'monofs-session commit' to publish workspace changes.\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(guardPath, []byte(guard), 0755); err != nil {
		return fmt.Errorf("write workspace git commit guard: %w", err)
	}
	return nil
}

func (p *WorkspaceGitProjection) writeInfoExclude() error {
	excludePath := filepath.Join(p.gitDir, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0755); err != nil {
		return fmt.Errorf("create workspace git info dir: %w", err)
	}
	patterns := make([]string, 0, len(p.excludedRootPaths()))
	for _, path := range p.excludedRootPaths() {
		clean := strings.Trim(strings.TrimSpace(path), "/")
		if clean == "" {
			continue
		}
		if clean == ".monofs" || strings.HasPrefix(clean, ".monofs/") {
			patterns = append(patterns, "/.monofs/")
			continue
		}
		if clean == syntheticWorkspaceGitName {
			patterns = append(patterns, "/.git")
			continue
		}
		if !strings.HasPrefix(clean, "FS_ERROR.txt") {
			patterns = append(patterns, "/"+clean+"/")
			continue
		}
		patterns = append(patterns, "/"+clean)
	}
	patterns = append(patterns, "/FS_ERROR.txt")
	sort.Strings(patterns)
	content := strings.Join(deduplicateStrings(patterns), "\n") + "\n"
	if err := os.WriteFile(excludePath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write workspace git exclude file: %w", err)
	}
	return nil
}

func (p *WorkspaceGitProjection) excludedRootPaths() []string {
	paths := []string{syntheticWorkspaceGitName, syntheticWorkspaceControlDirName, "FS_ERROR.txt"}
	if p.sessionMgr != nil {
		paths = append(paths, p.sessionMgr.ListUserRootDirs()...)
	}
	return deduplicateStrings(paths)
}

func (p *WorkspaceGitProjection) removeExcludedPathsFromIndex(ctx context.Context) error {
	paths := p.excludedRootPaths()
	if len(paths) == 0 {
		return nil
	}
	args := []string{"rm", "-r", "--cached", "--ignore-unmatch", "--"}
	args = append(args, paths...)
	return p.runGit(ctx, args...)
}

func deduplicateStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

func (p *WorkspaceGitProjection) gitOutput(ctx context.Context, args ...string) ([]byte, error) {
	return p.gitOutputWithEnv(ctx, nil, args...)
}

func (p *WorkspaceGitProjection) gitOutputWithEnv(ctx context.Context, extraEnv map[string]string, args ...string) ([]byte, error) {
	cmdArgs := append([]string{"--git-dir=" + p.gitDir, "--work-tree=" + p.mountPoint}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Env = append(os.Environ(), formatGitEnv(extraEnv)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func (p *WorkspaceGitProjection) runGit(ctx context.Context, args ...string) error {
	_, err := p.gitOutput(ctx, args...)
	return err
}

func (p *WorkspaceGitProjection) runGitWithEnv(ctx context.Context, extraEnv map[string]string, args ...string) error {
	_, err := p.gitOutputWithEnv(ctx, extraEnv, args...)
	return err
}

func (p *WorkspaceGitProjection) runGitBare(ctx context.Context, args ...string) error {
	cmdArgs := append([]string{"--git-dir=" + p.gitDir}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func formatGitEnv(extraEnv map[string]string) []string {
	if len(extraEnv) == 0 {
		return nil
	}
	keys := make([]string, 0, len(extraEnv))
	for key := range extraEnv {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	formatted := make([]string, 0, len(keys))
	for _, key := range keys {
		formatted = append(formatted, key+"="+extraEnv[key])
	}
	return formatted
}

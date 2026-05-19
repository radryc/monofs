package fuse

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestWorkspaceGitProjectionSyncCreatesCleanRepoAndTracksChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable not available")
	}

	mountPoint := t.TempDir()
	stateDir := t.TempDir()
	filePath := filepath.Join(mountPoint, "github.com", "acme", "repo", "main.go")
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(mountPoint, syntheticGitignoreName), []byte(monorepoGitignore), 0644); err != nil {
		t.Fatalf("WriteFile(.gitignore) error = %v", err)
	}
	if err := os.WriteFile(filePath, []byte("package main\n"), 0644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}

	projection, err := NewWorkspaceGitProjection(mountPoint, stateDir, nil, testLogger(), uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatalf("NewWorkspaceGitProjection() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(mountPoint, syntheticWorkspaceGitName), projection.GitFileContent(), 0644); err != nil {
		t.Fatalf("WriteFile(.git) error = %v", err)
	}

	if err := projection.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if got := gitStatusShort(t, mountPoint); got != "" {
		t.Fatalf("git status after initial sync = %q, want clean worktree", got)
	}

	if err := os.WriteFile(filePath, []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatalf("rewrite main.go error = %v", err)
	}
	if got := gitStatusShort(t, mountPoint); !strings.Contains(got, "M github.com/acme/repo/main.go") {
		t.Fatalf("git status after edit = %q, want modified file", got)
	}

	if err := projection.Sync(context.Background()); err != nil {
		t.Fatalf("Sync(after edit) error = %v", err)
	}
	if got := gitStatusShort(t, mountPoint); got != "" {
		t.Fatalf("git status after resync = %q, want clean worktree", got)
	}

	commitOutput := gitOutput(t, mountPoint, "log", "-1", "--pretty=%s")
	if strings.TrimSpace(commitOutput) != workspaceGitCommitTitle {
		t.Fatalf("last commit title = %q, want %q", strings.TrimSpace(commitOutput), workspaceGitCommitTitle)
	}
	if hookPath := filepath.Join(projection.gitDir, "hooks", "pre-commit"); !fileExists(hookPath) {
		t.Fatalf("pre-commit hook missing at %s", hookPath)
	}
	if excludePath := filepath.Join(projection.gitDir, "info", "exclude"); !fileContains(t, excludePath, "/.git") {
		t.Fatalf("exclude file missing synthetic .git pattern")
	}
}

func TestWorkspaceGitProjectionExcludesUserRootDirs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable not available")
	}

	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	mountPoint := t.TempDir()
	stateDir := t.TempDir()
	if err := sessionMgr.CreateUserRootDir("scratch"); err != nil {
		t.Fatalf("CreateUserRootDir() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(mountPoint, "scratch"), 0755); err != nil {
		t.Fatalf("MkdirAll(scratch) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(mountPoint, "scratch", "note.txt"), []byte("temporary\n"), 0644); err != nil {
		t.Fatalf("WriteFile(scratch/note.txt) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(mountPoint, syntheticGitignoreName), []byte(monorepoGitignore), 0644); err != nil {
		t.Fatalf("WriteFile(.gitignore) error = %v", err)
	}

	projection, err := NewWorkspaceGitProjection(mountPoint, stateDir, sessionMgr, testLogger(), uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatalf("NewWorkspaceGitProjection() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(mountPoint, syntheticWorkspaceGitName), projection.GitFileContent(), 0644); err != nil {
		t.Fatalf("WriteFile(.git) error = %v", err)
	}
	if err := projection.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if tracked := strings.TrimSpace(gitOutput(t, mountPoint, "ls-files")); tracked != syntheticGitignoreName {
		t.Fatalf("tracked files = %q, want only %q", tracked, syntheticGitignoreName)
	}
	if got := gitStatusShort(t, mountPoint); got != "" {
		t.Fatalf("git status with excluded user dir = %q, want clean worktree", got)
	}
}

func TestResolvePathOwnerReturnsNearestExistingAncestorOwner(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "missing", "overlay")

	uid, gid, err := ResolvePathOwner(target)
	if err != nil {
		t.Fatalf("ResolvePathOwner() error = %v", err)
	}

	info, err := os.Stat(base)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", base, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("temp dir stat did not expose uid/gid")
	}
	if uid != stat.Uid || gid != stat.Gid {
		t.Fatalf("ResolvePathOwner() = (%d, %d), want (%d, %d)", uid, gid, stat.Uid, stat.Gid)
	}
}

func gitStatusShort(t *testing.T, worktree string) string {
	t.Helper()
	return strings.TrimSpace(gitOutput(t, worktree, "status", "--short"))
}

func gitOutput(t *testing.T, worktree string, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"-C", worktree}, args...)
	cmd := exec.Command("git", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fileContains(t *testing.T, path, needle string) bool {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return strings.Contains(string(content), needle)
}

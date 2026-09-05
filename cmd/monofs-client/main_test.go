package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestNewShardedClientConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := newShardedClientConfig(
		"router.example:9090",
		"client-123",
		"/mnt/monofs",
		true,
		15*time.Second,
		logger,
		true,
	)

	if cfg.RouterAddr != "router.example:9090" {
		t.Fatalf("RouterAddr = %q, want %q", cfg.RouterAddr, "router.example:9090")
	}
	if cfg.ClientID != "client-123" {
		t.Fatalf("ClientID = %q, want %q", cfg.ClientID, "client-123")
	}
	if cfg.MountPoint != "/mnt/monofs" {
		t.Fatalf("MountPoint = %q, want %q", cfg.MountPoint, "/mnt/monofs")
	}
	if !cfg.Writable {
		t.Fatal("Writable = false, want true")
	}
	if cfg.RPCTimeout != 15*time.Second {
		t.Fatalf("RPCTimeout = %s, want %s", cfg.RPCTimeout, 15*time.Second)
	}
	if !cfg.UseExternalAddresses {
		t.Fatal("UseExternalAddresses = false, want true")
	}
	if cfg.RefreshInterval != 30*time.Second {
		t.Fatalf("RefreshInterval = %s, want %s", cfg.RefreshInterval, 30*time.Second)
	}
	if cfg.Logger == nil {
		t.Fatal("Logger = nil, want non-nil")
	}
}

func TestValidateClientPathsRejectsStateInsideMount(t *testing.T) {
	mountpoint := "/tmp/monofs"
	overlayDir := filepath.Join(mountpoint, "overlay")
	cacheDir := filepath.Join(mountpoint, "cache")
	workspaceGitDir := filepath.Join(overlayDir, "workspace-git")

	if err := validateClientPaths(mountpoint, overlayDir, "", workspaceGitDir); err == nil {
		t.Fatal("validateClientPaths() error = nil, want overlay path rejection")
	}
	if err := validateClientPaths(mountpoint, "", cacheDir, workspaceGitDir); err == nil {
		t.Fatal("validateClientPaths() error = nil, want cache path rejection")
	}
}

func TestValidateClientPathsAllowsExternalState(t *testing.T) {
	mountpoint := "/tmp/monofs"
	overlayDir := "/tmp/monofs-overlay"
	cacheDir := "/tmp/monofs-cache"
	workspaceGitDir := filepath.Join(overlayDir, "workspace-git")

	if err := validateClientPaths(mountpoint, overlayDir, cacheDir, workspaceGitDir); err != nil {
		t.Fatalf("validateClientPaths() error = %v, want nil", err)
	}
}

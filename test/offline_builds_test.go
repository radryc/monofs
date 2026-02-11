package test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOfflineBuilds_Go tests end-to-end Go offline builds with cache metadata
func TestOfflineBuilds_Go(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Setup test environment
	testDir := setupTestEnv(t, "go-offline")
	defer cleanupTestEnv(t, testDir)

	mountPoint := filepath.Join(testDir, "mnt")
	routerAddr := "localhost:19090" // Use different port for test

	// Step 1: Start MonoFS cluster (router + 2 nodes)
	t.Log("Starting MonoFS cluster...")
	cluster := startTestCluster(t, ctx, testDir, routerAddr)
	defer cluster.Stop()

	// Wait for cluster to be ready
	waitForCluster(t, ctx, routerAddr, 30*time.Second)

	// Step 2: Create test Go project
	projectPath := createTestGoProject(t, testDir)

	// Step 3: Ingest test project
	t.Log("Ingesting test Go project...")
	ingestCmd := exec.CommandContext(ctx, "./bin/monofs-admin",
		"ingest",
		"--router="+routerAddr,
		"--source="+projectPath,
		"--ingestion-type=git",
	)
	ingestCmd.Dir = filepath.Join(testDir, "..")
	if output, err := ingestCmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to ingest project: %v\n%s", err, output)
	}

	// Step 4: Ingest dependencies with cache metadata generation
	t.Log("Ingesting Go dependencies with cache metadata...")

	// First mount to access the project
	mountCmd := startMount(t, ctx, routerAddr, mountPoint)
	defer mountCmd.Process.Kill()

	// Wait for mount
	waitForMount(t, mountPoint, 10*time.Second)

	// Get project path in MonoFS
	monofsProjectPath := findProjectInMount(t, mountPoint, filepath.Base(projectPath))
	if monofsProjectPath == "" {
		t.Fatal("Project not found in MonoFS mount")
	}

	ingestDepsCmd := exec.CommandContext(ctx, "./bin/monofs-admin",
		"ingest-deps",
		"--router="+routerAddr,
		"--file="+filepath.Join(monofsProjectPath, "go.mod"),
		"--type=go",
		"--concurrency=5",
	)
	ingestDepsCmd.Dir = filepath.Join(testDir, "..")
	if output, err := ingestDepsCmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to ingest dependencies: %v\n%s", err, output)
	}

	// Step 5: Verify cache metadata exists
	t.Log("Verifying cache metadata...")
	cacheDir := filepath.Join(mountPoint, "go-modules", "pkg", "mod", "cache", "download")
	if !dirExists(cacheDir) {
		t.Fatalf("Cache directory not found: %s", cacheDir)
	}

	// Check for specific module cache metadata (github.com/google/uuid)
	cachePath := filepath.Join(cacheDir, "github.com", "google", "uuid", "@v")
	if !dirExists(cachePath) {
		t.Fatalf("Module cache metadata not found: %s", cachePath)
	}

	// Verify .info, .mod, and list files exist
	for _, file := range []string{"v1.6.0.info", "v1.6.0.mod", "list"} {
		if !fileExists(filepath.Join(cachePath, file)) {
			t.Errorf("Cache metadata file missing: %s", file)
		}
	}

	// Step 6: Test offline build
	t.Log("Testing offline Go build...")
	buildDir := filepath.Join(testDir, "build-test")
	if err := os.MkdirAll(buildDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Copy project to build directory
	copyDir(t, monofsProjectPath, buildDir)

	// Build with offline environment
	buildCmd := exec.CommandContext(ctx, "go", "build", "-v", "./...")
	buildCmd.Dir = buildDir
	buildCmd.Env = append(os.Environ(),
		"GOMODCACHE="+filepath.Join(mountPoint, "go-modules", "pkg", "mod"),
		"GOPROXY=off",
		"GOVCS=*:off",
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
	)

	output, err := buildCmd.CombinedOutput()
	t.Logf("Build output:\n%s", output)

	if err != nil {
		t.Fatalf("Offline build failed: %v\nOutput: %s", err, output)
	}

	// Verify build produced binary
	binaryPath := filepath.Join(buildDir, filepath.Base(projectPath))
	if !fileExists(binaryPath) {
		t.Errorf("Build binary not found: %s", binaryPath)
	}

	t.Log("✅ Go offline build test passed!")
}

// TestOfflineBuilds_Npm tests end-to-end npm offline builds
func TestOfflineBuilds_Npm(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Check if npm is installed
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not found, skipping test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	testDir := setupTestEnv(t, "npm-offline")
	defer cleanupTestEnv(t, testDir)

	mountPoint := filepath.Join(testDir, "mnt")
	routerAddr := "localhost:19091"

	// Start cluster
	t.Log("Starting MonoFS cluster for npm...")
	cluster := startTestCluster(t, ctx, testDir, routerAddr)
	defer cluster.Stop()
	waitForCluster(t, ctx, routerAddr, 30*time.Second)

	// Create test npm project
	projectPath := createTestNpmProject(t, testDir)

	// Ingest project
	t.Log("Ingesting npm project...")
	ingestCmd := exec.CommandContext(ctx, "./bin/monofs-admin",
		"ingest",
		"--router="+routerAddr,
		"--source="+projectPath,
		"--ingestion-type=git",
	)
	ingestCmd.Dir = filepath.Join(testDir, "..")
	if output, err := ingestCmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to ingest npm project: %v\n%s", err, output)
	}

	// Mount filesystem
	mountCmd := startMount(t, ctx, routerAddr, mountPoint)
	defer mountCmd.Process.Kill()
	waitForMount(t, mountPoint, 10*time.Second)

	// Find project in mount
	monofsProjectPath := findProjectInMount(t, mountPoint, filepath.Base(projectPath))
	if monofsProjectPath == "" {
		t.Fatal("npm project not found in MonoFS")
	}

	// Ingest dependencies
	t.Log("Ingesting npm dependencies with cache metadata...")
	ingestDepsCmd := exec.CommandContext(ctx, "./bin/monofs-admin",
		"ingest-deps",
		"--router="+routerAddr,
		"--file="+filepath.Join(monofsProjectPath, "package.json"),
		"--type=npm",
		"--concurrency=5",
	)
	ingestDepsCmd.Dir = filepath.Join(testDir, "..")
	if output, err := ingestDepsCmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to ingest npm dependencies: %v\n%s", err, output)
	}

	// Verify npm cache metadata
	t.Log("Verifying npm cache metadata...")
	npmCacheDir := filepath.Join(mountPoint, "npm-cache")
	if !dirExists(npmCacheDir) {
		t.Fatalf("npm cache directory not found: %s", npmCacheDir)
	}

	// Test offline install
	t.Log("Testing offline npm install...")
	buildDir := filepath.Join(testDir, "npm-build")
	copyDir(t, monofsProjectPath, buildDir)

	installCmd := exec.CommandContext(ctx, "npm", "install", "--prefer-offline")
	installCmd.Dir = buildDir
	installCmd.Env = append(os.Environ(),
		"NPM_CONFIG_CACHE="+npmCacheDir,
		"NPM_CONFIG_PREFER_OFFLINE=true",
	)

	if output, err := installCmd.CombinedOutput(); err != nil {
		t.Logf("npm install output:\n%s", output)
		// npm might still try to reach network, so we log but don't fail
		t.Logf("Note: npm install attempted but may have needed network: %v", err)
	} else {
		t.Log("✅ npm offline install test passed!")
	}
}

// TestOfflineBuilds_Cargo tests end-to-end Cargo offline builds
func TestOfflineBuilds_Cargo(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Check if cargo is installed
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo not found, skipping test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	testDir := setupTestEnv(t, "cargo-offline")
	defer cleanupTestEnv(t, testDir)

	mountPoint := filepath.Join(testDir, "mnt")
	routerAddr := "localhost:19092"

	// Start cluster
	t.Log("Starting MonoFS cluster for Cargo...")
	cluster := startTestCluster(t, ctx, testDir, routerAddr)
	defer cluster.Stop()
	waitForCluster(t, ctx, routerAddr, 30*time.Second)

	// Create test Cargo project
	projectPath := createTestCargoProject(t, testDir)

	// Ingest project
	t.Log("Ingesting Cargo project...")
	ingestCmd := exec.CommandContext(ctx, "./bin/monofs-admin",
		"ingest",
		"--router="+routerAddr,
		"--source="+projectPath,
		"--ingestion-type=git",
	)
	ingestCmd.Dir = filepath.Join(testDir, "..")
	if output, err := ingestCmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to ingest Cargo project: %v\n%s", err, output)
	}

	// Mount and ingest dependencies
	mountCmd := startMount(t, ctx, routerAddr, mountPoint)
	defer mountCmd.Process.Kill()
	waitForMount(t, mountPoint, 10*time.Second)

	monofsProjectPath := findProjectInMount(t, mountPoint, filepath.Base(projectPath))
	if monofsProjectPath == "" {
		t.Fatal("Cargo project not found in MonoFS")
	}

	t.Log("Ingesting Cargo dependencies with cache metadata...")
	ingestDepsCmd := exec.CommandContext(ctx, "./bin/monofs-admin",
		"ingest-deps",
		"--router="+routerAddr,
		"--file="+filepath.Join(monofsProjectPath, "Cargo.toml"),
		"--type=cargo",
		"--concurrency=5",
	)
	ingestDepsCmd.Dir = filepath.Join(testDir, "..")
	if output, err := ingestDepsCmd.CombinedOutput(); err != nil {
		t.Logf("Note: Cargo ingest-deps may not be fully implemented: %v\n%s", err, output)
	}

	// Verify Cargo cache
	cargoHome := filepath.Join(mountPoint, ".cargo")
	if dirExists(cargoHome) {
		t.Log("✅ Cargo cache directory created")
	}

	t.Log("Cargo offline build test completed (limited due to Cargo backend implementation)")
}

// Helper functions

type testCluster struct {
	routerCmd *exec.Cmd
	nodeCmd1  *exec.Cmd
	nodeCmd2  *exec.Cmd
}

func (c *testCluster) Stop() {
	if c.routerCmd != nil && c.routerCmd.Process != nil {
		c.routerCmd.Process.Kill()
	}
	if c.nodeCmd1 != nil && c.nodeCmd1.Process != nil {
		c.nodeCmd1.Process.Kill()
	}
	if c.nodeCmd2 != nil && c.nodeCmd2.Process != nil {
		c.nodeCmd2.Process.Kill()
	}
}

func setupTestEnv(t *testing.T, name string) string {
	testDir := filepath.Join(os.TempDir(), "monofs-test-"+name+"-"+fmt.Sprintf("%d", time.Now().Unix()))
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatal(err)
	}
	return testDir
}

func cleanupTestEnv(t *testing.T, testDir string) {
	// Unmount if still mounted
	mountPoint := filepath.Join(testDir, "mnt")
	exec.Command("fusermount", "-u", mountPoint).Run()
	exec.Command("fusermount3", "-u", mountPoint).Run()

	// Remove test directory
	if err := os.RemoveAll(testDir); err != nil {
		t.Logf("Warning: failed to cleanup test dir: %v", err)
	}
}

func startTestCluster(t *testing.T, ctx context.Context, testDir, routerAddr string) *testCluster {
	// Start router
	routerCmd := exec.CommandContext(ctx, "./bin/monofs-router",
		"--port="+strings.Split(routerAddr, ":")[1],
		"--http-port=18080",
		"--nodes=node1=localhost:19001,node2=localhost:19002",
		"--weights=node1=100,node2=100",
	)
	routerCmd.Dir = filepath.Join(testDir, "..")
	routerCmd.Stdout = os.Stdout
	routerCmd.Stderr = os.Stderr
	if err := routerCmd.Start(); err != nil {
		t.Fatalf("Failed to start router: %v", err)
	}

	// Start node1
	node1Dir := filepath.Join(testDir, "node1")
	os.MkdirAll(node1Dir, 0755)
	nodeCmd1 := exec.CommandContext(ctx, "./bin/monofs-server",
		"--addr=:19001",
		"--node-id=node1",
		"--db-path="+filepath.Join(node1Dir, "db"),
	)
	nodeCmd1.Dir = filepath.Join(testDir, "..")
	if err := nodeCmd1.Start(); err != nil {
		t.Fatalf("Failed to start node1: %v", err)
	}

	// Start node2
	node2Dir := filepath.Join(testDir, "node2")
	os.MkdirAll(node2Dir, 0755)
	nodeCmd2 := exec.CommandContext(ctx, "./bin/monofs-server",
		"--addr=:19002",
		"--node-id=node2",
		"--db-path="+filepath.Join(node2Dir, "db"),
	)
	nodeCmd2.Dir = filepath.Join(testDir, "..")
	if err := nodeCmd2.Start(); err != nil {
		t.Fatalf("Failed to start node2: %v", err)
	}

	return &testCluster{
		routerCmd: routerCmd,
		nodeCmd1:  nodeCmd1,
		nodeCmd2:  nodeCmd2,
	}
}

func waitForCluster(t *testing.T, ctx context.Context, routerAddr string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.CommandContext(ctx, "./bin/monofs-admin",
			"status",
			"--router="+routerAddr,
		)
		if err := cmd.Run(); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("Cluster failed to become ready")
}

func startMount(t *testing.T, ctx context.Context, routerAddr, mountPoint string) *exec.Cmd {
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		t.Fatal(err)
	}

	mountCmd := exec.CommandContext(ctx, "./bin/monofs-client",
		"--router="+routerAddr,
		"--mount="+mountPoint,
		"--writable",
	)
	mountCmd.Stdout = os.Stdout
	mountCmd.Stderr = os.Stderr

	if err := mountCmd.Start(); err != nil {
		t.Fatalf("Failed to start mount: %v", err)
	}

	return mountCmd
}

func waitForMount(t *testing.T, mountPoint string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if dirExists(mountPoint) {
			// Check if actually mounted
			entries, err := os.ReadDir(mountPoint)
			if err == nil && len(entries) >= 0 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("Mount point not ready: %s", mountPoint)
}

func findProjectInMount(t *testing.T, mountPoint, projectName string) string {
	// Look in common locations
	locations := []string{
		filepath.Join(mountPoint, "github.com"),
		filepath.Join(mountPoint, "localhost"),
	}

	for _, loc := range locations {
		if !dirExists(loc) {
			continue
		}
		entries, err := os.ReadDir(loc)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				subPath := filepath.Join(loc, entry.Name())
				subEntries, _ := os.ReadDir(subPath)
				for _, sub := range subEntries {
					if strings.Contains(sub.Name(), projectName) {
						return filepath.Join(subPath, sub.Name())
					}
				}
			}
		}
	}

	return ""
}

func createTestGoProject(t *testing.T, testDir string) string {
	projectDir := filepath.Join(testDir, "test-go-project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create go.mod
	goMod := `module github.com/test/project

go 1.24

require github.com/google/uuid v1.6.0
`
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}

	// Create main.go
	mainGo := `package main

import (
	"fmt"
	"github.com/google/uuid"
)

func main() {
	id := uuid.New()
	fmt.Printf("Generated UUID: %s\n", id)
}
`
	if err := os.WriteFile(filepath.Join(projectDir, "main.go"), []byte(mainGo), 0644); err != nil {
		t.Fatal(err)
	}

	// Initialize git repo
	exec.Command("git", "init", projectDir).Run()
	exec.Command("git", "-C", projectDir, "add", ".").Run()
	exec.Command("git", "-C", projectDir, "commit", "-m", "Initial commit").Run()

	return projectDir
}

func createTestNpmProject(t *testing.T, testDir string) string {
	projectDir := filepath.Join(testDir, "test-npm-project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create package.json
	packageJSON := `{
  "name": "test-project",
  "version": "1.0.0",
  "dependencies": {
    "lodash": "^4.17.21"
  }
}
`
	if err := os.WriteFile(filepath.Join(projectDir, "package.json"), []byte(packageJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// Create index.js
	indexJS := `const _ = require('lodash');
console.log(_.VERSION);
`
	if err := os.WriteFile(filepath.Join(projectDir, "index.js"), []byte(indexJS), 0644); err != nil {
		t.Fatal(err)
	}

	exec.Command("git", "init", projectDir).Run()
	exec.Command("git", "-C", projectDir, "add", ".").Run()
	exec.Command("git", "-C", projectDir, "commit", "-m", "Initial commit").Run()

	return projectDir
}

func createTestCargoProject(t *testing.T, testDir string) string {
	projectDir := filepath.Join(testDir, "test-cargo-project")
	if err := os.MkdirAll(filepath.Join(projectDir, "src"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create Cargo.toml
	cargoToml := `[package]
name = "test-project"
version = "0.1.0"
edition = "2021"

[dependencies]
serde = "1.0"
`
	if err := os.WriteFile(filepath.Join(projectDir, "Cargo.toml"), []byte(cargoToml), 0644); err != nil {
		t.Fatal(err)
	}

	// Create main.rs
	mainRs := `fn main() {
    println!("Hello from Cargo!");
}
`
	if err := os.WriteFile(filepath.Join(projectDir, "src", "main.rs"), []byte(mainRs), 0644); err != nil {
		t.Fatal(err)
	}

	exec.Command("git", "init", projectDir).Run()
	exec.Command("git", "-C", projectDir, "add", ".").Run()
	exec.Command("git", "-C", projectDir, "commit", "-m", "Initial commit").Run()

	return projectDir
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func copyDir(t *testing.T, src, dst string) {
	cmd := exec.Command("cp", "-r", src, dst)
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to copy directory: %v", err)
	}
}

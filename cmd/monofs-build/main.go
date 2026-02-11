// MonoFS Build - Wrapper for running single build commands with MonoFS cache.
// For build environment setup, use: eval $(monofs-session setup .)
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const version = "1.1.0"

// BuildBackend describes how to run a specific build system with MonoFS
type BuildBackend struct {
	Name        string
	DetectFiles []string
	CachePath   string
	Command     string
	SetupEnv    func(mountPoint string) []string
}

var backends = []BuildBackend{
	{Name: "go", DetectFiles: []string{"go.mod", "go.sum"}, CachePath: "go-modules/pkg/mod", Command: "go",
		SetupEnv: func(mp string) []string {
			return []string{"GOMODCACHE=" + filepath.Join(mp, "go-modules/pkg/mod"), "GONOSUMDB=*", "GONOSUMCHECK=*", "GOFLAGS=-mod=mod"}
		}},
	{Name: "bazel", DetectFiles: []string{"MODULE.bazel", "WORKSPACE", "WORKSPACE.bazel"}, CachePath: "bazel-repos", Command: "bazel",
		SetupEnv: func(mp string) []string {
			return []string{"BAZEL_REPOSITORY_CACHE=" + filepath.Join(mp, "bazel-repos")}
		}},
	{Name: "npm", DetectFiles: []string{"package.json", "package-lock.json"}, CachePath: "npm-cache", Command: "npm",
		SetupEnv: func(mp string) []string {
			return []string{"NPM_CONFIG_CACHE=" + filepath.Join(mp, "npm-cache"), "NPM_CONFIG_PREFER_OFFLINE=true"}
		}},
	{Name: "maven", DetectFiles: []string{"pom.xml"}, CachePath: "maven-repo", Command: "mvn",
		SetupEnv: func(mp string) []string {
			return []string{"MAVEN_REPO=" + filepath.Join(mp, "maven-repo")}
		}},
	{Name: "cargo", DetectFiles: []string{"Cargo.toml", "Cargo.lock"}, CachePath: "cargo-home", Command: "cargo",
		SetupEnv: func(mp string) []string {
			return []string{"CARGO_HOME=" + filepath.Join(mp, "cargo-home"), "CARGO_NET_OFFLINE=true"}
		}},
	{Name: "pip", DetectFiles: []string{"requirements.txt", "pyproject.toml", "setup.py"}, CachePath: "pip-cache", Command: "pip",
		SetupEnv: func(mp string) []string {
			return []string{"PIP_CACHE_DIR=" + filepath.Join(mp, "pip-cache"), "PIP_NO_INDEX=true"}
		}},
}

func detectMountPoint() string {
	if mp := os.Getenv("MONOFS_MOUNT"); mp != "" {
		return mp
	}
	for _, c := range []string{"/mnt", "/mnt/monofs"} {
		if entries, err := os.ReadDir(c); err == nil && len(entries) > 0 {
			return c
		}
	}
	return "/mnt"
}

func detectBuildSystem() *BuildBackend {
	for i := range backends {
		for _, f := range backends[i].DetectFiles {
			if _, err := os.Stat(f); err == nil {
				return &backends[i]
			}
		}
	}
	return nil
}

func main() {
	mountFlag := flag.String("mount", "", "MonoFS mount point (default: auto-detect)")
	dryRun := flag.Bool("dry-run", false, "Show what would be executed without running")
	showVersion := flag.Bool("version", false, "Show version")
	help := flag.Bool("help", false, "Show help")

	// Parse flags up to first non-flag argument or "--"
	args := os.Args[1:]
	flagEnd := len(args)
	for i, arg := range args {
		if arg == "--" || (!strings.HasPrefix(arg, "-") && arg != "help") {
			flagEnd = i
			break
		}
	}
	flag.CommandLine.Parse(args[:flagEnd])
	remaining := args[flagEnd:]

	if *showVersion {
		fmt.Printf("monofs-build version %s\n", version)
		return
	}

	// Redirect setup to monofs-session
	if len(remaining) > 0 && remaining[0] == "setup" {
		fmt.Fprintln(os.Stderr, "Note: 'setup' has moved to monofs-session.")
		fmt.Fprintln(os.Stderr, "Use:  eval $(monofs-session setup .)")
		// Still work — exec monofs-session setup with same args
		cmdArgs := append([]string{"setup"}, remaining[1:]...)
		cmd := exec.Command("monofs-session", cmdArgs...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			os.Exit(1)
		}
		return
	}

	if *help || len(remaining) == 0 {
		printUsage()
		return
	}

	mountPoint := *mountFlag
	if mountPoint == "" {
		mountPoint = detectMountPoint()
	}

	// Find backend
	var backend *BuildBackend
	var buildArgs []string

	for i := range backends {
		if backends[i].Name == remaining[0] {
			backend = &backends[i]
			// Find -- separator
			for j := 1; j < len(remaining); j++ {
				if remaining[j] == "--" && j+1 < len(remaining) {
					buildArgs = remaining[j+1:]
					break
				}
			}
			if buildArgs == nil {
				buildArgs = remaining[1:]
			}
			break
		}
	}

	if backend == nil {
		backend = detectBuildSystem()
		if backend == nil {
			fmt.Fprintln(os.Stderr, "Error: could not detect build system. Specify type explicitly.")
			fmt.Fprintln(os.Stderr, "  monofs-build go -- build ./...")
			os.Exit(1)
		}
		buildArgs = remaining
		fmt.Fprintf(os.Stderr, "Auto-detected: %s\n", backend.Name)
	}

	if _, err := os.Stat(mountPoint); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: MonoFS not mounted at %s\n", mountPoint)
		os.Exit(1)
	}

	envVars := backend.SetupEnv(mountPoint)

	if *dryRun {
		fmt.Printf("Build System: %s\nMount: %s\n", backend.Name, mountPoint)
		for _, env := range envVars {
			fmt.Printf("  %s\n", env)
		}
		fmt.Printf("Command: %s %s\n", backend.Command, strings.Join(buildArgs, " "))
		return
	}

	cmd := exec.Command(backend.Command, buildArgs...)
	cmd.Env = append(os.Environ(), envVars...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Build failed: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf(`MonoFS Build v%s - Run build commands with MonoFS cache

For environment setup, use monofs-session:
  eval $(monofs-session setup .)     # auto-detects ALL build systems
  go build ./...                     # then use native commands!

This tool runs a single command with MonoFS env vars injected:
  monofs-build go -- build ./...
  monofs-build npm -- install
  monofs-build cargo -- build --release

Options:
  --mount=PATH   MonoFS mount point (default: auto-detect or MONOFS_MOUNT)
  --dry-run      Show env vars and command without running
  --version      Show version
`, version)
}

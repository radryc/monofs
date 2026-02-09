// MonoFS Build - Universal wrapper for running builds with MonoFS cache
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const version = "1.0.0"

// BuildBackend describes how to run a specific build system with MonoFS
type BuildBackend struct {
	Name        string                           // go, bazel, npm, maven, cargo, pip
	DetectFiles []string                         // Files that indicate this build system (go.mod, package.json, etc.)
	CachePath   string                           // Path under mount point (go-modules/pkg/mod, npm-cache, etc.)
	Command     string                           // Default command (go, bazel, npm, mvn, cargo, pip)
	SetupEnv    func(mountPoint string) []string // Custom env setup function
}

var backends = []BuildBackend{
	{
		Name:        "go",
		DetectFiles: []string{"go.mod", "go.sum"},
		CachePath:   "go-modules/pkg/mod",
		Command:     "go",
		SetupEnv: func(mountPoint string) []string {
			gomodcache := filepath.Join(mountPoint, "go-modules/pkg/mod")
			return []string{
				"GOMODCACHE=" + gomodcache,
				"GONOSUMDB=*",
				"GONOSUMCHECK=*",
				"GOFLAGS=-mod=mod",
			}
		},
	},
	{
		Name:        "bazel",
		DetectFiles: []string{"MODULE.bazel", "WORKSPACE", "WORKSPACE.bazel", "BUILD.bazel", "BUILD"},
		CachePath:   "bazel-repos",
		Command:     "bazel",
		SetupEnv: func(mountPoint string) []string {
			return []string{
				"BAZEL_REPOSITORY_CACHE=" + filepath.Join(mountPoint, "bazel-repos"),
			}
		},
	},
	{
		Name:        "npm",
		DetectFiles: []string{"package.json", "package-lock.json"},
		CachePath:   "npm-cache",
		Command:     "npm",
		SetupEnv: func(mountPoint string) []string {
			return []string{
				"NPM_CONFIG_CACHE=" + filepath.Join(mountPoint, "npm-cache"),
				"NPM_CONFIG_PREFER_OFFLINE=true",
			}
		},
	},
	{
		Name:        "maven",
		DetectFiles: []string{"pom.xml"},
		CachePath:   "maven-repo",
		Command:     "mvn",
		SetupEnv: func(mountPoint string) []string {
			return []string{
				"MAVEN_REPO=" + filepath.Join(mountPoint, "maven-repo"),
			}
		},
	},
	{
		Name:        "cargo",
		DetectFiles: []string{"Cargo.toml", "Cargo.lock"},
		CachePath:   "cargo-home",
		Command:     "cargo",
		SetupEnv: func(mountPoint string) []string {
			return []string{
				"CARGO_HOME=" + filepath.Join(mountPoint, "cargo-home"),
				"CARGO_NET_OFFLINE=true",
			}
		},
	},
	{
		Name:        "pip",
		DetectFiles: []string{"requirements.txt", "pyproject.toml", "setup.py"},
		CachePath:   "pip-cache",
		Command:     "pip",
		SetupEnv: func(mountPoint string) []string {
			return []string{
				"PIP_CACHE_DIR=" + filepath.Join(mountPoint, "pip-cache"),
				"PIP_NO_INDEX=true",
			}
		},
	},
}

func main() {
	// Global flags
	var (
		mountPoint  = flag.String("mount", "/mnt/monofs", "MonoFS mount point")
		dryRun      = flag.Bool("dry-run", false, "Show what would be executed without running")
		showVersion = flag.Bool("version", false, "Show version")
		help        = flag.Bool("help", false, "Show help")
	)

	// Parse flags up to first non-flag argument or "--"
	args := os.Args[1:]
	flagEndIdx := len(args)
	for i, arg := range args {
		if arg == "--" || (!strings.HasPrefix(arg, "-") && arg != "help") {
			flagEndIdx = i
			break
		}
	}

	flag.CommandLine.Parse(args[:flagEndIdx])
	remainingArgs := args[flagEndIdx:]

	if *showVersion {
		fmt.Printf("monofs-build version %s\n", version)
		return
	}

	if *help || len(remainingArgs) == 0 {
		printUsage()
		return
	}

	// Determine build type and command
	var backend *BuildBackend
	var buildArgs []string

	// Check if first arg is a known backend type
	if len(remainingArgs) > 0 {
		for i := range backends {
			if backends[i].Name == remainingArgs[0] {
				backend = &backends[i]
				// Look for -- separator
				dashIdx := -1
				for j := 1; j < len(remainingArgs); j++ {
					if remainingArgs[j] == "--" {
						dashIdx = j
						break
					}
				}
				if dashIdx >= 0 && dashIdx+1 < len(remainingArgs) {
					buildArgs = remainingArgs[dashIdx+1:]
				} else {
					buildArgs = remainingArgs[1:]
				}
				break
			}
		}
	}

	// Auto-detect if not explicitly specified
	if backend == nil {
		detected := detectBuildSystem()
		if detected == nil {
			fmt.Println("Error: Could not detect build system in current directory.")
			fmt.Println("\nSupported build systems: go, bazel, npm, maven, cargo, pip")
			fmt.Println("\nEither:")
			fmt.Println("  1. Run from a directory with build files (go.mod, package.json, etc.)")
			fmt.Println("  2. Specify build type explicitly: monofs-build <type> -- <command>")
			os.Exit(1)
		}
		backend = detected
		buildArgs = remainingArgs
		fmt.Printf("Auto-detected build system: %s\n", backend.Name)
	}

	// Verify mount point exists
	if _, err := os.Stat(*mountPoint); os.IsNotExist(err) {
		fmt.Printf("Error: MonoFS not mounted at %s\n", *mountPoint)
		fmt.Println("\nMake sure to mount MonoFS first:")
		fmt.Printf("  monofs-client --mount=%s --router=<router-addr>\n", *mountPoint)
		os.Exit(1)
	}

	// Verify cache path exists
	cachePath := filepath.Join(*mountPoint, backend.CachePath)
	if _, err := os.Stat(cachePath); os.IsNotExist(err) {
		fmt.Printf("Error: Cache directory not found at %s\n", cachePath)
		fmt.Printf("\nMake sure dependencies are ingested for %s:\n", backend.Name)
		fmt.Println("  monofs-admin ingest-deps --file=<manifest> --type=" + backend.Name)
		os.Exit(1)
	}

	// Setup environment
	envVars := backend.SetupEnv(*mountPoint)

	if *dryRun {
		fmt.Printf("\n=== Dry Run Mode ===\n")
		fmt.Printf("Build System: %s\n", backend.Name)
		fmt.Printf("Mount Point:  %s\n", *mountPoint)
		fmt.Printf("Cache Path:   %s\n", cachePath)
		fmt.Printf("\nEnvironment Variables:\n")
		for _, env := range envVars {
			fmt.Printf("  %s\n", env)
		}
		fmt.Printf("\nCommand: %s %s\n", backend.Command, strings.Join(buildArgs, " "))
		return
	}

	// Execute build command
	if err := runBuild(backend.Command, buildArgs, envVars); err != nil {
		fmt.Printf("Build failed: %v\n", err)
		os.Exit(1)
	}
}

// detectBuildSystem scans current directory for build files
func detectBuildSystem() *BuildBackend {
	for i := range backends {
		for _, file := range backends[i].DetectFiles {
			if _, err := os.Stat(file); err == nil {
				return &backends[i]
			}
		}
	}
	return nil
}

// runBuild executes the build command with MonoFS environment
func runBuild(command string, args []string, envVars []string) error {
	cmd := exec.Command(command, args...)
	cmd.Env = append(os.Environ(), envVars...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func printUsage() {
	fmt.Printf(`MonoFS Build - Universal wrapper for builds with MonoFS cache (v%s)

USAGE:
  monofs-build [options] <command...>                  # Auto-detect build system
  monofs-build [options] <type> -- <command...>        # Explicit build system

OPTIONS:
  --mount=PATH     MonoFS mount point (default: /mnt/monofs)
  --dry-run        Show what would be executed without running
  --version        Show version
  --help           Show this help

SUPPORTED BUILD SYSTEMS:
  go       Go modules       (GOMODCACHE)
  bazel    Bazel            (BAZEL_REPOSITORY_CACHE)
  npm      npm/Node.js      (NPM_CONFIG_CACHE)
  maven    Maven            (MAVEN_REPO)
  cargo    Cargo/Rust       (CARGO_HOME)
  pip      Python pip       (PIP_CACHE_DIR)

EXAMPLES:
  # Auto-detect from current directory
  monofs-build build ./...
  monofs-build test
  monofs-build install

  # Explicit build system
  monofs-build go -- build -v ./...
  monofs-build npm -- install
  monofs-build cargo -- build --release
  monofs-build maven -- clean install

  # Custom mount point
  monofs-build --mount=/tmp/monofs go -- test ./...

  # Dry run to see environment
  monofs-build --dry-run go -- build ./...

WORKFLOW:
  1. Mount MonoFS:
     monofs-client --mount=/mnt/monofs --router=localhost:9090

  2. Ingest dependencies:
     monofs-admin ingest-deps --file=go.mod --type=go

  3. Run build:
     monofs-build build ./...

ENVIRONMENT VARIABLES:
  The tool automatically sets the appropriate cache/module environment
  variables for each build system. See --dry-run for exact variables.

`, version)
}

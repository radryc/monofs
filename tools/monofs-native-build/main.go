// monofs-native-build is a wrapper that runs the native build system
// for repos that haven't been migrated to Bazel yet. It is invoked by
// generated genrules in BUILD.bazel files for repos in native/partial state.
//
// Usage:
//
//	monofs-native-build <display_path> <action> <output_file>
//
// Examples:
//
//	monofs-native-build sre/legacy build build.done
//	monofs-native-build sre/legacy test test.done
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintf(os.Stderr, "usage: monofs-native-build <display_path> <action> <output_file>\n")
		os.Exit(1)
	}

	displayPath := os.Args[1]
	action := os.Args[2]
	outputFile := os.Args[3]

	// The repo directory is relative to the mount root.
	// At build time, Bazel's working directory is the workspace root
	// (the mount point), so the display_path is a direct subdirectory.
	repoDir := filepath.Join(".", displayPath)

	buildSystem := detectBuildSystem(repoDir)
	cmd := buildCommand(buildSystem, action)

	fmt.Printf("monofs-native-build: %s %s (detected: %s)\n", displayPath, action, buildSystem)

	c := exec.Command(cmd[0], cmd[1:]...)
	c.Dir = repoDir
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr

	if err := c.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Write output marker file so Bazel knows the genrule succeeded.
	if err := os.WriteFile(outputFile, []byte("ok\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing output marker: %v\n", err)
		os.Exit(1)
	}
}

func detectBuildSystem(repoDir string) string {
	markers := []struct {
		file   string
		system string
	}{
		{"Makefile", "make"},
		{"go.mod", "go"},
		{"package.json", "npm"},
		{"Cargo.toml", "cargo"},
		{"pom.xml", "maven"},
		{"build.gradle", "gradle"},
	}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(repoDir, m.file)); err == nil {
			return m.system
		}
	}
	return "make"
}

func buildCommand(system, action string) []string {
	switch system {
	case "make":
		return []string{"make", action}
	case "go":
		switch action {
		case "build":
			return []string{"go", "build", "./..."}
		case "test":
			return []string{"go", "test", "./..."}
		default:
			return []string{"go", action, "./..."}
		}
	case "npm":
		return []string{"npm", "run", action}
	case "cargo":
		return []string{"cargo", action}
	case "maven":
		return []string{"mvn", action}
	case "gradle":
		return []string{"gradle", action}
	default:
		// Shell out action directly.
		return strings.Fields(action)
	}
}

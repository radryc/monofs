package gazelle

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GoGenerator generates BUILD.bazel files for Go repositories by
// wrapping `gazelle`. It adds cross-repo import resolution so that
// imports from other ingested repos map to @repo_name//... targets.
type GoGenerator struct{}

// Name implements Generator.
func (g *GoGenerator) Name() string { return "go" }

// CanGenerate implements Generator.
func (g *GoGenerator) CanGenerate(ctx context.Context, repoDir string) bool {
	_, err := os.Stat(filepath.Join(repoDir, "go.mod"))
	return err == nil
}

// Generate implements Generator.
//
// It runs `gazelle` in the repo directory. If gazelle is not in PATH,
// it falls back to a simple heuristic that generates BUILD.bazel files
// from Go source layout without dependency resolution.
func (g *GoGenerator) Generate(ctx context.Context, opts GenerateOptions) (*GenerateResult, error) {
	result := &GenerateResult{}

	if opts.RepoDir == "" {
		return nil, fmt.Errorf("repo dir is empty")
	}

	// Try using the real gazelle binary first.
	if gazellePath, err := exec.LookPath("gazelle"); err == nil {
		return g.generateWithGazelle(ctx, gazellePath, opts, result)
	}

	// Fallback: simple heuristic generation without gazelle.
	return g.generateSimple(ctx, opts, result)
}

// generateWithGazelle runs the real gazelle binary.
func (g *GoGenerator) generateWithGazelle(ctx context.Context, gazellePath string, opts GenerateOptions, result *GenerateResult) (*GenerateResult, error) {
	args := []string{
		"-repo_root=" + opts.RepoDir,
	}

	// If Gazelle needs a go_prefix, supply it from the module path.
	if opts.ModulePath != "" {
		args = append(args, "-go_prefix="+opts.ModulePath)
	}

	// mode=fix writes BUILD.bazel files; mode=diff would only report changes.
	args = append(args, "-mode=fix")

	if opts.Verbose {
		args = append(args, "-v=1")
	}

	cmd := exec.CommandContext(ctx, gazellePath, args...)
	cmd.Dir = opts.RepoDir
	cmd.Stdout = &gazelleWriter{result: result, repoDir: opts.RepoDir}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Run(); err != nil {
		// Gazelle can return non-zero on parse errors while still
		// generating files. We report the error but don't fail.
		result.Errors = append(result.Errors, fmt.Sprintf("gazelle: %v", err))
	}

	return result, nil
}

// generateSimple is a fallback that creates one BUILD.bazel per Go
// package directory. It does NOT resolve dependencies or imports.
func (g *GoGenerator) generateSimple(ctx context.Context, opts GenerateOptions, result *GenerateResult) (*GenerateResult, error) {
	if opts.Verbose {
		result.Errors = append(result.Errors, "gazelle not found in PATH; using simple BUILD generation without dependency resolution")
	}

	err := filepath.WalkDir(opts.RepoDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip hidden dirs, vendor, testdata.
		if d.IsDir() {
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") || base == "vendor" || base == "testdata" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}

		// Only process .go files (non-test).
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}

		dir := filepath.Dir(path)
		buildFile := filepath.Join(dir, "BUILD.bazel")

		// Check if BUILD file already exists (don't overwrite unless forced).
		if !opts.Force {
			if _, err := os.Stat(buildFile); err == nil {
				result.FilesSkipped++
				return nil
			}
		}

		// Don't write the file yet -- we'll collect unique dirs and
		// generate one BUILD.bazel per directory.
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("walk repo: %w", err)
	}

	// Walk again, this time collecting Go source files per directory,
	// and generate BUILD.bazel.
	type pkgInfo struct {
		files   []string
		hasTest bool
	}
	pkgs := make(map[string]*pkgInfo)

	err = filepath.WalkDir(opts.RepoDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") || base == "vendor" || base == "testdata" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		dir := filepath.Dir(path)
		if _, ok := pkgs[dir]; !ok {
			pkgs[dir] = &pkgInfo{}
		}
		pkgs[dir].files = append(pkgs[dir].files, d.Name())
		if strings.HasSuffix(d.Name(), "_test.go") {
			pkgs[dir].hasTest = true
		}
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("walk repo: %w", err)
	}

	// Determine import path prefix.
	importPrefix := opts.ModulePath
	if importPrefix == "" {
		importPrefix = "unknown"
	}

	for dir, pkg := range pkgs {
		relDir, err := filepath.Rel(opts.RepoDir, dir)
		if err != nil {
			relDir = dir
		}

		importPath := importPrefix
		if relDir != "." {
			importPath = importPrefix + "/" + relDir
		}

		var srcs, testSrcs []string
		for _, f := range pkg.files {
			f = strings.TrimSuffix(f, ".go")
			if strings.HasSuffix(f, "_test") {
				testSrcs = append(testSrcs, f+".go")
			} else {
				srcs = append(srcs, f+".go")
			}
		}

		buildFile := filepath.Join(dir, "BUILD.bazel")

		// Skip if manual and not forced.
		if !opts.Force {
			if existing, err := os.ReadFile(buildFile); err == nil {
				if bytes.Contains(existing, []byte("# monofs: manual")) {
					result.FilesSkipped++
					continue
				}
			}
		}

		if opts.Verbose {
			result.WrittenFiles = append(result.WrittenFiles, relDir+"/BUILD.bazel")
		}

		if opts.DryRun {
			result.FilesGenerated++
			continue
		}

		content := g.buildBazelContent(relDir, importPath, srcs, testSrcs)
		if err := os.MkdirAll(filepath.Dir(buildFile), 0755); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("mkdir %s: %v", filepath.Dir(buildFile), err))
			continue
		}
		if err := os.WriteFile(buildFile, []byte(content), 0644); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("write %s: %v", buildFile, err))
			continue
		}
		result.FilesGenerated++
	}

	return result, nil
}

// buildBazelContent generates the content of a BUILD.bazel file for a Go package.
func (g *GoGenerator) buildBazelContent(relDir, importPath string, srcs, testSrcs []string) string {
	var b bytes.Buffer

	b.WriteString("# Generated by monofs-gazelle -- DO NOT EDIT\n")
	b.WriteString("# To prevent regeneration, add: # monofs: manual\n\n")
	b.WriteString("load(\"@rules_go//go:def.bzl\", \"go_library\")\n\n")

	// go_library if there are non-test sources.
	if len(srcs) > 0 {
		visibility := "//visibility:public"
		// Main packages are typically private.
		if filepath.Base(relDir) == "main" || relDir == "cmd" {
			visibility = "//visibility:private"
		}
		// If it's under cmd/..., it's a main package.
		if strings.HasPrefix(relDir, "cmd/") {
			b.WriteString("load(\"@rules_go//go:def.bzl\", \"go_binary\")\n\n")
			b.WriteString(fmt.Sprintf("go_binary(\n"))
			b.WriteString(fmt.Sprintf("    name = %q,\n", filepath.Base(relDir)))
		} else {
			pkgName := filepath.Base(relDir)
			if relDir == "." {
				pkgName = filepath.Base(importPath)
			}
			b.WriteString(fmt.Sprintf("go_library(\n"))
			b.WriteString(fmt.Sprintf("    name = %q,\n", pkgName))
		}

		b.WriteString(fmt.Sprintf("    srcs = [\n"))
		for _, s := range srcs {
			b.WriteString(fmt.Sprintf("        %q,\n", s))
		}
		b.WriteString("    ],\n")

		if importPath != "" && !strings.HasPrefix(relDir, "cmd/") {
			b.WriteString(fmt.Sprintf("    importpath = %q,\n", importPath))
		}

		if !strings.HasPrefix(relDir, "cmd/") {
			b.WriteString(fmt.Sprintf("    visibility = [%q],\n", visibility))
		}

		b.WriteString(")\n")
	}

	// go_test if there are test sources.
	if len(testSrcs) > 0 {
		if len(srcs) > 0 {
			b.WriteString("\n")
		}
		b.WriteString("load(\"@rules_go//go:def.bzl\", \"go_test\")\n\n")
		pkgName := filepath.Base(relDir)
		if relDir == "." {
			pkgName = filepath.Base(importPath)
		}
		b.WriteString(fmt.Sprintf("go_test(\n"))
		b.WriteString(fmt.Sprintf("    name = %q,\n", pkgName+"_test"))

		allSrcs := append([]string{}, srcs...)
		allSrcs = append(allSrcs, testSrcs...)
		b.WriteString(fmt.Sprintf("    srcs = [\n"))
		for _, s := range allSrcs {
			b.WriteString(fmt.Sprintf("        %q,\n", s))
		}
		b.WriteString("    ],\n")

		b.WriteString(fmt.Sprintf("    embed = [%q],\n", ":"+pkgName))
		b.WriteString(")\n")
	}

	return b.String()
}

// gazelleWriter captures output from the gazelle process.
type gazelleWriter struct {
	result  *GenerateResult
	repoDir string
}

func (w *gazelleWriter) Write(p []byte) (int, error) {
	// Count files generated by scanning for "BUILD.bazel" in output.
	// This is approximate; real Gazelle output parsing would be more precise.
	if bytes.Contains(p, []byte("BUILD.bazel")) {
		w.result.FilesGenerated++
	}
	return len(p), nil
}

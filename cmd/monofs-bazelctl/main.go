// monofs-bazelctl manages Bazel integration for MonoFS virtual monorepos.
//
// Commands:
//
//	sync        Generate synthetic WORKSPACE, MODULE.bazel, .bazelrc, etc.
//	generate    Generate BUILD.bazel files for ingested repos (via Gazelle).
//	status      Show workspace Bazel status (adoption state, per-repo health).
//	deps        Dependency graph operations (graph, tree, affected).
//	update-deps Update cross-repo dependency lockfile.
//	promote     Promote a repo's Bazel adoption state.
//	demote      Demote a repo's Bazel adoption state.
//	validate    Validate generated BUILD files.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/radryc/monofs/internal/bazel"
	"github.com/radryc/monofs/internal/bazel/deps"
	"github.com/radryc/monofs/internal/bazel/gazelle"
	"github.com/radryc/monofs/internal/bazel/migration"
)

// Version information (injected at build time).
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "monofs-bazelctl %s (commit: %s, built: %s)\n", Version, Commit, BuildTime)
		fmt.Fprintf(os.Stderr, "\nUsage:\n")
		fmt.Fprintf(os.Stderr, "  monofs-bazelctl sync [flags]\n")
		fmt.Fprintf(os.Stderr, "  monofs-bazelctl generate [flags]\n")
		fmt.Fprintf(os.Stderr, "  monofs-bazelctl status [flags]\n")
		fmt.Fprintf(os.Stderr, "  monofs-bazelctl deps <graph|tree|affected> [flags]\n")
		fmt.Fprintf(os.Stderr, "  monofs-bazelctl update-deps [flags]\n")
		fmt.Fprintf(os.Stderr, "  monofs-bazelctl promote [flags]\n")
		fmt.Fprintf(os.Stderr, "  monofs-bazelctl demote [flags]\n")
		fmt.Fprintf(os.Stderr, "  monofs-bazelctl validate [flags]\n")
		fmt.Fprintf(os.Stderr, "  monofs-bazelctl version\n")
		fmt.Fprintf(os.Stderr, "\nFlags:\n")
		flag.PrintDefaults()
	}

	if len(os.Args) < 2 {
		flag.Usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "version":
		fmt.Printf("monofs-bazelctl version=%s commit=%s build_time=%s\n", Version, Commit, BuildTime)
	case "sync":
		runSync()
	case "generate":
		runGenerate()
	case "deps":
		runDeps()
	case "update-deps":
		runUpdateDeps()
	case "promote":
		runPromote()
	case "demote":
		runDemote()
	case "status":
		runStatus()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		flag.Usage()
		os.Exit(1)
	}
}

func runSync() {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	mountRoot := fs.String("mount", "", "Mount root path (required)")
	manifestPath := fs.String("manifest", "", "Path to workspace.json (if not at <mount>/.monofs/workspace.json)")
	bazelVersion := fs.String("bazel-version", bazel.DefaultBazelVersion, "Bazel version to pin in .bazelversion")
	cacheAddr := fs.String("cache-addr", "", "monofs-cache address (enables remote cache config)")
	executorAddr := fs.String("executor-addr", "", "monofs-executor address (enables remote executor config)")
	fs.Parse(os.Args[2:])

	if *mountRoot == "" {
		fmt.Fprintln(os.Stderr, "error: --mount is required")
		fs.Usage()
		os.Exit(1)
	}

	mp := *manifestPath
	if mp == "" {
		mp = *mountRoot + "/.monofs/workspace.json"
	}

	manifest, err := bazel.LoadManifest(mp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading manifest: %v\n", err)
		os.Exit(1)
	}

	gen := &bazel.Generator{
		MountRoot:    *mountRoot,
		BazelVersion: *bazelVersion,
	}
	if *cacheAddr != "" {
		gen.CacheEnabled = true
		gen.CacheAddr = *cacheAddr
	}
	if *executorAddr != "" {
		gen.ExecutorEnabled = true
		gen.ExecutorAddr = *executorAddr
	}

	ctx := context.Background()
	written, err := gen.WriteGeneratedFiles(ctx, manifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error generating files: %v\n", err)
		os.Exit(1)
	}

	for _, name := range written {
		fmt.Printf("  wrote %s\n", name)
	}
	fmt.Printf("Synced %d repos to %s\n", len(manifest.IncludedRepos()), *mountRoot)
}

func runGenerate() {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	repoDir := fs.String("repo", "", "Repo directory path (required)")
	mountRoot := fs.String("mount", "", "Mount root path (optional, enables cross-repo dep resolution)")
	force := fs.Bool("force", false, "Overwrite existing BUILD files, even with manual markers")
	dryRun := fs.Bool("dry-run", false, "Preview generation without writing files")
	verbose := fs.Bool("verbose", false, "Verbose output")
	fs.Parse(os.Args[2:])

	if *repoDir == "" {
		fmt.Fprintln(os.Stderr, "error: --repo is required")
		fs.Usage()
		os.Exit(1)
	}

	reg := gazelle.NewRegistry()
	opts := gazelle.GenerateOptions{
		Force:   *force,
		DryRun:  *dryRun,
		Verbose: *verbose,
	}

	// Wire resolver from workspace manifest + lockfile if mount is provided.
	if *mountRoot != "" {
		manifestPath := filepath.Join(*mountRoot, ".monofs", "workspace.json")
		if manifest, err := bazel.LoadManifest(manifestPath); err == nil {
			opts.WorkspaceManifest = manifest
			lockfilePath := filepath.Join(*mountRoot, ".monofs", "workspace.lock")
			if lf, err := deps.LoadLockfile(lockfilePath); err == nil {
				opts.Resolver = deps.NewImportResolver(manifest, lf)
			}
		}
	}

	ctx := context.Background()
	result, err := gazelle.GenerateRepo(ctx, reg, *repoDir, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Generated: %d BUILD files\n", result.FilesGenerated)
	if result.FilesSkipped > 0 {
		fmt.Printf("Skipped:    %d (manual marker or already exists)\n", result.FilesSkipped)
	}
	for _, errStr := range result.Errors {
		fmt.Printf("Warning:    %s\n", errStr)
	}
	if *verbose {
		for _, f := range result.WrittenFiles {
			fmt.Printf("  %s\n", f)
		}
	}
}

func runDeps() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: monofs-bazelctl deps <graph|tree|affected> [--mount=<path>]")
		os.Exit(1)
	}

	sub := os.Args[2]
	fs := flag.NewFlagSet("deps "+sub, flag.ExitOnError)
	mountRoot := fs.String("mount", "", "Mount root path (required)")
	repo := fs.String("repo", "", "Repo display_path (for tree/affected)")
	fs.Parse(os.Args[3:])

	if *mountRoot == "" {
		fmt.Fprintln(os.Stderr, "error: --mount is required")
		os.Exit(1)
	}

	lockfilePath := filepath.Join(*mountRoot, ".monofs", "workspace.lock")
	lf, err := deps.LoadLockfile(lockfilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading lockfile: %v\n", err)
		os.Exit(1)
	}

	switch sub {
	case "graph":
		fmt.Printf("Dependency graph (%d repos):\n\n", len(lf.Repositories))
		for _, path := range lf.RepoPaths() {
			r := lf.GetRepo(path)
			deps := lf.GetDeps(path)
			if len(deps) == 0 {
				fmt.Printf("  %s  →  (none)\n", path)
			} else {
				for _, dep := range deps {
					depCommit := ""
					if r != nil {
						if d, ok := r.Dependencies[dep]; ok {
							c := d.Commit
							if len(c) > 8 {
								c = c[:8]
							}
							depCommit = " @" + c
						}
					}
					fmt.Printf("  %s  →  %s%s\n", path, dep, depCommit)
				}
			}
		}
		fmt.Println()

	case "tree":
		if *repo == "" {
			fmt.Fprintln(os.Stderr, "error: --repo is required for 'deps tree'")
			os.Exit(1)
		}
		printDepTree(lf, *repo, "", make(map[string]bool))

	case "affected":
		if *repo == "" {
			fmt.Fprintln(os.Stderr, "error: --repo is required for 'deps affected'")
			os.Exit(1)
		}
		rev := lf.GetReverseDeps(*repo)
		if len(rev) == 0 {
			fmt.Printf("No repos depend on %s\n", *repo)
		} else {
			fmt.Printf("Repos affected by changes to %s:\n", *repo)
			for _, r := range rev {
				fmt.Printf("  %s\n", r)
			}
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown deps subcommand: %s (use graph, tree, or affected)\n", sub)
		os.Exit(1)
	}
}

func printDepTree(lf *deps.WorkspaceLockfile, path, indent string, visited map[string]bool) {
	if visited[path] {
		fmt.Printf("%s%s (cycle)\n", indent, path)
		return
	}
	visited[path] = true
	fmt.Printf("%s%s\n", indent, path)
	for _, dep := range lf.GetDeps(path) {
		printDepTree(lf, dep, indent+"  ", visited)
	}
	visited[path] = false
}

func runUpdateDeps() {
	fs := flag.NewFlagSet("update-deps", flag.ExitOnError)
	mountRoot := fs.String("mount", "", "Mount root path (required)")
	repoDir := fs.String("repo", "", "Update deps for a specific repo only")
	allRepos := fs.Bool("all", false, "Update deps for all repos")
	fs.Parse(os.Args[2:])

	if *mountRoot == "" {
		fmt.Fprintln(os.Stderr, "error: --mount is required")
		os.Exit(1)
	}

	manifestPath := filepath.Join(*mountRoot, ".monofs", "workspace.json")
	manifest, err := bazel.LoadManifest(manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading manifest: %v\n", err)
		os.Exit(1)
	}

	lockfilePath := filepath.Join(*mountRoot, ".monofs", "workspace.lock")
	lf, err := deps.LoadLockfile(lockfilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading lockfile: %v\n", err)
		os.Exit(1)
	}

	discoverer := deps.NewDepDiscoverer(manifest)
	ctx := context.Background()

	updateRepo := func(displayPath string) {
		repoAbsPath := filepath.Join(*mountRoot, displayPath)
		discovered, err := discoverer.DiscoverAll(ctx, repoAbsPath, displayPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: discovering deps for %s: %v\n", displayPath, err)
			return
		}

		// Add each discovered dep to the lockfile.
		for _, d := range discovered {
			lf.AddDep(displayPath, d.DisplayPath, "latest")
			fmt.Printf("  %s → %s (via %s)\n", displayPath, d.DisplayPath, d.SourceImport)
		}

		// Also add the repo itself to the lockfile.
		for _, repo := range manifest.IncludedRepos() {
			if repo.DisplayPath == displayPath {
				lf.AddRepo(displayPath, repo.Source, repo.CommitHash, repo.Ref)
				break
			}
		}
	}

	if *repoDir != "" {
		fmt.Printf("Updating dependencies for %s...\n", *repoDir)
		updateRepo(*repoDir)
	} else if *allRepos {
		fmt.Printf("Updating dependencies for all repos...\n")
		for _, repo := range manifest.IncludedRepos() {
			updateRepo(repo.DisplayPath)
		}
	} else {
		fmt.Fprintln(os.Stderr, "error: specify --repo=<path> or --all")
		os.Exit(1)
	}

	// Detect and report cycles.
	resolver := deps.NewImportResolver(manifest, lf)
	cycles := resolver.DetectCycles()
	if len(cycles) > 0 {
		fmt.Fprintf(os.Stderr, "\nwarning: dependency cycles detected:\n")
		for _, c := range cycles {
			fmt.Fprintf(os.Stderr, "  cycle: %v\n", []string(c))
		}
	}

	if err := lf.Save(lockfilePath); err != nil {
		fmt.Fprintf(os.Stderr, "error saving lockfile: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Lockfile updated: %s (%d repos)\n", lockfilePath, len(lf.Repositories))
}

func runPromote() {
	fs := flag.NewFlagSet("promote", flag.ExitOnError)
	mountRoot := fs.String("mount", "", "Mount root path (required)")
	repo := fs.String("repo", "", "Repo display_path to promote (required)")
	hermetic := fs.Bool("hermetic", false, "Promote directly to hermetic state")
	fs.Parse(os.Args[2:])

	if *mountRoot == "" || *repo == "" {
		fmt.Fprintln(os.Stderr, "error: --mount and --repo are required")
		fs.Usage()
		os.Exit(1)
	}

	repoDir := filepath.Join(*mountRoot, *repo)
	cfg, err := migration.LoadRepoConfig(repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	oldState := cfg.State
	newState := oldState.Next()
	if *hermetic {
		newState = migration.StateHermetic
	}

	if err := migration.ValidateStateTransition(oldState, newState); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Show validation warnings.
	warns, _ := migration.ValidatePrompt(repoDir, oldState)
	for _, w := range warns {
		fmt.Printf("  warning: %s\n", w)
	}

	cfg.State = newState
	if newState == migration.StateHermetic {
		// Remove fallback genrules.
		repos := []migration.RepoStatus{{DisplayPath: *repo, State: newState}}
		removed, _ := migration.RemoveFallbackGenrules(*mountRoot, repos)
		for _, r := range removed {
			fmt.Printf("  removed fallback: %s\n", r)
		}
	}

	if newState.IsAtLeast(migration.StateActive) {
		// Write fallback genrules for repos still native.
		// (The promoted repo now skips fallback since it's active.)
	}

	if err := migration.SaveRepoConfig(repoDir, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error saving config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Promoted %s: %s → %s\n", *repo, oldState, newState)
}

func runDemote() {
	fs := flag.NewFlagSet("demote", flag.ExitOnError)
	mountRoot := fs.String("mount", "", "Mount root path (required)")
	repo := fs.String("repo", "", "Repo display_path to demote (required)")
	targetState := fs.String("state", "native", "Target state: native, generating, partial, active")
	fs.Parse(os.Args[2:])

	if *mountRoot == "" || *repo == "" {
		fmt.Fprintln(os.Stderr, "error: --mount and --repo are required")
		fs.Usage()
		os.Exit(1)
	}

	newState := migration.State(*targetState)
	if !newState.Valid() {
		fmt.Fprintf(os.Stderr, "error: invalid state %q (valid: native, generating, partial, active, hermetic)\n", *targetState)
		os.Exit(1)
	}

	repoDir := filepath.Join(*mountRoot, *repo)
	cfg, err := migration.LoadRepoConfig(repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	oldState := cfg.State
	if err := migration.ValidateStateTransition(oldState, newState); err != nil {
		// Allow direct demotion in emergencies.
		fmt.Fprintf(os.Stderr, "warning: %v (forcing demotion)\n", err)
	}

	cfg.State = newState
	if !newState.IsAtLeast(migration.StateActive) {
		// Write fallback genrules since we're going back to native/partial.
		repos := []migration.RepoStatus{{DisplayPath: *repo, State: newState}}
		written, _ := migration.WriteFallbackGenrules(*mountRoot, repos)
		for _, w := range written {
			fmt.Printf("  wrote fallback: %s\n", w)
		}
	}

	if err := migration.SaveRepoConfig(repoDir, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error saving config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Demoted %s: %s → %s\n", *repo, oldState, newState)
}

func runStatus() {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	mountRoot := fs.String("mount", "", "Mount root path (required)")
	fs.Parse(os.Args[2:])

	if *mountRoot == "" {
		fmt.Fprintln(os.Stderr, "error: --mount is required")
		os.Exit(1)
	}

	manifestPath := filepath.Join(*mountRoot, ".monofs", "workspace.json")
	manifest, err := bazel.LoadManifest(manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading manifest: %v\n", err)
		os.Exit(1)
	}

	ws := &migration.WorkspaceStatus{
		MountRoot: *mountRoot,
	}

	for _, repo := range manifest.IncludedRepos() {
		repoDir := filepath.Join(*mountRoot, repo.DisplayPath)
		cfg, _ := migration.LoadRepoConfig(repoDir)

		rs := migration.RepoStatus{
			DisplayPath: repo.DisplayPath,
			State:       cfg.State,
			BuildSystem: migration.BuildSystemLabel(repoDir),
		}
		ws.Repos = append(ws.Repos, rs)
		ws.TotalRepos++
		if rs.State == migration.StateActive {
			ws.ActiveCount++
		}
		if rs.State == migration.StateHermetic {
			ws.HermeticCount++
		}
	}

	fmt.Printf("WORKSPACE: %s\n\n", ws.MountRoot)
	fmt.Printf("Adoption: %.0f%% (%d active + %d hermetic / %d total)\n\n",
		ws.AdoptionPercent(), ws.ActiveCount, ws.HermeticCount, ws.TotalRepos)

	for _, r := range ws.Repos {
		fmt.Printf("  %s\n", r.StatusText())
	}
}

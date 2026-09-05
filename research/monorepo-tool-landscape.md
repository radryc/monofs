# Monorepo Tool Landscape: Comparative Analysis

> Structured notes for MonoFS comparison analysis.  
> Generated: 2026-07-28

---

## 1. Build / Orchestration Tools

### Bazel
- **What it does**: Google's open-source build system (derived from Blaze). Correct, reproducible, fast builds at massive scale using action graphs. Multi-language (C++, Java, Go, Python, Rust, etc.) via rulesets.
- **Architecture**: Client/server model with a local build server. BUILD files define targets and dependencies as a directed acyclic graph (DAG). Actions are sandboxed (Linux namespaces). Shared remote cache (CAS) and remote execution (RE API). Uses Starlark (Python dialect) for rule definitions.
- **Key features**: Hermetic builds (sandboxed), content-addressable caching, remote execution, strict dependency tracking, query/cquery/aquery for graph introspection, `bazel test` runs only affected tests.
- **Limitations**: High migration cost (must write BUILD files for everything), steep learning curve, slow cold starts, JVM-based (memory hungry), poor IDE integration compared to language-native tools, BUILD file maintenance burden.
- **Virtual monorepo relation**: Bazel is the canonical "real monorepo" build tool. It requires everything in one repo. A virtual monorepo filesystem could feed Bazel multiple repos as if one, but BUILD files would need generation/mapping.

### Buck2
- **What it does**: Meta's second-generation build system. A complete rewrite of Buck in Rust. Designed for Meta's massive monorepo. Multi-language with an emphasis on speed and correctness.
- **Architecture**: Pure Rust core, no JVM. Build rules defined in Starlark. Uses a client/daemon model with watchman integration. Content-addressable build outputs. Supports remote execution and a virtual filesystem layer (EdenFS integration). Build Isolation via RE-compatible protocol.
- **Key features**: Extremely fast (Rust), Starlark-based rules (shared with Bazel but not identical), BXL (Buck Extension Language) for introspection, watchman-based file watching, deno-inspired rule runner, built-in support for dynamic dependencies.
- **Limitations**: Smaller community than Bazel, fewer rulesets available, tightly coupled to Meta's internal infrastructure (though open-sourced), documentation less mature, no equivalent of Bazel's `rules_*` ecosystem.
- **Virtual monorepo relation**: Already designed with virtual filesystem awareness (EdenFS). Buck2 queries the VFS directly rather than requiring all files on disk. The closest existing tool to the virtual monorepo concept among build systems.

### Nx
- **What it does**: Build system and monorepo management tool from Nrwl. Task orchestration with smart caching and dependency graph analysis. Primarily JavaScript/TypeScript ecosystem but supports others via plugins.
- **Architecture**: Node.js CLI + daemon. `nx.json` + `project.json` configuration. Computes a task graph from explicit project dependencies. Local computation cache (by hash of inputs) + remote cache via Nx Cloud. Plugins provide generators, executors, and dependency inference.
- **Key features**: Incremental adoption (works with existing npm/workspaces), affected command detection (only run tasks for changed projects), distributed task execution (Nx Cloud), visual graph in Nx Console (VSCode/JetBrains), code generation via schematics/generators, easy CI integration.
- **Limitations**: Node.js ecosystem focus (though expanding via plugins), remote cache/DTE requires Nx Cloud (proprietary SaaS), complex configurations at scale, plugin quality varies, less suitable for non-JS/TS heavy repos.
- **Virtual monorepo relation**: Nx has a "synthetic monorepo" concept via Nx Cloud that connects multiple repos. This is conceptually close to a virtual monorepo — different repos presented as a unified workspace with cross-repo task orchestration.

### Turborepo
- **What it does**: High-performance build system for JavaScript/TypeScript monorepos. Acquired by Vercel. Focuses on task running, caching, and parallelism.
- **Architecture**: Rust core (Go originally). Works with existing package.json scripts and workspace configurations (npm, yarn, pnpm). `turbo.json` defines task pipelines and dependencies. Content-hashing for cache keys. Remote caching via Vercel.
- **Key features**: Extremely simple adoption (add `turbo` to existing workspace), zero-config for basic use, parallel task execution across available cores, remote cache for CI speedup, pruning for deployment, watch mode for dev.
- **Limitations**: JS/TS ecosystem only, no build rule definitions (relies on package.json scripts), no code generation, limited to task orchestration (doesn't do actual compilation), remote cache tied to Vercel, sparse mode console output can hide errors.
- **Virtual monorepo relation**: No native virtual monorepo support. Pure task runner for single-repo layouts. Could be extended with filesystem-level virtual monorepo but doesn't have the concept.

### Lage
- **What it does**: Microsoft's task runner for JS monorepos. Lightweight alternative to Rush's full orchestrator. "Lage" = "lake" in Portuguese (calm, smooth). Task graph execution with caching.
- **Architecture**: Node.js-based. Uses `lage.config.js` to define pipelines. Backs onto npm/pnpm/yarn workspaces. Computes task graph, runs tasks in topological order with parallelism. Uses Nx-style computation hashing for cache keys.
- **Key features**: Lightweight, fast, integrates with backfill/remote cache, pipeline configuration, logging aggregation, works with any package manager.
- **Limitations**: Smaller community, JS-only, less polished than Turborepo/Nx, fewer integrations, Microsoft-internal focus.
- **Virtual monorepo relation**: No virtual monorepo concept. Single-repo task runner.

### Rush
- **What it does**: Microsoft's scalable monorepo manager for large web projects. Not just a task runner — full monorepo lifecycle management: installing, linking, building, publishing, change tracking.
- **Architecture**: Node.js CLI. Works with pnpm (primary). `rush.json` defines projects and policies. Rush installs all dependencies into a common node_modules using pnpm's strict linking. Heft (companion build system). Change files track version bumps. Publishing workflow built-in.
- **Key features**: Complete monorepo lifecycle (install/build/test/publish), policy enforcement (consistent versions, shrinkwrap), phased commands, change log generation, decoupled project versioning, works at Microsoft scale (hundreds of projects).
- **Limitations**: Complex setup, heavy toolchain (Rush + Heft + pnpm), JS/TS only, slower adoption curve, opinionated workflow, can feel over-engineered for smaller repos.
- **Virtual monorepo relation**: No virtual monorepo concept. It manages one physical repo with many packages. Change files and policy files sit in the root repo.

### Pants
- **What it does**: Build system originally created at Twitter (now Pantsbuild community). Multi-language (Python, Go, Java, Scala, Shell, Protobuf) with a focus on Python monorepos.
- **Architecture**: Python + Rust core (Pants 2.x). BUILD files define targets. Daemon-based for incremental builds. Dependency inference from source code (no manual dep declarations needed). Sandboxed execution for hermetic builds. Remote execution and caching.
- **Key features**: Automatic dependency inference (parse imports, no need to declare deps), fine-grained caching, remote execution, first-class Python support, `pants test ::` tests everything, plugin API for custom rules.
- **Limitations**: Python-heavy (daemon is Python), smaller community than Bazel, fewer language rulesets, documentation can be sparse, migration from other systems is manual.
- **Virtual monorepo relation**: No virtual monorepo concept. Operates on a physical monorepo directory tree.

### Please
- **What it does**: Build system from Thought Machine. Go-based, inspired by Bazel but simpler. "Please build, please test, please run."
- **Architecture**: Pure Go binary (single static binary!). BUILD files (similar to Bazel's but simpler). Hermetic builds with sandboxing. Content-addressed caching. Built-in support for Go, Python, Java, C++, Shell. No JVM or Python runtime needed.
- **Key features**: Single binary deployment (no runtime deps), simple BUILD syntax, fast, sandboxed by default, built-in cross-compilation, good Go support, `plz` CLI is intuitive.
- **Limitations**: Very small community, limited rulesets, Thought Machine-driven development velocity, less mature than Bazel/Buck, limited documentation.
- **Virtual monorepo relation**: No virtual monorepo concept. Single-binary simplicity works well for physical monorepos.

### Lerna
- **What it does**: Original monorepo tool for JavaScript. Manages multi-package repositories with shared dependencies, versioning, and publishing.
- **Architecture**: Node.js CLI. Works with npm/yarn workspaces. Two modes: fixed/locked (all packages same version) and independent (each package versioned separately). Uses conventional commits for changelog generation.
- **Key features**: Bootstrap/link local dependencies, run commands across all packages, version bumping, changelog generation, publishing to npm, simple mental model.
- **Limitations**: Slower than Turborepo/Nx (no computation caching), limited task orchestration, JS-only, now largely superseded by Turborepo/Nx + workspaces, mainly used for publishing workflows.
- **Virtual monorepo relation**: No virtual monorepo concept. Deprecated in favor of better tools.

---

## 2. Virtual Filesystem / Workspace Tools

### Microsoft VFS for Git
- **What it does**: Virtualizes the Git working directory so large repos don't need full checkouts. Only downloads files as they're accessed. Originally built for the Windows codebase (~300GB, 3.5M files).
- **Architecture**: A filesystem virtualization layer (Windows ProjFS or MacFUSE on macOS). Git is modified to only download metadata (tree objects) on clone. File contents are fetched on-demand when read. `gvfs` command manages hydration. A background maintenance process keeps the repo healthy.
- **Key features**: Clone a 300GB repo in minutes instead of hours, on-demand file hydration, sparse checkout that actually works at scale, transparent to tools (looks like a normal filesystem), integrated with Git's object model.
- **Limitations**: Requires OS-level filesystem virtualization (ProjFS on Windows, FUSE on macOS — no Linux support), complex setup, can be slow on first access (network fetch), background processes consume resources, deprecated in favor of Scalar.
- **Virtual monorepo relation**: THIS IS a virtual monorepo for a SINGLE repo. It virtualizes one repo's working tree. MonoFS's concept extends this across multiple repos.

### Meta's EdenFS / Sapling
- **What it does**: Meta's virtual filesystem for their massive monorepo. EdenFS mounts the repo as a FUSE filesystem. Sapling is the client-side VCS (successor to Mercurial at Meta). Together they provide a check-out-less workflow where files materialize on demand.
- **Architecture**: `EdenFS` — C++ FUSE daemon that presents a virtual filesystem backed by a content-addressed blobstore. Files are fetched from a remote store (Manifold) on first access. `Sapling` (Rust) — the VCS that talks to EdenFS. Doesn't need a working copy on disk; operates through EdenFS's virtual tree. Watchman provides file change notifications. Buck2 integrates directly with EdenFS.
- **Key features**: No checkout required — start working immediately, files materialize on access, millions of files without local storage, Sapling provides familiar Git-like CLI (`sl clone`, `sl commit`), integrated smartlog (stacked diffs), FUSE-based so all tools see a normal filesystem.
- **Limitations**: Requires EdenFS daemon (resource overhead), network-dependent (files must be fetched), Linux/macOS FUSE only, deeply tied to Meta's infrastructure, Sapling's Git compatibility is partial.
- **Virtual monorepo relation**: EdenFS is THE canonical example of a virtual monorepo for a single repository. MonoFS generalizes this by ingesting multiple independent Git repos into one virtual filesystem namespace.

### Google Piper / CitC
- **What it does**: Google's internal VCS and workspace system. Piper is the monolithic repository (single source of truth). CitC (Clients in the Cloud) provides cloud-backed workspaces that feel local but have no local copy of the full repo.
- **Architecture**: Piper is a proprietary VCS (not Git — custom monolithic store). Stores entire Google codebase as one repository. CitC uses a cloud-backed FUSE filesystem — workspaces are mounted, files are fetched on demand. All builds happen in the cloud or in CitC workspaces. Blaze (Bazel's internal counterpart) integrates natively.
- **Key features**: No local checkout — workspaces are cloud-backed, instant workspace creation, global code search (Code Search), cross-repo refactoring (Rosie), pre-submit testing (TAP), mandatory code review (Critique), trunk-based development.
- **Limitations**: Entirely proprietary (no open-source equivalent), requires Google infrastructure (Borg, Colossus, etc.), network-dependent, tightly integrated toolchain that can't be separated.
- **Virtual monorepo relation**: Piper/CitC is the original virtual monorepo — a single repo virtualized via cloud workspaces. MonoFS aims to provide similar ergonomics using Git repos rather than a proprietary VCS.

### Git Virtual File System (GVFS) / Scalar
- **What it does**: GVFS was Microsoft's virtual filesystem for Git (rebranded to VFS for Git, above). Scalar is its successor — a set of Git extensions and background maintenance that makes large repos fast without FUSE.
- **Architecture**: Scalar is a .NET CLI tool that wraps Git. Uses Git's built-in sparse-checkout, partial clone, and background maintenance features instead of filesystem virtualization. `scalar clone` sets up a partial clone, sparse checkout, and scheduled background fetches/GC. File system watcher for faster `git status`.
- **Key features**: No OS-level FUSE needed (works on any OS), uses Git's native partial clone and sparse checkout, background maintenance (auto-fetch, auto-GC), significantly faster than vanilla Git on large repos, actively maintained by Microsoft.
- **Limitations**: Still requires a clone (partial but present), not transparent to all tools, large repos can still be slow on first operations, less magical than FUSE-based approaches, Git learning curve for sparse checkout configuration.
- **Virtual monorepo relation**: Scalar is a "lightweight virtual monorepo" — partial clone + sparse checkout gives some benefits without FUSE complexity. MonoFS's FUSE approach provides a more complete virtualization.

### Slack's ReplayFS
- **What it does**: Slack's experimental FUSE filesystem that "records and replays" filesystem operations. Built to solve the problem of reproducible builds and CI determinism without containers.
- **Architecture**: FUSE-based filesystem that intercepts all FS syscalls. Records file reads/writes during a build. Can replay those exact reads for subsequent builds. Content-addressed storage. Focused on build reproducibility, not monorepo scaling.
- **Key features**: Record-and-replay for deterministic builds, content-addressed storage, avoids container overhead for reproducibility, fine-grained FS operation capture.
- **Limitations**: Experimental/internal tool, not a monorepo scaling solution per se, focused on build reproducibility more than workspace virtualization, Slack-specific, no public release.
- **Virtual monorepo relation**: A different spin on FUSE — for build reproducibility rather than repo virtualization. MonoFS's FUSE approach for multi-repo synthesis is a different application of the same underlying technology.

---

## 3. Source Management / Multi-Repo Tools

### Google's Repo Tool
- **What it does**: Orchestrates multiple Git repositories as a single project. Created by Google for Android (AOSP) development. Manages hundreds of Git repos as one coherent checkout.
- **Architecture**: Python script that reads a manifest XML file listing repos, branches, and paths. `repo init` downloads the manifest. `repo sync` clones/pulls all repos into a directory tree. `repo upload` sends changes for review (Gerrit).
- **Key features**: Single command to clone hundreds of repos, manifest-based configuration, branch management across repos (`repo start`), forall command (run command in every repo), Gerrit integration, used at massive scale (Android AOSP).
- **Limitations**: No unified history (each repo has separate Git history), no cross-repo atomic commits, `repo sync` can be slow for many repos, manifest maintenance burden, tooling is Android-specific in practice, Gerrit dependency for code review.
- **Virtual monorepo relation**: Repo creates a traditional "multi-repo checkout" — it's co-located but not unified. MonoFS's virtual monorepo provides a unified filesystem where repos appear as one coherent tree, unlike Repo's separate .git directories.

### git-subtree
- **What it does**: Merges an external repository's history into a subdirectory of the main repo. Unlike submodule, the external code becomes part of the main repo's history (no separate clone needed).
- **Architecture**: Git built-in command. `git subtree add` imports a repo's history into a subdirectory (squash or full history). `git subtree pull` updates the import. `git subtree push` contributes changes back upstream. Stores contributor info in commit messages.
- **Key features**: No separate clone/init step — just clone the main repo, code is part of main repo's history, simpler workflow than submodules, `git log` shows everything, works with any Git hosting.
- **Limitations**: Squash merges lose upstream history context, pushing back upstream is complex, doesn't track which subtree came from where (no .gitmodules equivalent), large repos make main repo huge, not suitable for many external repos.
- **Virtual monorepo relation**: Subtree physically merges repos — the opposite of virtual. MonoFS keeps repos separate but presents them as one filesystem.

### git-submodule
- **What it does**: Embeds external Git repositories as subdirectories within a parent repo. Each submodule points to a specific commit in its own repo. Native Git feature.
- **Architecture**: `.gitmodules` file stores mapping of path to repo URL. Each submodule has its own `.git` directory. Parent repo records submodule commit SHA. `git clone --recurse-submodules` clones everything. `git submodule update` syncs to recorded commits.
- **Key features**: Native Git, each submodule has full Git history, clear separation of concerns, pinning to specific commits, widely supported by Git hosting.
- **Limitations**: Two-step workflow (commit in submodule, then commit in parent), detached HEAD in submodules is confusing, merge conflicts in .gitmodules, recursive submodules are hard, CI runs must init submodules, no cross-repo atomic changes.
- **Virtual monorepo relation**: Submodules are the traditional "embed foreign repo" pattern. MonoFS's virtual monorepo eliminates the clone/init/update dance — repos appear as directories in a single FUSE mount.

### gitslave
- **What it does**: Creates a group of related Git repositories — one "master" repo and several "slave" repos. Commands are replicated across all repos.
- **Architecture**: Perl script. `gits attach` links slave repos to a master. `.gitslave` file stores configuration. Commands like `gits checkout -b feature` run in master + all slaves. Manages superproject/subrepo relationships.
- **Key features**: Parallel command execution across repos, branch management across repos, simpler than submodules for related repos, unified diff/log across repos.
- **Limitations**: Perl dependency, aging project, small community, only works with gitslave commands (not transparent to Git), doesn't handle merge conflicts between repos naturally, limited adoption.
- **Virtual monorepo relation**: Another multi-repo orchestration pattern — runs commands across repos but doesn't unify them. MonoFS provides filesystem-level unification rather than command replication.

### myrepos (mr)
- **What it does**: Tool to manage multiple version control repositories at once. Agnostic to VCS (Git, Mercurial, SVN, darcs, etc.). Runs commands across all registered repos.
- **Architecture**: Perl script. `~/.mrconfig` defines repos and their VCS types. `mr register` adds current dir as a repo. `mr update` runs VCS-appropriate update across all repos. `mr run` executes arbitrary commands. Extensible via shell scripts.
- **Key features**: VCS-agnostic (Git, hg, svn, bzr, darcs, cvs), arbitrary command dispatch, simple configuration, extensible, chainable actions.
- **Limitations**: Perl dependency, no dependency ordering between repos, no unified history, no atomic cross-repo operations, aging project, limited to command replication.
- **Virtual monorepo relation**: Like gitslave, this is a command orchestrator, not a filesystem unifier. MonoFS provides actual filesystem coherence rather than just command fanout.

---

## 4. Platform Monorepo Products

### Nix (Flakes)
- **What it does**: Purely functional package manager and build system. Nix Flakes provide reproducible, composable software environments with dependency locking. Can be used as a monorepo build system.
- **Architecture**: Nix expression language defines derivations (build recipes). Everything is content-addressed in `/nix/store`. Flakes add a standard entry point (`flake.nix`) with inputs, outputs, and a lock file. Lazy evaluation — only what's needed is built. Cross-language (anything that can be built from source).
- **Key features**: Truly reproducible builds (same hash = same output), hermetically sealed, binary cache (cache.nixos.org), multi-language, dev shells (`nix develop`), flake lock files for pinning, multi-platform builds.
- **Limitations**: Steep learning curve (Nix language is unusual), macOS compatibility gaps, large disk footprint (store accumulates), documentation is fragmented, flakes are still experimental, weird error messages, IDE support is minimal.
- **Virtual monorepo relation**: Nix can ingest multiple source repos as flake inputs and treat them as a unified build. This is conceptually a virtual monorepo at the build/package level rather than filesystem level. MonoFS's FUSE approach could complement Nix by providing filesystem-level unification.

### JetBrains Space
- **What it does**: All-in-one development platform from JetBrains. Combines Git hosting, code reviews, CI/CD, package registry, issue tracking, and documentation in one product. Monorepo-friendly.
- **Architecture**: Cloud-hosted platform (SaaS or on-prem). Git repositories with built-in code review (merge requests). Automation (CI/CD) with Kotlin DSL. Package repositories (Docker, Maven, NuGet). Project management (issues, checklists, documents). Strong IDE integration (JetBrains IDEs).
- **Key features**: Unified platform (no toolchain stitching), deep IntelliJ/GoLand/PyCharm integration, code review with IDE-level inspections, automation scripts in Kotlin, built-in package registries, calendar/meetings integration.
- **Limitations**: Vendor lock-in (JetBrains ecosystem), less mature than GitHub/GitLab, smaller community, pricing model, primarily for JetBrains IDE users, limited third-party integrations.
- **Virtual monorepo relation**: Not a virtual monorepo — it's a development platform that hosts multiple repos. Could be a source of repos for MonoFS to ingest.

### Sourcegraph
- **What it does**: Universal code search and intelligence platform. Indexes code across many repositories and provides code navigation (find references, go-to-definition) without local checkout.
- **Architecture**: Go backend with PostgreSQL/Redis. Indexes code using Zoekt (trigram search engine) for fast code search. Language servers provide precise code intelligence (hover, definitions, references). Browser-based UI. Supports Git, Perforce, CVS. Batch changes (multi-repo refactoring).
- **Key features**: Code search across thousands of repos, precise code intelligence (compiler-accurate), batch changes (automated large-scale refactoring), code insights (dashboards, ownership), browser-only workflow, supports any VCS.
- **Limitations**: Requires running server infrastructure (or paid cloud), language server setup per language, large-scale deployments are complex, precise intel needs build configuration, search latency at extreme scale, paid features (batch changes).
- **Virtual monorepo relation**: Sourcegraph provides a "virtual monorepo" for SEARCH — you can search across thousands of repos as if they were one. MonoFS provides a "virtual monorepo" for FILESYSTEM — you can WORK across repos as if they were one. The two are complementary: MonoFS uses Zoekt (same engine!) for code search.

### Gerrit
- **What it does**: Git-based code review system. Google-originated. Forge-based workflow where every commit is individually reviewable. Designed for monorepo-scale code review. Also has multi-repo management features.
- **Architecture**: Java server (JGit). Each repo has a Gerrit project. Changes are pushed as individual commits to `refs/for/BRANCH`. Review happens per-commit (not per-PR). CI verification can be per-commit. Submit strategies control merge behavior. Repo tool integration for multi-repo.
- **Key features**: Per-commit review (not per-branch/PR), fine-grained access control, draft reviews, CI verification gates, submit strategies (rebase, merge, cherry-pick), deep Git integration, scales to thousands of repos.
- **Limitations**: Java server (heavy), complex setup/maintenance, per-commit workflow is unfamiliar to GitHub users, UI is less polished, plugin ecosystem is smaller, learning curve for admins.
- **Virtual monorepo relation**: Gerrit manages many repos but they're separate Git repos. MonoFS could provide a unified filesystem view where a developer makes changes across repos, and Gerrit could remain the review backend for each individual repo. Complementary.

---

## 5. Summary: How MonoFS Relates

| Concept | Existing Tools | MonoFS's Approach |
|---|---|---|
| **Filesystem unification** | EdenFS (single repo), VFS for Git (single repo) | FUSE mount of MULTIPLE independent Git repos as one coherent tree |
| **Build system** | Bazel, Buck2, Nx, Turborepo (require everything in one tree or one workspace) | Provides that tree via FUSE so any build system works without migration |
| **Code search** | Sourcegraph (Zoekt-based search) | Uses Zoekt internally for code search across the virtual namespace |
| **Multi-repo management** | Repo tool, submodules, myrepos (orchestration, not unification) | Actual filesystem-level unification instead of scripting |
| **CI/CD** | Each platform has its own (Nx Cloud, Gerrit CI, etc.) | Built-in pipeline system with Guardian config + Doctor telemetry |
| **Repository history** | All tools work with existing Git histories | go-git for Git operations, preserves individual repo histories |
| **Distributed coordination** | Mostly single-node (except remote execution) | Raft KVS for distributed task queuing, gRPC for cluster communication |

### Key Gaps in Existing Tools that MonoFS Addresses

1. **No tool provides FUSE-based multi-repo synthesis** — VFS for Git, EdenFS, and CitC all virtualize a SINGLE repo. MonoFS is the only one that ingests multiple independent Git repos into one coherent filesystem.

2. **Build system lock-in** — Bazel, Buck2, Pants require BUILD files and specific rule systems. MonoFS is build-system agnostic — any tool sees a normal filesystem.

3. **History preservation** — subtree merges lose individual repo history. Submodules require manual clone/init. MonoFS keeps repos independent but presents them as one tree.

4. **Operational intelligence** — Most tools lack built-in telemetry (build times, failure rates, ownership). MonoFS's Doctor namespace provides this.

5. **Config management** — No existing tool provides a unified config layer across repos. MonoFS's Guardian namespace addresses this.

6. **Incremental migration** — Moving to a monorepo (Bazel, Buck2) requires massive upfront work. MonoFS provides monorepo ergonomics without migration.

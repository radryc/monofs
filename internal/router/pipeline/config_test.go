package pipeline

import (
	"testing"
)

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{
			name: "minimal valid config",
			yaml: `
name: test-pipeline
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: builder
    steps:
      - run: make build
`,
			wantErr: false,
		},
		{
			name: "multi-job DAG",
			yaml: `
name: full-pipeline
on:
  push:
    branches: [main]
jobs:
  lint:
    runs-on: builder
    steps:
      - run: make fmt
  build:
    needs: [lint]
    runs-on: builder
    steps:
      - run: make build
  deploy:
    needs: [build]
    runs-on: deployer
    steps:
      - run: guardianctl deploy
`,
			wantErr: false,
		},
		{
			name: "missing name",
			yaml: `
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: builder
    steps:
      - run: make build
`,
			wantErr: true,
		},
		{
			name: "no jobs",
			yaml: `
name: empty
on:
  push:
    branches: [main]
jobs: {}
`,
			wantErr: true,
		},
		{
			name: "cycle detection",
			yaml: `
name: cyclical
on:
  push:
    branches: [main]
jobs:
  a:
    needs: [b]
    runs-on: builder
    steps:
      - run: echo a
  b:
    needs: [a]
    runs-on: builder
    steps:
      - run: echo b
`,
			wantErr: true,
		},
		{
			name: "unknown dependency",
			yaml: `
name: bad-dep
on:
  push:
    branches: [main]
jobs:
  build:
    needs: [nonexistent]
    runs-on: builder
    steps:
      - run: make build
`,
			wantErr: true,
		},
		{
			name: "matrix strategy",
			yaml: `
name: matrix-build
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: builder
    strategy:
      matrix:
        os: [linux, darwin]
        arch: [amd64, arm64]
      max-parallel: 4
    steps:
      - run: make build-${{ matrix.os }}-${{ matrix.arch }}
`,
			wantErr: false,
		},
		{
			name: "pull request trigger",
			yaml: `
name: pr-checks
on:
  pull_request:
    branches: [main]
    paths-ignore: [docs/**, "*.md"]
jobs:
  test:
    runs-on: builder
    steps:
      - run: make test
`,
			wantErr: false,
		},
		{
			name: "tag trigger",
			yaml: `
name: release
on:
  tags: ["v*"]
jobs:
  release:
    runs-on: builder
    steps:
      - run: make release
`,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParseConfig([]byte(tt.yaml))
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err != nil {
				return
			}
			if cfg.Name == "" {
				t.Fatal("config name is empty")
			}
		})
	}
}

func TestDAGEntrypoints(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
name: dag-test
on:
  push:
    branches: [main]
jobs:
  a:
    runs-on: builder
    steps:
      - run: echo a
  b:
    needs: [a]
    runs-on: builder
    steps:
      - run: echo b
  c:
    needs: [a]
    runs-on: builder
    steps:
      - run: echo c
  d:
    needs: [b, c]
    runs-on: builder
    steps:
      - run: echo d
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	entrypoints := cfg.EntrypointJobs()
	if len(entrypoints) != 1 || entrypoints[0] != "a" {
		t.Fatalf("expected entrypoint [a], got %v", entrypoints)
	}

	downstream := cfg.DownstreamJobs("a")
	if len(downstream) != 2 {
		t.Fatalf("expected 2 downstream from a, got %v", downstream)
	}

	completed := map[string]bool{"a": true}
	if !cfg.AllNeedsSatisfied("b", completed) {
		t.Fatal("b should be satisfied after a completes")
	}
	if cfg.AllNeedsSatisfied("d", completed) {
		t.Fatal("d should not be satisfied until b and c complete")
	}

	completed["b"] = true
	completed["c"] = true
	if !cfg.AllNeedsSatisfied("d", completed) {
		t.Fatal("d should be satisfied after b and c complete")
	}
}

func TestMatchEvent(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
name: trigger-test
on:
  push:
    branches: [main, "release/*"]
  pull_request:
    branches: [main]
  tags: ["v*"]
jobs:
  test:
    runs-on: builder
    steps:
      - run: make test
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	tests := []struct {
		name  string
		event WebhookEvent
		want  bool
	}{
		{"push to main", WebhookEvent{EventType: TriggerPush, Branch: "main"}, true},
		{"push to release/v1", WebhookEvent{EventType: TriggerPush, Branch: "release/v1"}, true},
		{"push to feature", WebhookEvent{EventType: TriggerPush, Branch: "feature/x"}, false},
		{"pr to main", WebhookEvent{EventType: TriggerPullRequest, Branch: "main"}, true},
		{"pr to feature", WebhookEvent{EventType: TriggerPullRequest, Branch: "feature/x"}, false},
		{"tag v1.0.0", WebhookEvent{EventType: TriggerTag, Tag: "v1.0.0"}, true},
		{"tag release", WebhookEvent{EventType: TriggerTag, Tag: "release-1"}, false},
		{"manual", WebhookEvent{EventType: TriggerManual, Branch: "main"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cfg.MatchEvent(tt.event); got != tt.want {
				t.Errorf("MatchEvent() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchEventWithSourceDir(t *testing.T) {
	cfg, _ := ParseConfig([]byte(`
name: scoped-test
on:
  push:
    branches: [main]
jobs:
  test:
    runs-on: builder
    steps:
      - run: make test
`))
	cfg.SourceDir = "packages/server"

	tests := []struct {
		name  string
		event WebhookEvent
		want  bool
	}{
		{"change in scope", WebhookEvent{EventType: TriggerPush, Branch: "main", ChangedFiles: []string{"packages/server/main.go"}}, true},
		{"change outside scope", WebhookEvent{EventType: TriggerPush, Branch: "main", ChangedFiles: []string{"packages/frontend/index.ts"}}, false},
		{"no changed files", WebhookEvent{EventType: TriggerPush, Branch: "main", ChangedFiles: nil}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cfg.MatchEvent(tt.event); got != tt.want {
				t.Errorf("MatchEvent() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchEventRootScope(t *testing.T) {
	cfg, _ := ParseConfig([]byte(`
name: root-test
on:
  push:
    branches: [main]
jobs:
  test:
    runs-on: builder
    steps:
      - run: make test
`))
	cfg.SourceDir = "."

	if !cfg.MatchEvent(WebhookEvent{EventType: TriggerPush, Branch: "main", ChangedFiles: []string{"anywhere/file.go"}}) {
		t.Error("root-scoped pipeline should match any changed file")
	}
}

func TestGlobMatching(t *testing.T) {
	tests := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"*", "anything", true},
		{"main", "main", true},
		{"main", "feature", false},
		{"release/*", "release/v1", true},
		{"release/*", "release/v1.2.3", true},
		{"release/*", "feature/v1", false},
		{"v*", "v1.0.0", true},
		{"v*", "v2", true},
		{"v*", "release", false},
		{"feature/*/x", "feature/a/x", true},
		{"feature/*/x", "feature/a/y", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"/"+tt.value, func(t *testing.T) {
			if got := matchGlob(tt.pattern, tt.value); got != tt.want {
				t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.value, got, tt.want)
			}
		})
	}
}

func TestExpandMatrix(t *testing.T) {
	matrix := MatrixConfig{
		"os":   {Static: []string{"linux", "darwin"}},
		"arch": {Static: []string{"amd64", "arm64"}},
	}
	result, err := expandMatrix(matrix, func(string) ([]string, bool) { return nil, false })
	if err != nil {
		t.Fatalf("expandMatrix: %v", err)
	}
	if len(result) != 4 {
		t.Fatalf("expected 4 combinations, got %d", len(result))
	}

	expected := map[string]bool{
		"linux-amd64":  false,
		"linux-arm64":  false,
		"darwin-amd64": false,
		"darwin-arm64": false,
	}
	for _, combo := range result {
		key := combo["os"] + "-" + combo["arch"]
		expected[key] = true
	}
	for k, v := range expected {
		if !v {
			t.Errorf("missing combination: %s", k)
		}
	}
}

func TestExpandMatrixFromExpression(t *testing.T) {
	matrix := MatrixConfig{
		"package": {Expr: "${{ needs.detect.outputs.packages }}"},
	}
	resolver := func(expr string) ([]string, bool) {
		if expr == "${{ needs.detect.outputs.packages }}" {
			return []string{"server", "router"}, true
		}
		return nil, false
	}
	result, err := expandMatrix(matrix, resolver)
	if err != nil {
		t.Fatalf("expandMatrix: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 combinations, got %d", len(result))
	}
	got := []string{result[0]["package"], result[1]["package"]}
	if got[0] != "server" || got[1] != "router" {
		t.Errorf("packages = %v, want [server router]", got)
	}
}

func TestExpandMatrixEmptyResolvedExpression(t *testing.T) {
	matrix := MatrixConfig{
		"package": {Expr: "${{ needs.detect.outputs.packages }}"},
	}
	result, err := expandMatrix(matrix, func(string) ([]string, bool) { return nil, true })
	if err != nil {
		t.Fatalf("expandMatrix: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected 0 combinations for empty output, got %d", len(result))
	}
}

func TestParseMatrixConfigStaticAndExpression(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
name: matrix-pipeline
on:
  push:
    branches: [main]
jobs:
  detect:
    runs-on: builder
    steps:
      - uses: monofs/affected@v1
        id: affected
  build:
    needs: [detect]
    strategy:
      matrix:
        package: ${{ needs.detect.outputs.packages }}
        os: [linux, darwin]
    runs-on: builder
    steps:
      - run: make build-${{ matrix.package }} GOOS=${{ matrix.os }}
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	build := cfg.Jobs["build"]
	if build.Strategy == nil {
		t.Fatal("expected strategy on build job")
	}
	pkgVar := build.Strategy.Matrix["package"]
	if pkgVar.Expr != "${{ needs.detect.outputs.packages }}" {
		t.Errorf("package expr = %q", pkgVar.Expr)
	}
	osVar := build.Strategy.Matrix["os"]
	if len(osVar.Static) != 2 || osVar.Static[0] != "linux" {
		t.Errorf("os static = %v", osVar.Static)
	}
}

func TestMatchEventPathsFilters(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
name: paths-pipeline
on:
  push:
    branches: [main]
    paths: [cmd/**, internal/**]
    paths-ignore: ["*.md", docs/**]
jobs:
  build:
    runs-on: builder
    steps:
      - run: make build
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	tests := []struct {
		name  string
		files []string
		want  bool
	}{
		{"code change matches", []string{"cmd/server/main.go"}, true},
		{"internal change matches", []string{"internal/server/x.go"}, true},
		{"docs ignored", []string{"docs/guide.md"}, false},
		{"root markdown ignored", []string{"README.md"}, false},
		{"unrelated path", []string{"config/settings.json"}, false},
		{"mixed code and docs matches", []string{"docs/a.md", "cmd/x.go"}, true},
		{"no file info matches", nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cfg.MatchEvent(WebhookEvent{
				EventType:    TriggerPush,
				Branch:       "main",
				ChangedFiles: tt.files,
			})
			if got != tt.want {
				t.Errorf("MatchEvent(files=%v) = %v, want %v", tt.files, got, tt.want)
			}
		})
	}
}

func TestPathGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		file    string
		want    bool
	}{
		{"cmd/**", "cmd/server/main.go", true},
		{"cmd/**", "cmd/main.go", true},
		{"cmd/**", "internal/x.go", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", false},
		{"docs/**", "docs/a/b/c.md", true},
		{"**", "anything/at/all.txt", true},
		{"cmd/*", "cmd/server/main.go", false},
		{"cmd/*", "cmd/main.go", true},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.file, func(t *testing.T) {
			if got := pathGlobMatch(tt.pattern, tt.file); got != tt.want {
				t.Errorf("pathGlobMatch(%q, %q) = %v, want %v", tt.pattern, tt.file, got, tt.want)
			}
		})
	}
}

func TestLoadPackageMeta(t *testing.T) {
	meta, err := LoadPackageMeta(nil, "../../../monofs-packages.yaml")
	if err != nil {
		t.Fatalf("LoadPackageMeta: %v", err)
	}
	if len(meta.Packages) == 0 {
		t.Fatal("expected at least one package")
	}
	if _, ok := meta.Packages["server"]; !ok {
		t.Fatal("expected 'server' package")
	}
	if _, ok := meta.Packages["router"]; !ok {
		t.Fatal("expected 'router' package")
	}
	if pkg := meta.Packages["server"]; pkg.Path == "" {
		t.Fatal("server package should have a path")
	}
}

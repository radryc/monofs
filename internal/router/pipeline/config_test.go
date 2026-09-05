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
	matrix := map[string][]string{
		"os":   {"linux", "darwin"},
		"arch": {"amd64", "arm64"},
	}
	result := expandMatrix(matrix)
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

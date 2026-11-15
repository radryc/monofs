package router

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/radryc/monofs/internal/router/guardian"
)

func newPipelineKVSTestRouter(t *testing.T) *Router {
	t.Helper()
	store, err := guardian.NewStore("")
	if err != nil {
		t.Fatalf("guardian.NewStore: %v", err)
	}
	t.Cleanup(store.Close)
	return &Router{
		guardianVersions: store,
		logger:           slog.New(slog.DiscardHandler),
	}
}

func TestListPipelinePath(t *testing.T) {
	r := newPipelineKVSTestRouter(t)

	for _, path := range []string{
		"/.pipelines/build/config.yaml",
		"/.pipelines/test/config.yaml",
		"/.pipelines/deleted/config.yaml",
		"/partitions/demo/config.yaml",
	} {
		if _, err := r.guardianVersions.Commit(guardian.Commit{
			LogicalPath: path,
			Content:     []byte("kind: Pipeline"),
			CommittedAt: 1,
		}); err != nil {
			t.Fatalf("commit %s: %v", path, err)
		}
	}
	if _, err := r.guardianVersions.Commit(guardian.Commit{
		LogicalPath: "/.pipelines/deleted/config.yaml",
		Tombstone:   true,
		CommittedAt: 2,
	}); err != nil {
		t.Fatalf("tombstone: %v", err)
	}

	for _, dir := range []string{"/.pipelines", "/.pipelines/", ".pipelines"} {
		entries, err := r.listPipelinePath(dir)
		if err != nil {
			t.Fatalf("listPipelinePath(%q): %v", dir, err)
		}
		if len(entries) != 2 {
			t.Fatalf("listPipelinePath(%q) = %v, want 2 entries", dir, entries)
		}
		if entries[0] != "build/config.yaml" || entries[1] != "test/config.yaml" {
			t.Fatalf("listPipelinePath(%q) = %v", dir, entries)
		}
	}

	entries, err := r.listPipelinePath("/.pipelines/missing")
	if err != nil {
		t.Fatalf("missing dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("missing dir entries = %v", entries)
	}
}

func TestReadPipelinePath(t *testing.T) {
	r := newPipelineKVSTestRouter(t)
	path := "/partitions/demo/config.yaml"
	version, err := r.guardianVersions.Commit(guardian.Commit{
		LogicalPath: path,
		Content:     []byte("kind: Config"),
		CommittedAt: 1,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	content, gotVersion, err := r.readPipelinePath(path)
	if err != nil {
		t.Fatalf("readPipelinePath: %v", err)
	}
	if string(content) != "kind: Config" || gotVersion != version.GetVersionId() {
		t.Fatalf("content = %q version = %q", content, gotVersion)
	}

	if _, _, err := r.readPipelinePath("/partitions/demo/missing.yaml"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing path error = %v", err)
	}
	if _, _, err := r.readPipelinePath("/not-managed/path.yaml"); err == nil || !strings.Contains(err.Error(), "map path") {
		t.Fatalf("unmanaged path error = %v", err)
	}

	if _, err := r.guardianVersions.Commit(guardian.Commit{
		LogicalPath: path,
		Tombstone:   true,
		CommittedAt: 2,
	}); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	if _, _, err := r.readPipelinePath(path); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("tombstoned path error = %v", err)
	}
}

func TestGuardianNodeClientForPathWithoutNodes(t *testing.T) {
	r := newPipelineKVSTestRouter(t)
	mapped := guardianPhysicalPath{DisplayPath: "guardian/demo", RelativePath: "config.yaml"}
	if _, _, err := r.guardianNodeClientForPath(mapped); err == nil {
		t.Fatal("expected an error when no healthy guardian nodes are available")
	}
}

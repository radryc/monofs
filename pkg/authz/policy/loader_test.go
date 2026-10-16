package policy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func compiledFixture(t *testing.T) []byte {
	t.Helper()
	p := compileTestPolicy(t, testPolicyYAML, testCatalog())
	data, err := MarshalPolicy(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLoaderFileSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	data := compiledFixture(t)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	loader := NewLoader(FileSource{Path: path}, time.Minute, t.Logf)
	ctx := context.Background()
	if err := loader.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !loader.Ready() {
		t.Fatal("loader should be ready")
	}
	first := loader.Get()
	if first == nil || first.Version() == "" {
		t.Fatal("expected loaded policy")
	}

	// Overwrite with a different policy; refresh must pick it up.
	changed := &File{
		Version:  1,
		Defaults: map[string]string{"partitions": "allow"},
	}
	bundle, _, err := Compile(changed, nil, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	changedData, err := MarshalBundle(*bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, changedData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loader.Refresh(ctx); err != nil {
		t.Fatalf("refresh changed: %v", err)
	}
	if loader.Get().Version() == first.Version() {
		t.Fatal("expected a new policy version")
	}

	// Corrupt the file; the last good policy must survive.
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loader.Refresh(ctx); err == nil {
		t.Fatal("expected refresh error for corrupt bundle")
	}
	if !loader.Ready() || loader.Get().Version() == "" {
		t.Fatal("last good policy must be kept after a failed refresh")
	}
	if loader.LastError() == nil {
		t.Fatal("expected LastError to be set")
	}
}

func TestLoaderHTTPSource(t *testing.T) {
	data := compiledFixture(t)
	var served atomic
	served.data = data

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("ETag", "v1")
		w.Header().Set("X-Policy-Version", "v1")
		w.Write(served.load())
	}))
	defer server.Close()

	loader := NewLoader(HTTPSource{URL: server.URL, Token: "secret-token"}, time.Minute, t.Logf)
	if err := loader.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !loader.Ready() {
		t.Fatal("loader should be ready")
	}
	version := loader.Get().Version()

	// Same content and etag: refresh is a no-op.
	if err := loader.Refresh(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if loader.Get().Version() != version {
		t.Fatal("version must not change for unchanged content")
	}
}

type atomic struct {
	data []byte
}

func (a *atomic) load() []byte { return a.data }

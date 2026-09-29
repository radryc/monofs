package executor

import (
	"path/filepath"
	"testing"
)

func TestResolveWithin(t *testing.T) {
	base := t.TempDir()

	got, err := resolveWithin(base, "a/b.txt")
	if err != nil {
		t.Fatalf("valid relative path rejected: %v", err)
	}
	if want := filepath.Join(base, "a/b.txt"); got != want {
		t.Fatalf("resolveWithin = %q, want %q", got, want)
	}

	if got, err := resolveWithin(base, ""); err != nil || got != base {
		t.Fatalf("empty path = (%q, %v), want (%q, nil)", got, err, base)
	}

	for _, rel := range []string{"../escape.txt", "/etc/passwd", "a/../../x"} {
		if _, err := resolveWithin(base, rel); err == nil {
			t.Fatalf("path %q should be rejected", rel)
		}
	}
}

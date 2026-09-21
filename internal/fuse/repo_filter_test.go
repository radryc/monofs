package fuse

import "testing"

func TestRepoFilterAllows(t *testing.T) {
	cases := []struct {
		name    string
		include []string
		exclude []string
		path    string
		want    bool
	}{
		{"no filter allows all", nil, nil, "github.com/acme/api", true},
		{"include exact", []string{"github.com/acme/api"}, nil, "github.com/acme/api", true},
		{"include exact miss", []string{"github.com/acme/api"}, nil, "github.com/acme/other", false},
		{"include prefix glob", []string{"github.com/acme/*"}, nil, "github.com/acme/api", true},
		{"include prefix glob miss", []string{"github.com/acme/*"}, nil, "sre/foo", false},
		{"include directory", []string{"sre/*"}, nil, "sre/foo/bar", true},
		{"include double star", []string{"docs/**"}, nil, "docs/a/b.md", true},
		{"exclude prefix", nil, []string{"guardian/*"}, "guardian/doctor", false},
		{"exclude exact", nil, []string{"docs/**"}, "docs/x", false},
		{"exclude miss", nil, []string{"guardian/*"}, "sre/foo", true},
		{"include then exclude", []string{"github.com/*"}, []string{"github.com/acme/*"}, "github.com/acme/api", false},
		{"include then exclude opposite", []string{"github.com/*"}, []string{"github.com/acme/*"}, "github.com/other/repo", true},
		{"exclude only allows others", nil, []string{"guardian/*", "docs/**"}, "github.com/acme/api", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := NewRepoFilter(c.include, c.exclude)
			if err != nil {
				t.Fatalf("NewRepoFilter: %v", err)
			}
			if got := f.Allows(c.path); got != c.want {
				t.Fatalf("Allows(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

func TestRepoFilterIsEmpty(t *testing.T) {
	f, _ := NewRepoFilter(nil, nil)
	if !f.IsEmpty() {
		t.Fatal("empty filter should be empty")
	}
	f2, _ := NewRepoFilter([]string{"a/*"}, nil)
	if f2.IsEmpty() {
		t.Fatal("filter with include should not be empty")
	}
	var nilFilter *RepoFilter
	if !nilFilter.IsEmpty() {
		t.Fatal("nil filter should be empty")
	}
}

func TestNewRepoFilterRejectsEmptyGlob(t *testing.T) {
	if _, err := NewRepoFilter([]string{""}, nil); err == nil {
		t.Fatal("empty include glob should be rejected")
	}
	if _, err := NewRepoFilter(nil, []string{"  "}); err == nil {
		t.Fatal("whitespace exclude glob should be rejected")
	}
}

func TestRepoGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"**", "github.com/acme/api", true},
		{"github.com/acme/api", "github.com/acme/api", true},
		{"github.com/acme/*", "github.com/acme/api", true},
		{"github.com/acme/*", "github.com/acme/api/sub", true},
		{"github.com/acme/api", "github.com/acme/api/sub", false},
		{"github.com/**", "github.com/acme/api", true},
		{"sre/*", "sre/foo", true},
		{"sre/?", "sre/ab", false},
		{"sre/?", "sre/a", true},
	}
	for _, c := range cases {
		if got := repoGlobMatch(c.pattern, c.path); got != c.want {
			t.Errorf("repoGlobMatch(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

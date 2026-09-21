package authz

import (
	"context"
	"testing"
)

func TestParseCodeowners(t *testing.T) {
	file, err := ParseCodeowners([]byte(`
# Root owners default to the admins team
* @admins

# Docs are owned by the docs team
/docs/ @docs-team

# Go files anywhere have a specific owner
**/*.go @gophers alice@example.com

/internal/security/ @security cto@example.com
`))
	if err != nil {
		t.Fatalf("ParseCodeowners: %v", err)
	}
	if len(file.Rules) != 4 {
		t.Fatalf("rules = %d, want 4", len(file.Rules))
	}
	if file.Rules[0].Pattern != "*" || file.Rules[0].Owners[0].Team != "admins" {
		t.Errorf("rule 0 = %+v", file.Rules[0])
	}
	last := file.Rules[3]
	if last.Pattern != "/internal/security/" {
		t.Errorf("rule 3 pattern = %q", last.Pattern)
	}
	if len(last.Owners) != 2 || !last.Owners[0].IsTeam() || last.Owners[1].Subject != "cto@example.com" {
		t.Errorf("rule 3 owners = %+v", last.Owners)
	}
}

func TestParseCodeownersRejectsMalformed(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"pattern without owners", "/docs/\n"},
		{"only comments", "# nothing here\n"},
		{"empty team ref", "/docs/ @\n"},
		{"no rules at all", "\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseCodeowners([]byte(tt.data)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestCodeownersPatternMatches(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		// Unanchored globs match at any depth (GitHub semantics: "*"
		// is the repo-wide catch-all, "*.md" matches all .md files).
		{"*", "README.md", true},
		{"*", "docs/README.md", true},
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", true},
		{"*.md", "docs/guide.md", true},
		{"*.md", "cmd/main.go", false},
		// Anchored directory patterns.
		{"/docs/", "docs", true},
		{"/docs/", "docs/a.md", true},
		{"/docs/", "docs/sub/b.md", true},
		{"/docs/", "other/a.md", false},
		{"/internal/security/", "internal/security/policy.yaml", true},
		{"/internal/security", "internal/security/policy.yaml", true},
		// Recursive globs.
		{"**/*.go", "cmd/main.go", true},
		{"**/*.go", "a/b/c/d.go", true},
		{"**/*.go", "cmd/main.py", false},
		{"cmd/**", "cmd/server/main.go", true},
		{"cmd/*", "cmd/main.go", true},
		{"cmd/*", "cmd/server/main.go", false},
		// Unanchored directory names match at any depth.
		{"docs/", "a/b/docs/x.md", true},
		{"docs/", "docs/x.md", true},
		{"docs/", "other/x.md", false},
		{"config.yaml", "a/b/config.yaml", true},
		{"config.yaml", "config.yaml", true},
		{"config.yaml", "a/other.yaml", false},
		// Wildcards.
		{"te?t/**", "test/x.go", true},
		{"/file?.txt", "file1.txt", true},
		{"/file?.txt", "file10.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.path, func(t *testing.T) {
			if got := CodeownersPatternMatches(tt.pattern, tt.path); got != tt.want {
				t.Errorf("CodeownersPatternMatches(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

func TestCodeownersOwnersForLastMatchWins(t *testing.T) {
	file, err := ParseCodeowners([]byte(`
* @everyone
/docs/ @docs-team
/docs/security/ @security
`))
	if err != nil {
		t.Fatalf("ParseCodeowners: %v", err)
	}

	if owners := file.OwnersFor("README.md"); len(owners) != 1 || owners[0].Team != "everyone" {
		t.Errorf("OwnersFor(README.md) = %+v", owners)
	}
	if owners := file.OwnersFor("docs/guide.md"); len(owners) != 1 || owners[0].Team != "docs-team" {
		t.Errorf("OwnersFor(docs/guide.md) = %+v", owners)
	}
	// The most specific (last matching) rule wins entirely.
	if owners := file.OwnersFor("docs/security/policy.md"); len(owners) != 1 || owners[0].Team != "security" {
		t.Errorf("OwnersFor(docs/security/policy.md) = %+v", owners)
	}
	if owners := file.OwnersFor("src/main.go"); len(owners) != 1 || owners[0].Team != "everyone" {
		t.Errorf("OwnersFor(src/main.go) = %+v", owners)
	}
	// No match at all.
	empty := &CodeownersFile{}
	if owners := empty.OwnersFor("x"); owners != nil {
		t.Errorf("OwnersFor on empty file = %+v", owners)
	}
}

// ownersFileForRefs builds a minimal OWNERS file with the given
// maintainer references for resolver tests (mapOwnersSource and its
// loader live in resolver_test.go).
func ownersFileForRefs(refs ...string) *OwnersFile {
	file := &OwnersFile{Version: 1}
	for _, ref := range refs {
		parsed, err := parseOwnerRef(ref)
		if err != nil {
			panic(err)
		}
		file.Maintainers = append(file.Maintainers, parsed)
	}
	return file
}

func TestOwnershipResolverCodeownersFallback(t *testing.T) {
	// No OWNERS files anywhere; CODEOWNERS governs.
	codeowners, err := ParseCodeowners([]byte(`
* @admins
/internal/ @internal-team bob@example.com
`))
	if err != nil {
		t.Fatalf("ParseCodeowners: %v", err)
	}

	resolver := NewOwnershipResolver(mapOwnersSource{}, nil)
	resolver.SetCodeownersProvider(CodeownersProviderFunc(func(ctx context.Context) (*CodeownersFile, error) {
		return codeowners, nil
	}))

	admin := Identity{Subject: "admin@example.com", Groups: []string{"admins"}}
	bob := Identity{Subject: "bob@example.com"}
	internalTeam := Identity{Subject: "it@example.com", Groups: []string{"internal-team"}}
	alice := Identity{Subject: "alice@example.com"}

	if owned, dir, err := resolver.IsOwner(context.Background(), "src/main.go", admin); err != nil || !owned {
		t.Errorf("IsOwner(src, admin) = %v, %q, %v; want owned", owned, dir, err)
	}
	if owned, dir, err := resolver.IsOwner(context.Background(), "internal/db.go", bob); err != nil || !owned {
		t.Errorf("IsOwner(internal, bob) = %v, %q, %v; want owned", owned, dir, err)
	}
	if owned, dir, err := resolver.IsOwner(context.Background(), "internal/db.go", internalTeam); err != nil || !owned {
		t.Errorf("IsOwner(internal, internal-team) = %v, %q, %v; want owned", owned, dir, err)
	}
	if owned, _, err := resolver.IsOwner(context.Background(), "src/main.go", alice); err != nil || owned {
		t.Errorf("IsOwner(src, alice) = %v, %v; want not owned", owned, err)
	}
	if owned, _, err := resolver.IsOwner(context.Background(), "internal/db.go", admin); err != nil || owned {
		// The more specific /internal/ rule overrides the * default,
		// so plain admins do not own internal paths.
		t.Errorf("IsOwner(internal, admin) = %v, %v; want not owned (/internal/ overrides *)", owned, err)
	}
}

func TestOwnershipResolverOwnersWinsOverCodeowners(t *testing.T) {
	// OWNERS governs docs/; CODEOWNERS would give it to a different team.
	codeowners, err := ParseCodeowners([]byte("/docs/ @docs-team\n"))
	if err != nil {
		t.Fatalf("ParseCodeowners: %v", err)
	}

	resolver := NewOwnershipResolver(mapOwnersSource{
		"": ownersFileForRefs("@admins"),
	}, nil)
	resolver.SetCodeownersProvider(CodeownersProviderFunc(func(ctx context.Context) (*CodeownersFile, error) {
		return codeowners, nil
	}))

	// The OWNERS file on the ancestor chain grants ownership via admins.
	admin := Identity{Subject: "admin@example.com", Groups: []string{"admins"}}
	if owned, dir, err := resolver.IsOwner(context.Background(), "docs/x.md", admin); err != nil || !owned || dir != "" {
		t.Errorf("IsOwner(docs, admin) = %v, %q, %v; want owned via root OWNERS", owned, dir, err)
	}

	// CODEOWNERS team alone does not own when OWNERS governs the path.
	docsTeam := Identity{Subject: "dt@example.com", Groups: []string{"docs-team"}}
	if owned, _, err := resolver.IsOwner(context.Background(), "docs/x.md", docsTeam); err != nil || owned {
		t.Errorf("IsOwner(docs, docs-team) = %v, %v; want not owned (OWNERS wins)", owned, err)
	}
}

func TestOwnershipResolverOwnersOfIncludesCodeowners(t *testing.T) {
	codeowners, err := ParseCodeowners([]byte(`
* @admins
/docs/ @docs-team
`))
	if err != nil {
		t.Fatalf("ParseCodeowners: %v", err)
	}

	resolver := NewOwnershipResolver(mapOwnersSource{}, nil)
	resolver.SetCodeownersProvider(CodeownersProviderFunc(func(ctx context.Context) (*CodeownersFile, error) {
		return codeowners, nil
	}))

	refs, err := resolver.OwnersOf(context.Background(), []string{"docs/a.md", "src/b.go"})
	if err != nil {
		t.Fatalf("OwnersOf: %v", err)
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		seen[ref.String()] = true
	}
	if !seen["@docs-team"] || !seen["@admins"] {
		t.Errorf("OwnersOf = %+v, want docs-team and admins", refs)
	}
}

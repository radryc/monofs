package authz

import (
	"fmt"
	"strings"
)

// CodeownersFileNames lists the conventional CODEOWNERS file locations,
// in GitHub's precedence order (docs/ first, then .github/, then root).
var CodeownersFileNames = []string{
	"docs/CODEOWNERS",
	".github/CODEOWNERS",
	"CODEOWNERS",
}

// CodeownersRule is one CODEOWNERS line: a path pattern and the owners
// that apply when the pattern matches. Later rules take precedence over
// earlier ones (GitHub semantics).
type CodeownersRule struct {
	Pattern string
	Owners  []OwnerRef
}

// CodeownersFile is a parsed CODEOWNERS document.
type CodeownersFile struct {
	Rules []CodeownersRule
}

// ParseCodeowners parses a GitHub-style CODEOWNERS document. Every
// non-comment line must contain a pattern followed by at least one
// owner reference. Later rules take precedence when matching.
func ParseCodeowners(data []byte) (*CodeownersFile, error) {
	file := &CodeownersFile{}
	for lineNo, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("authz: CODEOWNERS line %d: expected pattern and at least one owner", lineNo+1)
		}
		pattern := fields[0]
		if pattern == "" {
			return nil, fmt.Errorf("authz: CODEOWNERS line %d: empty pattern", lineNo+1)
		}
		owners := make([]OwnerRef, 0, len(fields)-1)
		for _, field := range fields[1:] {
			ref, err := parseOwnerRef(field)
			if err != nil {
				return nil, fmt.Errorf("authz: CODEOWNERS line %d: %w", lineNo+1, err)
			}
			owners = append(owners, ref)
		}
		file.Rules = append(file.Rules, CodeownersRule{Pattern: pattern, Owners: owners})
	}
	if len(file.Rules) == 0 {
		return nil, fmt.Errorf("authz: CODEOWNERS file has no rules")
	}
	return file, nil
}

// OwnersFor returns the owners of the last matching rule for the path,
// or nil when no rule matches. Later rules win, matching GitHub.
func (f *CodeownersFile) OwnersFor(path string) []OwnerRef {
	if f == nil {
		return nil
	}
	path = strings.Trim(strings.TrimSpace(path), "/")
	var matched []OwnerRef
	for _, rule := range f.Rules {
		if CodeownersPatternMatches(rule.Pattern, path) {
			matched = rule.Owners
		}
	}
	return matched
}

// CodeownersPatternMatches reports whether a CODEOWNERS pattern matches
// a slash-trimmed repo-relative path.
//
// Semantics (documented simplification of GitHub's rules):
//   - A leading "/" anchors the pattern to the repository root.
//   - Without a leading "/", the pattern may match the path relative to
//     any directory (i.e. the pattern also matches any path suffix).
//   - A trailing "/" matches the directory and everything under it.
//   - "**" spans directory separators, "*" and "?" match within a
//     single path segment.
func CodeownersPatternMatches(pattern, path string) bool {
	pattern = strings.TrimSpace(pattern)
	path = strings.Trim(strings.TrimSpace(path), "/")
	if pattern == "" || path == "" {
		return false
	}

	anchored := strings.HasPrefix(pattern, "/")
	// A trailing slash means "this directory and everything below it";
	// detect it before the slash trim below removes it.
	if strings.HasSuffix(pattern, "/") {
		pattern = strings.Trim(pattern, "/") + "/**"
	}
	pattern = strings.Trim(pattern, "/")

	// "*" and "**" are repo-wide catch-alls.
	if pattern == "*" || pattern == "**" {
		return true
	}

	// A wildcard-free pattern that names a directory also matches
	// everything under it (GitHub treats "/docs" like "/docs/").
	if pattern != "" && !strings.ContainsAny(pattern, "*?") {
		if anchored {
			return path == pattern || strings.HasPrefix(path, pattern+"/")
		}
		segments := strings.Split(path, "/")
		for i := range segments {
			suffix := strings.Join(segments[i:], "/")
			if suffix == pattern || strings.HasPrefix(suffix, pattern+"/") {
				return true
			}
		}
		return false
	}

	if anchored {
		return codeownersGlobMatch(pattern, path)
	}

	// Unanchored: match the pattern against the path and every suffix.
	segments := strings.Split(path, "/")
	for i := range segments {
		suffix := strings.Join(segments[i:], "/")
		if codeownersGlobMatch(pattern, suffix) {
			return true
		}
	}
	return false
}

// codeownersGlobMatch implements the "**"/"*"/"?" glob against a full
// relative path.
func codeownersGlobMatch(pattern, path string) bool {
	return codeownersMatchSegments(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

func codeownersMatchSegments(pattern, path []string) bool {
	// "**" consumes zero or more path segments.
	if len(pattern) > 0 && pattern[0] == "**" {
		for skip := 0; skip <= len(path); skip++ {
			if codeownersMatchSegments(pattern[1:], path[skip:]) {
				return true
			}
		}
		return false
	}
	if len(pattern) == 0 || len(path) == 0 {
		return len(pattern) == 0 && len(path) == 0
	}
	if !codeownersMatchSegment(pattern[0], path[0]) {
		return false
	}
	return codeownersMatchSegments(pattern[1:], path[1:])
}

// codeownersMatchSegment matches a single "*" / "?" glob segment.
func codeownersMatchSegment(pattern, segment string) bool {
	// Dynamic programming match over runes.
	pv := []rune(pattern)
	sv := []rune(segment)
	dp := make([]bool, len(sv)+1)
	dp[0] = true
	for _, p := range pv {
		next := make([]bool, len(sv)+1)
		switch p {
		case '*':
			// '*' matches zero or more characters within the segment.
			covered := false
			for j := 0; j <= len(sv); j++ {
				if dp[j] {
					covered = true
				}
				next[j] = covered
			}
		case '?':
			for j := 1; j <= len(sv); j++ {
				next[j] = dp[j-1]
			}
		default:
			for j := 1; j <= len(sv); j++ {
				next[j] = dp[j-1] && sv[j-1] == p
			}
		}
		dp = next
	}
	return dp[len(sv)]
}

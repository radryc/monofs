package fuse

import (
	"fmt"
	"regexp"
	"strings"
)

// RepoFilter restricts which repositories appear in a sparse workspace mount,
// based on client-supplied --include/--exclude display-path globs.
//
// Semantics:
//   - With no include patterns, all repositories are included by default.
//   - With include patterns, only repositories whose display path matches at
//     least one include pattern are included.
//   - Exclude patterns then subtract (applied after include).
//
// Patterns are slash-separated globs where "*" matches within a path segment,
// "**" spans segments, and "?" matches a single character within a segment.
type RepoFilter struct {
	include []string
	exclude []string
}

// NewRepoFilter compiles a repo filter from include and exclude glob lists.
// Either may be empty. Both empty yields an empty filter (mount everything).
func NewRepoFilter(include, exclude []string) (*RepoFilter, error) {
	for _, p := range include {
		if err := validateRepoGlob(p); err != nil {
			return nil, fmt.Errorf("invalid include glob: %w", err)
		}
	}
	for _, p := range exclude {
		if err := validateRepoGlob(p); err != nil {
			return nil, fmt.Errorf("invalid exclude glob: %w", err)
		}
	}
	return &RepoFilter{
		include: append([]string(nil), include...),
		exclude: append([]string(nil), exclude...),
	}, nil
}

func (f *RepoFilter) IsEmpty() bool {
	return f == nil || (len(f.include) == 0 && len(f.exclude) == 0)
}

func (f *RepoFilter) IncludePatterns() []string {
	if f == nil {
		return nil
	}
	return append([]string(nil), f.include...)
}

func (f *RepoFilter) ExcludePatterns() []string {
	if f == nil {
		return nil
	}
	return append([]string(nil), f.exclude...)
}

// Allows reports whether a repository with the given display path should be
// mounted. An empty filter allows everything.
func (f *RepoFilter) Allows(displayPath string) bool {
	if f.IsEmpty() {
		return true
	}
	displayPath = strings.Trim(strings.TrimSpace(displayPath), "/")

	if len(f.include) > 0 {
		included := false
		for _, p := range f.include {
			if repoGlobMatch(p, displayPath) {
				included = true
				break
			}
		}
		if !included {
			return false
		}
	}

	for _, p := range f.exclude {
		if repoGlobMatch(p, displayPath) {
			return false
		}
	}
	return true
}

func validateRepoGlob(pattern string) error {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return fmt.Errorf("empty glob")
	}
	if strings.ContainsAny(p, "\x00") {
		return fmt.Errorf("globs must not contain null bytes")
	}
	return nil
}

// repoGlobMatch matches a full display path against a glob. "*" matches within
// a segment, "**" spans segments, "?" matches a single character within a
// segment. A trailing "/*" is treated as "/**" — i.e. a directory include (or
// exclude) matches the directory and everything beneath it.
func repoGlobMatch(pattern, path string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if pattern == "**" || pattern == "*" {
		return path != ""
	}
	// A trailing "/*" is shorthand for the whole subtree ("/**").
	if strings.HasSuffix(pattern, "/*") {
		pattern = strings.TrimSuffix(pattern, "*") + "**"
	}

	var sb strings.Builder
	sb.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				sb.WriteString(".*")
				i++
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString("[^/]")
		case '.', '(', ')', '[', ']', '{', '}', '\\', '^', '$', '+', '|':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		default:
			sb.WriteByte(c)
		}
	}
	sb.WriteString("$")

	matched, err := regexp.MatchString(sb.String(), path)
	return err == nil && matched
}

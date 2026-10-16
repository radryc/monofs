package policy

import (
	"path"
	"strings"
)

// pathAllowed reports whether displayPath matches any of the patterns.
// Supported patterns: "*" (everything), "prefix/**" (prefix and subtree),
// otherwise shell-style globs via path.Match.
func pathAllowed(patterns []string, displayPath string) bool {
	displayPath = strings.Trim(strings.TrimSpace(displayPath), "/")
	if displayPath == "" {
		return false
	}
	for _, pattern := range patterns {
		p := strings.Trim(strings.TrimSpace(pattern), "/")
		if p == "" {
			continue
		}
		if p == "*" {
			return true
		}
		if strings.HasSuffix(p, "/**") {
			prefix := strings.TrimSuffix(p, "/**")
			if displayPath == prefix || strings.HasPrefix(displayPath, prefix+"/") {
				return true
			}
			continue
		}
		if ok, err := path.Match(p, displayPath); err == nil && ok {
			return true
		}
	}
	return false
}

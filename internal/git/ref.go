package git

import (
	"regexp"
	"strings"
)

// RefKind classifies a --ref / ingest ref string so the clone and tree-walk
// paths know how to resolve it.
type RefKind int

const (
	// RefNamed is a plain ref name (a branch or a tag) that must be resolved
	// against the remote/local repository.
	RefNamed RefKind = iota
	// RefTag is an explicit tag reference (refs/tags/...).
	RefTag
	// RefSHA is a full 40- or 64-hex commit SHA.
	RefSHA
)

// refNameRegex matches a bare git ref name (no slashes-in-refs prefix and no
// SHA form). Used only to fall through to "named" classification.
var fullSHARe = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

// ClassifyRef classifies a ref string. Leading "refs/tags/", "refs/heads/",
// and "refs/remotes/" prefixes are honored; a full-hash string is a SHA;
// everything else is a named ref (branch or tag).
func ClassifyRef(ref string) RefKind {
	ref = strings.TrimSpace(ref)
	if fullSHARe.MatchString(ref) {
		return RefSHA
	}
	switch {
	case strings.HasPrefix(ref, "refs/tags/"):
		return RefTag
	case strings.HasPrefix(ref, "refs/heads/"), strings.HasPrefix(ref, "refs/remotes/"):
		return RefNamed
	default:
		return RefNamed
	}
}

// IsHexSHA reports whether ref is a full 40- or 64-hex commit SHA.
func IsHexSHA(ref string) bool {
	return fullSHARe.MatchString(strings.TrimSpace(ref))
}

// TrimRefPrefix strips a leading "refs/tags/" or "refs/heads/" prefix so the
// bare name can go into plumbing.NewTagReferenceName / NewBranchReferenceName.
func TrimRefPrefix(ref string) string {
	ref = strings.TrimSpace(ref)
	for _, prefix := range []string{"refs/tags/", "refs/heads/", "refs/remotes/"} {
		if strings.HasPrefix(ref, prefix) {
			return strings.TrimPrefix(ref, prefix)
		}
	}
	return ref
}

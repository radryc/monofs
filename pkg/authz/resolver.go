package authz

import (
	"context"
	"strings"
)

// OwnersSource loads the OWNERS file governing a directory. Implementations may
// read from MonoFS storage, a local checkout, or an in-memory map. A directory
// without an OWNERS file returns (nil, nil).
type OwnersSource interface {
	LoadOwners(ctx context.Context, dir string) (*OwnersFile, error)
}

// OwnersPath returns the conventional OWNERS file path for a directory:
// "<dir>/.guardian/OWNERS" (or ".guardian/OWNERS" at the root).
func OwnersPath(dir string) string {
	dir = strings.Trim(strings.TrimSpace(dir), "/")
	if dir == "" {
		return OwnersDir + "/" + OwnersFileName
	}
	return dir + "/" + OwnersDir + "/" + OwnersFileName
}

// ancestorDirs returns the path itself and each ancestor directory, from the
// deepest to the root (""), e.g. "a/b/c" -> ["a/b/c", "a/b", "a", ""].
func ancestorDirs(path string) []string {
	p := strings.Trim(strings.TrimSpace(path), "/")
	var dirs []string
	for {
		dirs = append(dirs, p)
		if p == "" {
			break
		}
		if idx := strings.LastIndexByte(p, '/'); idx >= 0 {
			p = p[:idx]
		} else {
			p = ""
		}
	}
	return dirs
}

// ownerRefMatchesIdentity reports whether id satisfies an owner reference,
// resolving team handles to IdP groups via mapping.
func ownerRefMatchesIdentity(ref OwnerRef, id Identity, mapping TeamMapping) bool {
	if id.IsAnonymous() {
		return false
	}
	if ref.IsTeam() {
		group, _ := mapping.GroupFor(ref.Team)
		return id.HasGroup(group)
	}
	s := ref.Subject
	return s == id.Subject || s == id.ClientID || (id.Email != "" && s == id.Email)
}

// OwnershipResolver decides subtree ownership using nested OWNERS files. An
// identity owns a path if it is listed as a maintainer in the nearest governing
// OWNERS file or in any ancestor OWNERS file (parent owners retain authority).
// When no OWNERS file governs a path and a CODEOWNERS provider is configured,
// CODEOWNERS rules are consulted as a fallback (OWNERS wins over CODEOWNERS).
type OwnershipResolver struct {
	source  OwnersSource
	mapping TeamMapping
	// codeowners, when set, supplies a repository-level CODEOWNERS
	// document used as an ownership fallback.
	codeowners CodeownersProvider
}

// CodeownersProvider loads the repository-level CODEOWNERS document. A
// repository without CODEOWNERS returns (nil, nil).
type CodeownersProvider interface {
	LoadCodeowners(ctx context.Context) (*CodeownersFile, error)
}

// CodeownersProviderFunc adapts a function into a CodeownersProvider.
type CodeownersProviderFunc func(ctx context.Context) (*CodeownersFile, error)

// LoadCodeowners implements CodeownersProvider.
func (f CodeownersProviderFunc) LoadCodeowners(ctx context.Context) (*CodeownersFile, error) {
	return f(ctx)
}

// NewOwnershipResolver builds a resolver over the given OWNERS source and team
// mapping.
func NewOwnershipResolver(source OwnersSource, mapping TeamMapping) *OwnershipResolver {
	return &OwnershipResolver{source: source, mapping: mapping}
}

// SetCodeownersProvider installs a CODEOWNERS fallback provider.
func (r *OwnershipResolver) SetCodeownersProvider(provider CodeownersProvider) {
	r.codeowners = provider
}

// IsOwner reports whether id may modify path directly. When owned, ownerDir is
// the deepest OWNERS directory that granted ownership.
func (r *OwnershipResolver) IsOwner(ctx context.Context, path string, id Identity) (owned bool, ownerDir string, err error) {
	// governed records whether any OWNERS file exists on the ancestor
	// chain; CODEOWNERS is only consulted when no OWNERS file governs
	// the path (OWNERS wins).
	governed := false
	for _, dir := range ancestorDirs(path) {
		owners, err := r.source.LoadOwners(ctx, dir)
		if err != nil {
			return false, "", err
		}
		if owners == nil {
			continue
		}
		governed = true
		for _, ref := range owners.Maintainers {
			if ownerRefMatchesIdentity(ref, id, r.mapping) {
				return true, dir, nil
			}
		}
	}
	if governed {
		return false, "", nil
	}
	// CODEOWNERS fallback.
	if r.codeowners != nil {
		codeowners, err := r.codeowners.LoadCodeowners(ctx)
		if err != nil {
			return false, "", err
		}
		if codeowners != nil {
			for _, ref := range codeowners.OwnersFor(path) {
				if ownerRefMatchesIdentity(ref, id, r.mapping) {
					return true, "CODEOWNERS", nil
				}
			}
		}
	}
	return false, "", nil
}

// Governed reports whether any OWNERS or CODEOWNERS rule governs path,
// regardless of who owns it. Ungoverned paths are open by default (no owners
// means no review requirement); callers use this to distinguish "governed but
// foreign" (review required) from "ungoverned" (open).
func (r *OwnershipResolver) Governed(ctx context.Context, path string) (bool, error) {
	for _, dir := range ancestorDirs(path) {
		owners, err := r.source.LoadOwners(ctx, dir)
		if err != nil {
			return false, err
		}
		if owners != nil {
			return true, nil
		}
	}
	if r.codeowners != nil {
		codeowners, err := r.codeowners.LoadCodeowners(ctx)
		if err != nil {
			return false, err
		}
		if codeowners != nil && len(codeowners.OwnersFor(path)) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// OwnsAll reports whether id owns every path (may modify all directly). It also
// returns the subset of paths that id does not own, which callers use to route
// changes through a merge request.
func (r *OwnershipResolver) OwnsAll(ctx context.Context, paths []string, id Identity) (ownsAll bool, unowned []string, err error) {
	for _, p := range paths {
		owned, _, err := r.IsOwner(ctx, p, id)
		if err != nil {
			return false, nil, err
		}
		if !owned {
			unowned = append(unowned, p)
		}
	}
	return len(unowned) == 0, unowned, nil
}

// OwnersOf returns the distinct maintainer references that govern any of the
// given paths (from the nearest and ancestor OWNERS files). Callers use this to
// assign reviewers to a merge request. Order is deterministic (path order, then
// deepest-to-root, first occurrence wins). CODEOWNERS owners are included for
// paths that no OWNERS file governs.
func (r *OwnershipResolver) OwnersOf(ctx context.Context, paths []string) ([]OwnerRef, error) {
	seen := make(map[string]bool)
	var refs []OwnerRef
	addRefs := func(candidates []OwnerRef) {
		for _, ref := range candidates {
			key := ref.String()
			if !seen[key] {
				seen[key] = true
				refs = append(refs, ref)
			}
		}
	}

	var codeowners *CodeownersFile
	if r.codeowners != nil {
		var err error
		codeowners, err = r.codeowners.LoadCodeowners(ctx)
		if err != nil {
			return nil, err
		}
	}

	for _, p := range paths {
		governed := false
		for _, dir := range ancestorDirs(p) {
			owners, err := r.source.LoadOwners(ctx, dir)
			if err != nil {
				return nil, err
			}
			if owners == nil {
				continue
			}
			governed = true
			addRefs(owners.Maintainers)
		}
		if !governed && codeowners != nil {
			addRefs(codeowners.OwnersFor(p))
		}
	}
	return refs, nil
}

package router

import (
	"context"
	"fmt"
	"strings"

	"github.com/radryc/monofs/internal/router/mergerequest"
	"github.com/radryc/monofs/pkg/authz"
)

// ConfigureOwnership wires the subtree-ownership resolver and the native
// merge-request store onto the router. Passing nil disables both. This must be
// called before the router begins serving requests.
func (r *Router) ConfigureOwnership(resolver *authz.OwnershipResolver) {
	if resolver == nil {
		r.ownershipResolver = nil
		r.ownershipResolverFull = nil
		r.mergeRequests = nil
		return
	}
	r.ownershipResolver = resolver
	r.ownershipResolverFull = resolver
	r.mergeRequests = mergerequest.NewStore(resolver)
}

// ownershipResolverRef returns the concrete resolver, if configured.
func (r *Router) ownershipResolverRef() *authz.OwnershipResolver {
	return r.ownershipResolverFull
}

// mergeRequestStore returns the native proposal store, or nil when the
// ownership gate has not been configured.
func (r *Router) mergeRequestStore() *mergerequest.Store {
	return r.mergeRequests
}

// EnableOwnershipGate configures the router to enforce the subtree-ownership
// review gate, loading OWNERS files from the managed partition store. mapping
// resolves OWNERS "@team" handles to IdP group names (may be empty).
func (r *Router) EnableOwnershipGate(mapping authz.TeamMapping) {
	resolver := authz.NewOwnershipResolver(routerOwnersSource{r: r}, mapping)
	r.ConfigureOwnership(resolver)
	r.logger.Info("ownership gate enabled")
}

// routerOwnersSource loads OWNERS files from the guardian/managed partition
// store on the router. A directory without an OWNERS file returns (nil, nil),
// which the resolver treats as "ungoverned" (open by default).
type routerOwnersSource struct {
	r *Router
}

// LoadOwners implements authz.OwnersSource by reading "<dir>/.guardian/OWNERS"
// through the router's guardian path store.
func (s routerOwnersSource) LoadOwners(ctx context.Context, dir string) (*authz.OwnersFile, error) {
	dir = strings.Trim(strings.TrimSpace(dir), "/")
	if dir == "" {
		// Root-level OWNERS (".guardian/OWNERS") is not inside a managed
		// partition; treat it as absent.
		return nil, nil
	}
	ownersDisplay := dir + "/" + authz.OwnersDir + "/" + authz.OwnersFileName
	displayPath, relative, ok := splitManagedDisplayPath(ownersDisplay)
	if !ok {
		return nil, nil
	}
	logical, err := guardianLogicalPathFromPhysical(displayPath, relative)
	if err != nil {
		return nil, nil
	}

	content, _, err := s.r.readPipelinePath(logical)
	if err != nil {
		// "not found" (or otherwise unreadable) -> no OWNERS for this dir.
		return nil, nil
	}
	owners, err := authz.ParseOwners(content)
	if err != nil {
		return nil, fmt.Errorf("router: parse OWNERS %q: %w", logical, err)
	}
	return owners, nil
}

// splitManagedDisplayPath splits a full managed display path (e.g.
// "guardian/doctor/.guardian/OWNERS") into its partition display path
// ("guardian/doctor") and relative path (".guardian/OWNERS"). Returns ok=false
// when the path is not under a managed namespace.
func splitManagedDisplayPath(full string) (displayPath, relative string, ok bool) {
	full = strings.Trim(strings.TrimSpace(full), "/")
	if full == "" {
		return "", "", false
	}
	parts := strings.Split(full, "/")
	switch parts[0] {
	case "guardian":
		if len(parts) < 2 {
			return "", "", false
		}
		return "guardian/" + parts[1], strings.Join(parts[2:], "/"), true
	case "doctor":
		if len(parts) < 2 {
			return "", "", false
		}
		return "doctor/" + parts[1], strings.Join(parts[2:], "/"), true
	case "guardian-system":
		return "guardian-system", strings.Join(parts[1:], "/"), true
	default:
		return "", "", false
	}
}

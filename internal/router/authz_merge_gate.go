package router

import (
	"context"
	"fmt"
	"strings"

	"github.com/radryc/monofs/internal/workspacebundle"
	"github.com/radryc/monofs/pkg/authz"
)

// MergeDecision is the outcome of the subtree-ownership gate for a modify
// (publish / source-push) action.
type MergeDecision int

const (
	// MergeDecisionDirect means the caller owns all affected subtrees and may
	// write directly.
	MergeDecisionDirect MergeDecision = iota
	// MergeDecisionMergeRequest means the caller does not own one or more
	// affected subtrees and the change must be routed through a merge request.
	MergeDecisionMergeRequest
)

// String renders the decision for logs and events.
func (d MergeDecision) String() string {
	switch d {
	case MergeDecisionDirect:
		return "direct"
	case MergeDecisionMergeRequest:
		return "merge_request"
	default:
		return "unknown"
	}
}

// ownershipChecker abstracts *authz.OwnershipResolver so the gate can be tested
// with a fake and disabled by leaving it nil.
type ownershipChecker interface {
	OwnsAll(ctx context.Context, paths []string, id authz.Identity) (ownsAll bool, unowned []string, err error)
}

// SetOwnershipResolver installs the subtree-ownership resolver used by the
// merge-request gate (authz epic D). Passing nil disables the gate.
func (r *Router) SetOwnershipResolver(checker ownershipChecker) {
	r.ownershipResolver = checker
}

// evaluateSubtreeOwnership decides whether the identity in ctx may modify the
// given paths directly or must open a merge request. When no resolver is
// configured it defaults to MergeDecisionDirect so behavior is unchanged.
//
// It returns the decision plus the subset of paths the caller does not own,
// which D2/D3 use to build the merge request and assign reviewers.
func (r *Router) evaluateSubtreeOwnership(ctx context.Context, paths []string) (MergeDecision, []string, error) {
	if r.ownershipResolver == nil || len(paths) == 0 {
		return MergeDecisionDirect, nil, nil
	}
	id, _ := authz.IdentityFromContext(ctx)
	ownsAll, unowned, err := r.ownershipResolver.OwnsAll(ctx, paths, id)
	if err != nil {
		return MergeDecisionDirect, nil, err
	}
	if ownsAll {
		return MergeDecisionDirect, nil, nil
	}
	return MergeDecisionMergeRequest, unowned, nil
}

// enforceOwnershipGate applies the subtree-ownership review gate to a source
// push. It only denies when the push is a direct push (empty logical branch:
// the target branch equals the repository's default branch, so no pull request
// is opened) AND the principal does not own one or more governed subtrees.
// Non-direct pushes are allowed because review happens at the forge via the
// pull request that Phase 2 opens automatically.
//
// Ungoverned paths (no OWNERS/CODEOWNERS rule anywhere) are open by default.
func (r *Router) enforceOwnershipGate(ctx context.Context, logicalBranch string, bundle *workspacebundle.SourceCommitBundle) error {
	if !r.config.OwnershipGateEnabled {
		return nil
	}
	if bundle == nil {
		return nil
	}
	if strings.TrimSpace(logicalBranch) != "" {
		// Non-direct strategy: PR review happens at the forge.
		return nil
	}

	paths := changedPathsFromSourceBundle(bundle)
	if len(paths) == 0 {
		return nil
	}

	decision, unowned, err := r.evaluateSubtreeOwnership(ctx, paths)
	if err != nil {
		// Default-open on resolution failure (mirrors the disabled gate).
		r.logger.Warn("ownership gate resolution failed, allowing push", "error", err)
		return nil
	}
	if decision == MergeDecisionDirect {
		return nil
	}

	// Filter to only the *governed* unowned paths; ungoverned paths are open.
	governed := unowned[:0]
	for _, p := range unowned {
		g, err := r.ownershipResolverFull.Governed(ctx, p)
		if err != nil {
			r.logger.Warn("ownership gate governance check failed", "path", p, "error", err)
			governed = append(governed, p)
			continue
		}
		if g {
			governed = append(governed, p)
		}
	}
	if len(governed) == 0 {
		return nil
	}

	owners := r.ownersForUnowned(ctx, governed)
	return fmt.Errorf("review required: %s owned by %s; use a logical branch so a pull request is opened",
		strings.Join(governed, ", "), strings.Join(owners, ", "))
}

// ownersForUnowned returns the maintainer references that govern the given
// paths, for use in deny reasons and reviewer assignment.
func (r *Router) ownersForUnowned(ctx context.Context, paths []string) []string {
	resolver := r.ownershipResolverRef()
	if resolver == nil {
		return nil
	}
	refs, err := resolver.OwnersOf(ctx, paths)
	if err != nil {
		r.logger.Warn("ownership gate could not resolve owners", "error", err)
		return nil
	}
	return reviewersFromOwnerRefs(refs)
}

// changedPathsFromSourceBundle collects the distinct absolute display paths
// (repository display path + operation path) touched by a source commit bundle,
// used to evaluate subtree ownership for the merge-request gate.
func changedPathsFromSourceBundle(bundle *workspacebundle.SourceCommitBundle) []string {
	if bundle == nil {
		return nil
	}
	seen := make(map[string]bool)
	var paths []string
	for _, commit := range bundle.Commits {
		for _, repo := range commit.Repositories {
			base := strings.Trim(repo.DisplayPath, "/")
			for _, op := range repo.Operations {
				full := base
				if p := strings.Trim(op.Path, "/"); p != "" {
					if full != "" {
						full += "/" + p
					} else {
						full = p
					}
				}
				if full != "" && !seen[full] {
					seen[full] = true
					paths = append(paths, full)
				}
			}
		}
	}
	return paths
}

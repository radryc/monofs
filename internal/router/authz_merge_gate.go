package router

import (
	"context"
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

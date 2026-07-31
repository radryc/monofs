package router

import (
	"context"
	"fmt"
	"strings"

	"github.com/radryc/monofs/internal/router/workspacepr"
	"github.com/radryc/monofs/pkg/authz"
)

// externalMergeRequest describes a pull/merge request to open on an external git
// provider (GitHub/GitLab) for a non-owner change (authz epic D2).
type externalMergeRequest struct {
	RepoCloneURL string
	SourceBranch string
	TargetBranch string
	Title        string
	UnownedPaths []string
	Reviewers    []string // owner handles/ids requested for review
}

// reviewersFromOwnerRefs renders owner references as reviewer strings.
func reviewersFromOwnerRefs(refs []authz.OwnerRef) []string {
	reviewers := make([]string, 0, len(refs))
	for _, ref := range refs {
		reviewers = append(reviewers, ref.String())
	}
	return reviewers
}

// buildPRRequest renders a CreatePRRequest, embedding the unowned paths and the
// requested reviewers in the body (the provider interface has no reviewer field).
func buildPRRequest(mr externalMergeRequest) workspacepr.CreatePRRequest {
	var b strings.Builder
	b.WriteString("Automated merge request: the author does not own all modified subtrees, ")
	b.WriteString("so this change requires owner review before it can land.\n")
	if len(mr.UnownedPaths) > 0 {
		b.WriteString("\nPaths requiring owner review:\n")
		for _, p := range mr.UnownedPaths {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}
	if len(mr.Reviewers) > 0 {
		fmt.Fprintf(&b, "\nRequested reviewers: %s\n", strings.Join(mr.Reviewers, ", "))
	}
	title := mr.Title
	if strings.TrimSpace(title) == "" {
		title = "Merge request: owner review required"
	}
	return workspacepr.CreatePRRequest{
		RepoCloneURL: mr.RepoCloneURL,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
		Title:        title,
		Body:         b.String(),
	}
}

// openExternalMergeRequest opens a PR/MR via the given provider.
func openExternalMergeRequest(ctx context.Context, provider workspacepr.PullRequestProvider, mr externalMergeRequest) (*workspacepr.CreatePRResult, error) {
	if provider == nil {
		return nil, fmt.Errorf("no pull-request provider configured for %s", mr.RepoCloneURL)
	}
	return provider.Create(ctx, buildPRRequest(mr))
}

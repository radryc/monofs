package router

import (
	"context"
	"fmt"
	"os"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/router/workspacepr"
	"github.com/radryc/monofs/internal/workspacebundle"
)

// attachPullRequest opens a pull request (or merge request) for a
// repository that was pushed to a review branch (workspace_branch or
// per_repo_branch strategy). The repository result is annotated with
// the PR URL so clients and the ledger can surface it. Failures are
// logged but never fail the push itself.
func (r *Router) attachPullRequest(ctx context.Context, repoResult *pb.WorkspaceSyncRepositoryResult, bundle *workspacebundle.SourceCommitBundle, jobID string) {
	if repoResult == nil {
		return
	}
	sourceBranch := strings.TrimSpace(repoResult.GetTargetBranch())
	targetBranch := strings.TrimSpace(repoResult.GetBranch())
	if sourceBranch == "" || targetBranch == "" || sourceBranch == targetBranch {
		return
	}
	if repoResult.GetStatus() != pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_PUBLISHED {
		return
	}

	provider, err := workspacepr.DetectProvider(
		repoResult.GetRepoUrl(),
		os.Getenv("MONOFS_GITLAB_BASE_URL"),
		os.Getenv("MONOFS_GITHUB_TOKEN"),
		os.Getenv("MONOFS_GITLAB_TOKEN"),
	)
	if err != nil {
		r.logger.Warn("pull request provider unavailable",
			"repo", repoResult.GetDisplayPath(), "error", err)
		return
	}

	title, body := pullRequestBody(bundle, jobID, repoResult)
	result, err := provider.Create(ctx, workspacepr.CreatePRRequest{
		RepoCloneURL: repoResult.GetRepoUrl(),
		SourceBranch: sourceBranch,
		TargetBranch: targetBranch,
		Title:        title,
		Body:         body,
	})
	if err != nil {
		r.logger.Warn("pull request creation failed",
			"repo", repoResult.GetDisplayPath(), "provider", provider.ProviderName(), "error", err)
		return
	}

	repoResult.PullRequestUrl = result.WebURL
	repoResult.PullRequestCreated = result.Created
	if result.Created {
		repoResult.Message = strings.TrimSpace(strings.TrimSuffix(repoResult.GetMessage(), "\n") + "; pull request created: " + result.WebURL)
		r.logger.Info("pull request created",
			"repo", repoResult.GetDisplayPath(),
			"provider", provider.ProviderName(),
			"url", result.WebURL)

		// Review the change by the maintainers of the touched subtrees
		// (OWNERS/CODEOWNERS), best-effort.
		r.requestPullReviewers(ctx, provider, repoResult, result, bundle)
	} else {
		repoResult.Message = strings.TrimSpace(strings.TrimSuffix(repoResult.GetMessage(), "\n") + "; open pull request: " + result.WebURL)
	}
}

// requestPullReviewers asks the forge to review an opened pull request by the
// maintainers of the changed subtrees. It is best-effort: no OWNERS data, a
// provider without a token, or a forge error are all logged and skipped.
func (r *Router) requestPullReviewers(ctx context.Context, provider workspacepr.PullRequestProvider, repoResult *pb.WorkspaceSyncRepositoryResult, result *workspacepr.CreatePRResult, bundle *workspacebundle.SourceCommitBundle) {
	if result == nil || !result.Created || result.ID == "" {
		return
	}
	resolver := r.ownershipResolverRef()
	if resolver == nil {
		return
	}
	paths := changedRepoPaths(repoResult.GetStorageId(), bundle)
	if len(paths) == 0 {
		return
	}
	refs, err := resolver.OwnersOf(ctx, paths)
	if err != nil {
		r.logger.Warn("could not resolve owners for review", "repo", repoResult.GetDisplayPath(), "error", err)
		return
	}
	reviewers := reviewersFromOwnerRefs(refs)
	if len(reviewers) == 0 {
		return
	}
	if err := provider.RequestReviewers(ctx, workspacepr.ReviewersRequest{
		RepoCloneURL: repoResult.GetRepoUrl(),
		PRNumber:     result.ID,
		Reviewers:    reviewers,
	}); err != nil {
		r.logger.Warn("review request failed",
			"repo", repoResult.GetDisplayPath(), "provider", provider.ProviderName(), "error", err)
	}
}

// changedRepoPaths returns the distinct repo-relativized display paths touched
// by a source commit bundle for a single repository storageID.
func changedRepoPaths(storageID string, bundle *workspacebundle.SourceCommitBundle) []string {
	if bundle == nil || storageID == "" {
		return nil
	}
	seen := make(map[string]bool)
	var paths []string
	for _, commit := range bundle.Commits {
		for _, repo := range commit.Repositories {
			if repo.StorageID != storageID {
				continue
			}
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

func pullRequestBody(bundle *workspacebundle.SourceCommitBundle, jobID string, repoResult *pb.WorkspaceSyncRepositoryResult) (string, string) {
	title := fmt.Sprintf("MonoFS workspace push (%s)", jobID)
	var lines []string
	if bundle != nil {
		for _, commit := range bundle.Commits {
			for _, repo := range commit.Repositories {
				if repo.StorageID != repoResult.GetStorageId() {
					continue
				}
				if msg := strings.TrimSpace(commit.Message); msg != "" {
					title = msg
				}
				lines = append(lines, fmt.Sprintf("- %s (%d operation(s))", commit.ID, len(repo.Operations)))
			}
		}
	}

	var body strings.Builder
	body.WriteString("Pushed from a MonoFS virtual monorepo workspace.\n\n")
	body.WriteString("Commits:\n")
	if len(lines) == 0 {
		body.WriteString("- (none recorded)\n")
	} else {
		for _, line := range lines {
			body.WriteString(line + "\n")
		}
	}
	body.WriteString(fmt.Sprintf("\nMonoFS-Job: %s\nMonoFS-Workspace: %s\n", jobID, repoResult.GetStorageId()))
	return title, body.String()
}

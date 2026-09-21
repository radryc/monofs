package fetcher

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"

	pb "github.com/radryc/monofs/api/proto"
)

const (
	maxUpstreamLogEntries = 200
	defaultUpstreamLogLen = 50
	maxUpstreamBlameBytes = 1 << 20 // 1 MiB file-size cap for blame
)

// upstreamRepoID derives a stable cache key from a repository URL.
func upstreamRepoID(repoURL string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(repoURL)))
	return fmt.Sprintf("upstream-%x", sum[:8])
}

// openUpstreamRepo opens (or clones with full history) the repository for
// log/tags/blame reads.
func (s *Service) openUpstreamRepo(ctx context.Context, repoURL, branch string) (*gogit.Repository, error) {
	if s.repoMgr == nil {
		return nil, fmt.Errorf("sync worker git cache is not configured")
	}
	if strings.TrimSpace(repoURL) == "" {
		return nil, fmt.Errorf("repo_url is required")
	}
	if branch == "" {
		branch = "main"
	}
	return s.repoMgr.CloneOrOpenHistory(ctx, repoURL, upstreamRepoID(repoURL), branch)
}

// GetUpstreamLog returns recent commits on an upstream branch.
func (s *Service) GetUpstreamLog(ctx context.Context, req *pb.UpstreamLogRequest) (*pb.UpstreamLogResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultUpstreamLogLen
	}
	if limit > maxUpstreamLogEntries {
		limit = maxUpstreamLogEntries
	}

	repo, err := s.openUpstreamRepo(ctx, req.GetRepoUrl(), req.GetBranch())
	if err != nil {
		return nil, err
	}

	head, err := s.repoMgr.ResolveCommit(repo, req.GetBranch())
	if err != nil {
		return nil, fmt.Errorf("resolve branch %q: %w", req.GetBranch(), err)
	}

	iter, err := repo.Log(&gogit.LogOptions{From: head})
	if err != nil {
		return nil, fmt.Errorf("walk commit history: %w", err)
	}

	resp := &pb.UpstreamLogResponse{}
	count := 0
	err = iter.ForEach(func(c *object.Commit) error {
		if count >= limit {
			return storer.ErrStop
		}
		resp.Entries = append(resp.Entries, &pb.UpstreamLogEntry{
			Hash:           c.Hash.String(),
			AuthorName:     c.Author.Name,
			AuthorEmail:    c.Author.Email,
			AuthoredAtUnix: c.Author.When.Unix(),
			Message:        strings.TrimSpace(c.Message),
		})
		count++
		return nil
	})
	if err != nil && err != storer.ErrStop {
		return nil, fmt.Errorf("collect commits: %w", err)
	}
	return resp, nil
}

// GetUpstreamTags lists the tags on an upstream repository with their resolved
// commit hashes.
func (s *Service) GetUpstreamTags(ctx context.Context, req *pb.UpstreamTagsRequest) (*pb.UpstreamTagsResponse, error) {
	repo, err := s.openUpstreamRepo(ctx, req.GetRepoUrl(), "main")
	if err != nil {
		return nil, err
	}

	tagIter, err := repo.Tags()
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}

	resp := &pb.UpstreamTagsResponse{}
	err = tagIter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		hash := ref.Hash()
		var taggedAt int64
		// Annotated tags carry a tagger timestamp; lightweight tags use the
		// commit's timestamp.
		if tag, err := repo.TagObject(hash); err == nil {
			taggedAt = tag.Tagger.When.Unix()
			hash = tag.Target
		} else if commit, err := repo.CommitObject(hash); err == nil {
			taggedAt = commit.Committer.When.Unix()
		}
		resp.Tags = append(resp.Tags, &pb.UpstreamTag{
			Name:         name,
			CommitHash:   hash.String(),
			TaggedAtUnix: taggedAt,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("collect tags: %w", err)
	}
	return resp, nil
}

// GetUpstreamBlame returns line-level blame for a file on an upstream branch.
func (s *Service) GetUpstreamBlame(ctx context.Context, req *pb.UpstreamBlameRequest) (*pb.UpstreamBlameResponse, error) {
	if strings.TrimSpace(req.GetPath()) == "" {
		return nil, fmt.Errorf("path is required")
	}
	repo, err := s.openUpstreamRepo(ctx, req.GetRepoUrl(), req.GetBranch())
	if err != nil {
		return nil, err
	}

	head, err := s.repoMgr.ResolveCommit(repo, req.GetBranch())
	if err != nil {
		return nil, fmt.Errorf("resolve branch %q: %w", req.GetBranch(), err)
	}
	commit, err := repo.CommitObject(head)
	if err != nil {
		return nil, fmt.Errorf("load commit: %w", err)
	}

	cleanPath := filepath.ToSlash(filepath.Clean(req.GetPath()))
	cleanPath = strings.TrimPrefix(cleanPath, "/")

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("load tree: %w", err)
	}
	file, err := tree.File(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("file %q not found at %s: %w", cleanPath, req.GetBranch(), err)
	}
	if file.Size > maxUpstreamBlameBytes {
		return nil, fmt.Errorf("file %q exceeds %d bytes, refusing blame", cleanPath, maxUpstreamBlameBytes)
	}

	result, err := gogit.Blame(commit, cleanPath)
	if err != nil {
		return nil, fmt.Errorf("blame %q: %w", cleanPath, err)
	}

	resp := &pb.UpstreamBlameResponse{}
	for i, line := range result.Lines {
		resp.Lines = append(resp.Lines, &pb.UpstreamBlameLine{
			LineNo:         int32(i + 1),
			Hash:           line.Hash.String(),
			AuthorName:     line.AuthorName,
			AuthoredAtUnix: line.Date.Unix(),
			Content:        line.Text,
		})
	}
	return resp, nil
}

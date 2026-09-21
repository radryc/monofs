package client

import (
	"context"
	"fmt"

	pb "github.com/radryc/monofs/api/proto"
)

// UpstreamLogEntry is a single commit in an upstream branch's history.
type UpstreamLogEntry struct {
	Hash        string
	AuthorName  string
	AuthorEmail string
	AuthoredAt  int64
	Message     string
}

// UpstreamTag is a tag name and its resolved commit hash.
type UpstreamTag struct {
	Name       string
	CommitHash string
	TaggedAt   int64
}

// UpstreamBlameLine is one line of a file with its authorship.
type UpstreamBlameLine struct {
	LineNo     int
	Hash       string
	Author     string
	AuthoredAt int64
	Content    string
}

// UpstreamLog returns recent commits on an upstream branch via the router.
func (sc *ShardedClient) UpstreamLog(ctx context.Context, repoURL, branch string, limit int) ([]UpstreamLogEntry, error) {
	callCtx, cancel, err := sc.upstreamCallContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	resp, err := sc.routerClient.GetUpstreamLog(callCtx, &pb.UpstreamLogRequest{
		RepoUrl: repoURL,
		Branch:  branch,
		Limit:   int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("upstream log: %w", err)
	}
	out := make([]UpstreamLogEntry, 0, len(resp.GetEntries()))
	for _, e := range resp.GetEntries() {
		out = append(out, UpstreamLogEntry{
			Hash:        e.GetHash(),
			AuthorName:  e.GetAuthorName(),
			AuthorEmail: e.GetAuthorEmail(),
			AuthoredAt:  e.GetAuthoredAtUnix(),
			Message:     e.GetMessage(),
		})
	}
	return out, nil
}

// UpstreamTags lists tags on an upstream repository via the router.
func (sc *ShardedClient) UpstreamTags(ctx context.Context, repoURL string) ([]UpstreamTag, error) {
	callCtx, cancel, err := sc.upstreamCallContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	resp, err := sc.routerClient.GetUpstreamTags(callCtx, &pb.UpstreamTagsRequest{RepoUrl: repoURL})
	if err != nil {
		return nil, fmt.Errorf("upstream tags: %w", err)
	}
	out := make([]UpstreamTag, 0, len(resp.GetTags()))
	for _, t := range resp.GetTags() {
		out = append(out, UpstreamTag{
			Name:       t.GetName(),
			CommitHash: t.GetCommitHash(),
			TaggedAt:   t.GetTaggedAtUnix(),
		})
	}
	return out, nil
}

// UpstreamBlame returns line-level blame for a file on an upstream branch via
// the router.
func (sc *ShardedClient) UpstreamBlame(ctx context.Context, repoURL, branch, path string) ([]UpstreamBlameLine, error) {
	callCtx, cancel, err := sc.upstreamCallContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	resp, err := sc.routerClient.GetUpstreamBlame(callCtx, &pb.UpstreamBlameRequest{
		RepoUrl: repoURL,
		Branch:  branch,
		Path:    path,
	})
	if err != nil {
		return nil, fmt.Errorf("upstream blame: %w", err)
	}
	out := make([]UpstreamBlameLine, 0, len(resp.GetLines()))
	for _, l := range resp.GetLines() {
		out = append(out, UpstreamBlameLine{
			LineNo:     int(l.GetLineNo()),
			Hash:       l.GetHash(),
			Author:     l.GetAuthorName(),
			AuthoredAt: l.GetAuthoredAtUnix(),
			Content:    l.GetContent(),
		})
	}
	return out, nil
}

func (sc *ShardedClient) upstreamCallContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	sc.mu.RLock()
	routerClient := sc.routerClient
	rpcTimeout := sc.rpcTimeout
	sc.mu.RUnlock()

	if routerClient == nil {
		return nil, nil, fmt.Errorf("no router connection")
	}
	if rpcTimeout <= 0 {
		rpcTimeout = 30e9 // 30s
	}
	callCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	return callCtx, cancel, nil
}

package fetcher

import (
	"context"
	"fmt"

	pb "github.com/radryc/monofs/api/proto"
)

// GetUpstreamLog forwards an upstream log request to a healthy fetcher.
func (c *Client) GetUpstreamLog(ctx context.Context, req *pb.UpstreamLogRequest) (*pb.UpstreamLogResponse, error) {
	fetcher := c.selectFetcher(req.GetRepoUrl())
	if fetcher == nil {
		return nil, fmt.Errorf("no healthy fetchers available")
	}
	callCtx, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer cancel()
	resp, err := fetcher.sync.GetUpstreamLog(callCtx, req)
	if err != nil {
		fetcher.recordError()
		return nil, err
	}
	fetcher.healthy.Store(true)
	return resp, nil
}

// GetUpstreamTags forwards an upstream tags request to a healthy fetcher.
func (c *Client) GetUpstreamTags(ctx context.Context, req *pb.UpstreamTagsRequest) (*pb.UpstreamTagsResponse, error) {
	fetcher := c.selectFetcher(req.GetRepoUrl())
	if fetcher == nil {
		return nil, fmt.Errorf("no healthy fetchers available")
	}
	callCtx, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer cancel()
	resp, err := fetcher.sync.GetUpstreamTags(callCtx, req)
	if err != nil {
		fetcher.recordError()
		return nil, err
	}
	fetcher.healthy.Store(true)
	return resp, nil
}

// GetUpstreamBlame forwards an upstream blame request to a healthy fetcher.
func (c *Client) GetUpstreamBlame(ctx context.Context, req *pb.UpstreamBlameRequest) (*pb.UpstreamBlameResponse, error) {
	fetcher := c.selectFetcher(req.GetRepoUrl())
	if fetcher == nil {
		return nil, fmt.Errorf("no healthy fetchers available")
	}
	callCtx, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer cancel()
	resp, err := fetcher.sync.GetUpstreamBlame(callCtx, req)
	if err != nil {
		fetcher.recordError()
		return nil, err
	}
	fetcher.healthy.Store(true)
	return resp, nil
}

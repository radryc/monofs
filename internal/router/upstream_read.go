package router

import (
	"context"
	"errors"

	pb "github.com/radryc/monofs/api/proto"
)

var errFetcherNotConfigured = errors.New("fetcher cluster not configured")

// GetUpstreamLog forwards an upstream log request to the fetcher cluster.
func (r *Router) GetUpstreamLog(ctx context.Context, req *pb.UpstreamLogRequest) (*pb.UpstreamLogResponse, error) {
	c := r.getFetcherClient()
	if c == nil {
		return nil, errFetcherNotConfigured
	}
	return c.GetUpstreamLog(ctx, req)
}

// GetUpstreamTags forwards an upstream tags request to the fetcher cluster.
func (r *Router) GetUpstreamTags(ctx context.Context, req *pb.UpstreamTagsRequest) (*pb.UpstreamTagsResponse, error) {
	c := r.getFetcherClient()
	if c == nil {
		return nil, errFetcherNotConfigured
	}
	return c.GetUpstreamTags(ctx, req)
}

// GetUpstreamBlame forwards an upstream blame request to the fetcher cluster.
func (r *Router) GetUpstreamBlame(ctx context.Context, req *pb.UpstreamBlameRequest) (*pb.UpstreamBlameResponse, error) {
	c := r.getFetcherClient()
	if c == nil {
		return nil, errFetcherNotConfigured
	}
	return c.GetUpstreamBlame(ctx, req)
}

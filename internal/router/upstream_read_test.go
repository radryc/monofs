package router

import (
	"context"
	"errors"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
)

func TestUpstreamReadsReportFetcherNotConfigured(t *testing.T) {
	r := &Router{}
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
	}{
		{"log", func() error { _, err := r.GetUpstreamLog(ctx, &pb.UpstreamLogRequest{}); return err }},
		{"tags", func() error { _, err := r.GetUpstreamTags(ctx, &pb.UpstreamTagsRequest{}); return err }},
		{"blame", func() error { _, err := r.GetUpstreamBlame(ctx, &pb.UpstreamBlameRequest{}); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, errFetcherNotConfigured) {
				t.Fatalf("error = %v, want errFetcherNotConfigured", err)
			}
		})
	}
}

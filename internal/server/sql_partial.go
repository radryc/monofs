package server

import (
	"context"
	"errors"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage/logengine"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// QueryPartial executes one generated pushdown on this node. It is an
// internal RPC used by the router's federated SQL coordinator; the caller's
// partition scope is applied inside the dataset view exactly like the public
// SQL console.
func (s *Server) QueryPartial(ctx context.Context, req *pb.QueryPartialRequest) (*pb.QueryPartialResponse, error) {
	if err := s.requireInternalSQL(ctx); err != nil {
		return nil, err
	}
	backend, err := s.doctorBackendOrErr()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	from := time.Unix(0, req.GetFromUnixNano()).UTC()
	to := time.Unix(0, req.GetToUnixNano()).UTC()
	if req.GetFromUnixNano() == 0 {
		from = time.Time{}
	}
	if req.GetToUnixNano() == 0 {
		to = time.Time{}
	}
	scope := logengine.QueryScope{}
	if req.GetVisiblePartitionsSet() {
		scope = requestQueryScope(req.GetVisiblePartitions(), true)
	}
	result, err := backend.QueryPartial(ctx, req.GetDataset(), req.GetSql(), from, to, int(req.GetLimit()), scope)
	if err != nil {
		if errors.Is(err, logengine.ErrDuckDBUnavailable) {
			return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "query partial failed: %v", err)
	}
	if result.Truncated {
		return nil, status.Errorf(codes.ResourceExhausted,
			"partial result for dataset %s exceeded the shard budget", req.GetDataset())
	}
	data, err := marshalJSON(result)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal partial result: %v", err)
	}
	return &pb.QueryPartialResponse{ResultsJson: string(data)}, nil
}

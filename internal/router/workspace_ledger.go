package router

import (
	"context"

	pb "github.com/radryc/monofs/api/proto"
)

func (r *Router) QueryLedger(ctx context.Context, req *pb.QueryLedgerRequest) (*pb.QueryLedgerResponse, error) {
	return r.ledger.Query(req), nil
}

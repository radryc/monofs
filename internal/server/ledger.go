package server

import (
	"context"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

// AppendLedgerEntries appends commit/outcome/refresh records to the node-owned ledger.
func (s *Server) AppendLedgerEntries(_ context.Context, req *pb.AppendLedgerEntriesRequest) (*pb.AppendLedgerEntriesResponse, error) {
	if req == nil {
		return &pb.AppendLedgerEntriesResponse{Success: true}, nil
	}

	accepted := int32(0)
	now := time.Now().Unix()

	for _, c := range req.GetCommits() {
		if c == nil {
			continue
		}
		if c.GetTimestampUnix() <= 0 {
			c.TimestampUnix = now
		}
		s.ledger.InsertCommit(c)
		accepted++
	}

	for _, o := range req.GetPushOutcomes() {
		if o == nil {
			continue
		}
		if o.GetTimestampUnix() <= 0 {
			o.TimestampUnix = now
		}
		s.ledger.InsertPushOutcome(o)
		accepted++
	}

	for _, r := range req.GetRefreshEvents() {
		if r == nil {
			continue
		}
		if r.GetTimestampUnix() <= 0 {
			r.TimestampUnix = now
		}
		s.ledger.InsertRefreshEvent(r)
		accepted++
	}

	return &pb.AppendLedgerEntriesResponse{
		Success:  true,
		Accepted: accepted,
	}, nil
}

// QueryLedger returns filtered ledger records from the node-owned ledger.
func (s *Server) QueryLedger(_ context.Context, req *pb.QueryLedgerRequest) (*pb.QueryLedgerResponse, error) {
	if req == nil {
		req = &pb.QueryLedgerRequest{}
	}
	return s.ledger.Query(req), nil
}

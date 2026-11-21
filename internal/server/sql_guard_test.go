package server

import (
	"context"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func sqlGuardRequests() (*pb.QueryDuckDBSQLRequest, *pb.QueryPartialRequest) {
	to := time.Now().UTC()
	from := to.Add(-time.Hour)
	return &pb.QueryDuckDBSQLRequest{
			Dataset:      "logs",
			Query:        "SELECT count(*) AS n FROM logs",
			FromUnixNano: from.UnixNano(),
			ToUnixNano:   to.UnixNano(),
			Limit:        10,
		}, &pb.QueryPartialRequest{
			Dataset:      "logs",
			Sql:          "SELECT count(*) AS n FROM logs",
			FromUnixNano: from.UnixNano(),
			ToUnixNano:   to.UnixNano(),
			Limit:        10,
		}
}

func TestSQLRPCsAreInternalOnly(t *testing.T) {
	srv := &Server{logEngine: &doctorStreamBackendStub{}}
	srv.SetInternalToken("cluster-secret")
	duckReq, partialReq := sqlGuardRequests()

	if _, err := srv.QueryDuckDBSQL(context.Background(), duckReq); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("QueryDuckDBSQL without token = %v, want PermissionDenied", err)
	}
	if _, err := srv.QueryPartial(context.Background(), partialReq); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("QueryPartial without token = %v, want PermissionDenied", err)
	}

	internalCtx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-monofs-internal", "cluster-secret"))
	if _, err := srv.QueryDuckDBSQL(internalCtx, duckReq); err != nil {
		t.Fatalf("QueryDuckDBSQL with internal token = %v", err)
	}
	if _, err := srv.QueryPartial(internalCtx, partialReq); err != nil {
		t.Fatalf("QueryPartial with internal token = %v", err)
	}

	bearerCtx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer cluster-secret"))
	if _, err := srv.QueryDuckDBSQL(bearerCtx, duckReq); err != nil {
		t.Fatalf("QueryDuckDBSQL with internal bearer = %v", err)
	}
}

func TestSQLRPCsAreDisabledWithoutToken(t *testing.T) {
	srv := &Server{logEngine: &doctorStreamBackendStub{}}
	duckReq, partialReq := sqlGuardRequests()
	if _, err := srv.QueryDuckDBSQL(context.Background(), duckReq); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("QueryDuckDBSQL = %v, want FailedPrecondition", err)
	}
	if _, err := srv.QueryPartial(context.Background(), partialReq); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("QueryPartial = %v, want FailedPrecondition", err)
	}
}

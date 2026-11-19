package sqlfederation

import (
	"context"
	"encoding/json"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/sqlagregator/pkg/sqlag"
	"google.golang.org/grpc"
)

type fakePartialClient struct {
	pb.MonoFSClient
	rows [][]any
}

func (f fakePartialClient) QueryPartial(context.Context, *pb.QueryPartialRequest, ...grpc.CallOption) (*pb.QueryPartialResponse, error) {
	payload, err := json.Marshal(map[string]any{
		"columns": []string{"p0"},
		"types":   []string{"int64"},
		"rows":    f.rows,
	})
	if err != nil {
		return nil, err
	}
	return &pb.QueryPartialResponse{ResultsJson: string(payload)}, nil
}

func TestLocalResolverRejectsOtherBackends(t *testing.T) {
	resolver := LocalResolver{Nodes: func() []Node { return nil }}
	if _, err := resolver.Resolve(context.Background(), RoutedEndpoint{
		Endpoint: &sqlag.Endpoint{ID: "x", Backend: "omni"},
	}); err == nil {
		t.Fatal("non-local backend accepted")
	}
}

func TestLocalFanoutConcatenatesNodes(t *testing.T) {
	nodes := []Node{
		{ID: "node-a", Client: fakePartialClient{rows: [][]any{{float64(2)}, {float64(3)}}}},
		{ID: "node-b", Client: fakePartialClient{rows: [][]any{{float64(5)}}}},
	}
	resolver := LocalResolver{Nodes: func() []Node { return nodes }}
	backend, err := resolver.Resolve(context.Background(), RoutedEndpoint{
		Endpoint: &sqlag.Endpoint{ID: "diva/logs/k8s/monofs", Backend: "monofs_local"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	stream, err := backend.Query(context.Background(), sqlag.PartialRequest{
		Dataset: "logs",
		SQL:     "SELECT count(*) AS p0 FROM logs",
		Schema:  []string{"p0"},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(stream.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(stream.Rows))
	}
	if stream.Rows[2][0].I != 5 {
		t.Fatalf("last row = %#v", stream.Rows[2])
	}
	if stream.EndpointID != "diva/logs/k8s/monofs" || len(stream.Schema.Columns) != 1 {
		t.Fatalf("stream = %#v", stream)
	}
}

func TestLocalFanoutFailsWithoutNodes(t *testing.T) {
	resolver := LocalResolver{Nodes: func() []Node { return nil }}
	backend, err := resolver.Resolve(context.Background(), RoutedEndpoint{
		Endpoint: &sqlag.Endpoint{ID: "diva/logs/k8s/monofs", Backend: "monofs_local"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := backend.Query(context.Background(), sqlag.PartialRequest{Dataset: "logs"}); err == nil {
		t.Fatal("expected no-node error")
	}
}

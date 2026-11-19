package sqlfederation

import (
	"context"
	"fmt"

	"github.com/radryc/sqlagregator/pkg/sqlag"
)

// NodeLister returns the healthy MonoFS nodes at query time.
type NodeLister func() []Node

// LocalResolver resolves a `monofs_local` placement to every healthy MonoFS
// node. MonoFS shards its files across nodes with HRW, so one logical shard
// spans all nodes and their partial results are disjoint.
type LocalResolver struct {
	Nodes NodeLister
}

// Resolve implements BackendResolver.
func (r LocalResolver) Resolve(_ context.Context, routed RoutedEndpoint) (sqlag.Backend, error) {
	if routed.Endpoint == nil || routed.Endpoint.Backend != "monofs_local" {
		return nil, fmt.Errorf("local resolver: backend %q is not monofs_local", backendName(routed))
	}
	if r.Nodes == nil {
		return nil, fmt.Errorf("local resolver: no node lister configured")
	}
	return &LocalFanoutBackend{id: routed.Endpoint.ID, nodes: r.Nodes}, nil
}

// LocalFanoutBackend runs one pushdown on every healthy node and concatenates
// the partial rows. Merging stays with the coordinator.
type LocalFanoutBackend struct {
	id    string
	nodes NodeLister
}

// ID returns the placement endpoint ID.
func (b *LocalFanoutBackend) ID() string { return b.id }

// Query implements sqlag.Backend.
func (b *LocalFanoutBackend) Query(ctx context.Context, req sqlag.PartialRequest) (*sqlag.Stream, error) {
	nodes := b.nodes()
	if len(nodes) == 0 {
		return nil, fmt.Errorf("local fanout %s: no healthy nodes", b.id)
	}
	var (
		schema sqlag.Schema
		rows   []sqlag.Row
	)
	for _, node := range nodes {
		backend := &NodeBackend{Node: node}
		stream, err := backend.Query(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("local fanout %s: node %s: %w", b.id, node.ID, err)
		}
		if len(schema.Columns) == 0 {
			schema = stream.Schema
		}
		rows = append(rows, stream.Rows...)
	}
	return &sqlag.Stream{EndpointID: b.id, Schema: schema, Rows: rows}, nil
}

var _ BackendResolver = LocalResolver{}
var _ sqlag.Backend = (*LocalFanoutBackend)(nil)

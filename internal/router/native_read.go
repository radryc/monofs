package router

import (
	"context"
	"fmt"
	"io"

	pb "github.com/radryc/monofs/api/proto"
)

// NativeRead returns file bytes using the same HRW/failover routing policy that
// existing userspace clients use today.
func (r *Router) NativeRead(ctx context.Context, path string, offset, size int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rankedTargets, _ := r.nativeRankedTargets(path, 3)
	if len(rankedTargets) == 0 {
		return nil, fmt.Errorf("no healthy nodes available")
	}

	var lastErr error
	for _, target := range rankedTargets {
		data, err := r.nativeReadFromTarget(ctx, target.client, target.id, path, offset, size)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}

	if lastErr != nil {
		for _, target := range r.nativeRouteTargets(ctx, path) {
			data, err := r.nativeReadFromTarget(ctx, target.client, target.id, path, offset, size)
			if err == nil {
				return data, nil
			}
		}
		return nil, lastErr
	}

	return nil, fmt.Errorf("no healthy nodes available")
}

func (r *Router) nativeReadFromTarget(ctx context.Context, client pb.MonoFSClient, nodeID, path string, offset, size int64) ([]byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, nativeNamespaceRPCTimeout)
	defer cancel()

	stream, err := client.Read(callCtx, &pb.ReadRequest{
		Path:   path,
		Offset: offset,
		Size:   size,
	})
	if err != nil {
		return nil, fmt.Errorf("read RPC to node %s: %w", nodeID, err)
	}

	data := make([]byte, 0)
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read stream from node %s: %w", nodeID, err)
		}
		data = append(data, chunk.GetData()...)
	}

	return data, nil
}

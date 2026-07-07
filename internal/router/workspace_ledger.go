package router

import (
	"context"
	"fmt"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/sharding"
)

func (r *Router) QueryLedger(ctx context.Context, req *pb.QueryLedgerRequest) (*pb.QueryLedgerResponse, error) {
	if req == nil {
		req = &pb.QueryLedgerRequest{}
	}

	workspaceID := strings.TrimSpace(req.GetWorkspaceId())
	if workspaceID == "" {
		return r.queryLedgerFanout(ctx, req)
	}

	client, err := r.ledgerNodeClient(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return client.QueryLedger(ctx, req)
}

func (r *Router) appendPushOutcome(ctx context.Context, outcome *pb.PushOutcome) error {
	if outcome == nil {
		return nil
	}
	workspaceID := strings.TrimSpace(outcome.GetWorkspaceId())
	if workspaceID == "" {
		return fmt.Errorf("workspace_id is required for ledger routing")
	}

	targets, err := r.ledgerNodeClients(ctx, workspaceID)
	if err != nil {
		return err
	}
	appendReq := &pb.AppendLedgerEntriesRequest{PushOutcomes: []*pb.PushOutcome{outcome}}
	for _, client := range targets {
		resp, callErr := client.AppendLedgerEntries(ctx, appendReq)
		if callErr != nil {
			continue
		}
		if !resp.GetSuccess() {
			continue
		}
		return nil
	}

	return fmt.Errorf("ledger append failed: all candidate nodes rejected append")
}

func (r *Router) queryLedgerFanout(ctx context.Context, req *pb.QueryLedgerRequest) (*pb.QueryLedgerResponse, error) {
	clients := r.allHealthyNodeClients()
	if len(clients) == 0 {
		return nil, fmt.Errorf("no healthy nodes available")
	}

	merged := &pb.QueryLedgerResponse{}
	for _, client := range clients {
		resp, err := client.QueryLedger(ctx, req)
		if err != nil {
			continue
		}
		merged.Commits = append(merged.Commits, resp.GetCommits()...)
		merged.PushOutcomes = append(merged.PushOutcomes, resp.GetPushOutcomes()...)
		merged.RefreshEvents = append(merged.RefreshEvents, resp.GetRefreshEvents()...)
		merged.TotalMatches += resp.GetTotalMatches()
	}

	if len(merged.Commits) == 0 && len(merged.PushOutcomes) == 0 && len(merged.RefreshEvents) == 0 {
		return nil, fmt.Errorf("no ledger responses available")
	}
	return merged, nil
}

func (r *Router) ledgerNodeClient(ctx context.Context, workspaceID string) (pb.MonoFSClient, error) {
	clients, err := r.ledgerNodeClients(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return clients[0], nil
}

func (r *Router) ledgerNodeClients(ctx context.Context, workspaceID string) ([]pb.MonoFSClient, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, fmt.Errorf("workspace_id is required for ledger routing")
	}

	clientID := strings.TrimSpace(extractClientID(ctx))
	if clientID == "" {
		clientID = "anonymous"
	}
	key := workspaceID + "|" + clientID

	r.mu.RLock()
	nodes := make([]sharding.Node, 0, len(r.nodes))
	clientsByNode := make(map[string]pb.MonoFSClient, len(r.nodes))
	for _, state := range r.nodes {
		if state == nil || state.info == nil || !state.info.Healthy || state.status != NodeActive || state.client == nil {
			continue
		}
		nodes = append(nodes, sharding.Node{
			ID:      state.info.NodeId,
			Address: state.info.Address,
			Weight:  state.info.Weight,
			Healthy: true,
		})
		clientsByNode[state.info.NodeId] = state.client
	}
	r.mu.RUnlock()

	if len(nodes) == 0 {
		return nil, fmt.Errorf("no healthy nodes available")
	}

	ranked := sharding.NewHRW(nodes).GetNodes(key, len(nodes))
	targets := make([]pb.MonoFSClient, 0, len(ranked))
	for _, node := range ranked {
		if client := clientsByNode[node.ID]; client != nil {
			targets = append(targets, client)
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no healthy nodes available")
	}

	return targets, nil
}

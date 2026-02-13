// Package router provides UI request handling via channels.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

// handleUIRequests processes UI requests in a separate goroutine to prevent blocking router operations.
func (r *Router) handleUIRequests() {
	r.logger.Info("UI request handler started")

	for {
		select {
		case req := <-r.uiRequests:
			r.processUIRequest(req)
		case <-r.stopUI:
			r.logger.Info("UI request handler stopped")
			return
		}
	}
}

// processUIRequest handles a single UI request.
func (r *Router) processUIRequest(req UIRequest) {
	switch req.Type {
	case UIRequestRepositories:
		data := r.buildRepositoriesData()
		req.Response <- UIResponse{Data: data, Error: nil}

	case UIRequestStatus:
		data := r.buildStatusData()
		req.Response <- UIResponse{Data: data, Error: nil}

	case UIRequestRouters:
		data := r.buildRoutersData()
		req.Response <- UIResponse{Data: data, Error: nil}
	}
}

// buildRepositoriesData creates repository list snapshot (called from UI goroutine).
func (r *Router) buildRepositoriesData() *RepositoriesData {
	// Query actual nodes for repository list (source of truth)
	// This ensures router1 and router2 show consistent data
	r.mu.RLock()
	inProgressSnapshot := make(map[string]*inProgressIngestion, len(r.inProgressIngestions))
	for k, v := range r.inProgressIngestions {
		inProgressSnapshot[k] = v
	}
	nodesSnapshot := make(map[string]*nodeState, len(r.nodes))
	for k, v := range r.nodes {
		nodesSnapshot[k] = v
	}
	// Snapshot ingested repos for file counts
	ingestedSnapshot := make(map[string]*ingestedRepo, len(r.ingestedRepos))
	for k, v := range r.ingestedRepos {
		ingestedSnapshot[k] = v
	}
	currentVersion := r.version.Load()
	staleThreshold := r.config.UnhealthyThreshold
	r.mu.RUnlock()

	// Query all nodes for their repositories
	repoMap := make(map[string]map[string]interface{}) // storageID -> repo info
	var repoMu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for _, state := range nodesSnapshot {
		state := state
		if state.client == nil || !state.info.Healthy {
			continue
		}
		if staleThreshold > 0 && time.Since(state.lastSeen) > staleThreshold {
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			resp, err := state.client.ListRepositories(ctx, &pb.ListRepositoriesRequest{})
			cancel()

			if err != nil {
				r.logger.Warn("failed to list repositories from node", "node_id", state.info.NodeId, "error", err)
				return
			}

			// Get full repository info for each
			for _, storageID := range resp.RepositoryIds {
				repoMu.Lock()
				_, exists := repoMap[storageID]
				if !exists {
					repoMap[storageID] = map[string]interface{}{}
				}
				repoMu.Unlock()
				if exists {
					continue
				}

				infoCtx, infoCancel := context.WithTimeout(context.Background(), 1*time.Second)
				repoInfo, err := state.client.GetRepositoryInfo(infoCtx, &pb.GetRepositoryInfoRequest{
					StorageId: storageID,
				})
				infoCancel()

				if err != nil {
					r.logger.Warn("failed to get repository info", "storage_id", storageID, "error", err)
					continue
				}

				// Debug: log commit data from server response
				if repoInfo.CommitHash != "" {
					r.logger.Info("received commit info from server",
						"storage_id", storageID,
						"commit_hash", repoInfo.CommitHash,
						"commit_time", repoInfo.CommitTime)
				}

				// Get file count and ingested time from router's tracking
				var filesCount int64
				var ingestedAt time.Time
				var rebalanceState string = "Stable"
				var rebalanceProgress float64 = 1.0
				if tracked, ok := ingestedSnapshot[storageID]; ok {
					tracked.mu.RLock()
					filesCount = tracked.filesCount
					ingestedAt = tracked.ingestedAt
					rebalanceState = tracked.rebalanceState.String()
					rebalanceProgress = tracked.rebalanceProgress
					tracked.mu.RUnlock()
				}
				if ingestedAt.IsZero() {
					ingestedAt = time.Now()
				}

				repoMu.Lock()
				repoMap[storageID] = map[string]interface{}{
					"storage_id":         storageID,
					"repo_id":            repoInfo.DisplayPath,
					"repo_url":           repoInfo.Source,
					"branch":             repoInfo.Ref,
					"commit_hash":        repoInfo.CommitHash,
					"commit_time":        repoInfo.CommitTime,
					"commit_message":     repoInfo.CommitMessage,
					"files_count":        filesCount,
					"ingested_at":        ingestedAt.Unix(),
					"topology_version":   currentVersion,
					"rebalance_state":    rebalanceState,
					"rebalance_progress": rebalanceProgress,
				}
				repoMu.Unlock()
			}
		}()
	}

	wg.Wait()

	repos := make([]map[string]interface{}, 0, len(repoMap)+len(inProgressSnapshot))

	// Add in-progress ingestions first
	for storageID, progress := range inProgressSnapshot {
		progress.mu.RLock()
		repoInfo := map[string]interface{}{
			"storage_id":         storageID,
			"repo_id":            progress.repoID,
			"repo_url":           progress.repoURL,
			"branch":             progress.branch,
			"files_count":        progress.filesProcessed,
			"total_files":        progress.totalFiles,
			"ingested_at":        progress.startedAt.Unix(),
			"topology_version":   currentVersion,
			"rebalance_state":    "Ingesting",
			"rebalance_progress": float64(progress.filesProcessed) / float64(max(progress.totalFiles, 1)),
			"stage":              progress.stage.String(),
			"message":            progress.message,
			"in_progress":        true,
		}
		repos = append(repos, repoInfo)
		progress.mu.RUnlock()
	}

	// Collect in-progress source URLs for cross-check dedup.
	// The storageID of an in-progress npm/cargo/maven ingestion may differ
	// from the storageID reported by nodes (initial vs canonical), so we
	// also match by source URL to prevent duplicates in the UI.
	inProgressURLs := make(map[string]bool, len(inProgressSnapshot))
	for _, progress := range inProgressSnapshot {
		progress.mu.RLock()
		if progress.repoURL != "" {
			inProgressURLs[progress.repoURL] = true
		}
		progress.mu.RUnlock()
	}

	// Add completed ingestions from nodes (source of truth)
	// Skip any that are already shown as in-progress to avoid duplicates
	for storageID, repoInfo := range repoMap {
		if _, isInProgress := inProgressSnapshot[storageID]; isInProgress {
			continue
		}
		// Also check by source URL for npm/cargo/maven where storageID changes mid-ingestion
		if repoURL, ok := repoInfo["repo_url"].(string); ok && repoURL != "" {
			if inProgressURLs[repoURL] {
				continue
			}
		}
		repos = append(repos, repoInfo)
	}

	return &RepositoriesData{
		Repositories:           repos,
		CurrentTopologyVersion: currentVersion,
	}
}

// buildStatusData creates cluster status snapshot (called from UI goroutine).
func (r *Router) buildStatusData() *StatusData {
	// Snapshot nodes with deep copy of fields to avoid races
	r.mu.RLock()
	type nodeSnapshot struct {
		nodeInfo        *pb.NodeInfo
		externalAddress string
		status          NodeStatus
		syncProgress    float64
		ownedFilesCount int64
		diskUsedBytes   int64
		diskTotalBytes  int64
		diskFreeBytes   int64
		backingUpNodes  []string
	}
	nodesSnapshot := make(map[string]nodeSnapshot, len(r.nodes))
	for nodeID, state := range r.nodes {
		nodesSnapshot[nodeID] = nodeSnapshot{
			nodeInfo:        state.info,
			externalAddress: state.externalAddress,
			status:          state.status,
			syncProgress:    state.syncProgress,
			ownedFilesCount: state.ownedFilesCount,
			diskUsedBytes:   state.diskUsedBytes,
			diskTotalBytes:  state.diskTotalBytes,
			diskFreeBytes:   state.diskFreeBytes,
			backingUpNodes:  append([]string(nil), state.backingUpNodes...), // Deep copy slice
		}
	}
	r.mu.RUnlock()

	// Build response without holding lock
	nodes := make([]map[string]interface{}, 0, len(nodesSnapshot))
	for _, snap := range nodesSnapshot {
		nodeInfo := map[string]interface{}{
			"id":         snap.nodeInfo.NodeId,
			"address":    snap.nodeInfo.Address,
			"healthy":    snap.nodeInfo.Healthy,
			"weight":     snap.nodeInfo.Weight,
			"status":     snap.status.String(),
			"file_count": snap.ownedFilesCount,
			"disk_used":  snap.diskUsedBytes,
			"disk_total": snap.diskTotalBytes,
			"disk_free":  snap.diskFreeBytes,
		}

		// Add backup info
		if len(snap.backingUpNodes) > 0 {
			nodeInfo["backing_up"] = snap.backingUpNodes
		}

		// Add "covered_by" for failed nodes
		if !snap.nodeInfo.Healthy {
			if backupNodeID, hasFailover := r.failoverMap.Load(snap.nodeInfo.NodeId); hasFailover {
				nodeInfo["covered_by"] = backupNodeID.(string)
			}
		}

		// Add sync progress for new nodes
		if snap.status == NodeSyncing {
			nodeInfo["sync_progress"] = snap.syncProgress
		}

		nodes = append(nodes, nodeInfo)
	}

	// Sort nodes by ID for consistent display
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i]["id"].(string) < nodes[j]["id"].(string)
	})

	// Add failover mappings
	failovers := make(map[string]string)
	r.failoverMap.Range(func(key, value interface{}) bool {
		failovers[key.(string)] = value.(string)
		return true
	})

	// Add drain status
	drainStatus := make(map[string]interface{})
	if r.IsDrained() {
		r.drainMu.RLock()
		drainStatus["active"] = true
		drainStatus["reason"] = r.drainReason
		drainStatus["drained_at"] = r.drainedAt.Unix()
		drainStatus["duration"] = time.Since(r.drainedAt).Seconds()
		r.drainMu.RUnlock()
	} else {
		drainStatus["active"] = false
	}

	return &StatusData{
		Nodes:     nodes,
		Failovers: failovers,
		DrainMode: drainStatus,
		Version: map[string]string{
			"version":    r.buildVersion,
			"commit":     r.buildCommit,
			"build_time": r.buildTime,
		},
	}
}

// buildRoutersData aggregates local and peer router data for UI.
func (r *Router) buildRoutersData() *RoutersData {
	peers := r.config.PeerRouters
	snapshots := make([]RouterSnapshot, 0, len(peers)+1)

	// Always include local router snapshot
	localStatus := r.buildStatusData()
	localRepos := r.buildRepositoriesData()
	routerName := r.config.RouterName
	if routerName == "" {
		routerName = "local"
	}
	snapshots = append(snapshots, RouterSnapshot{
		Name:         routerName,
		URL:          "",
		Local:        true,
		Status:       localStatus,
		Repositories: localRepos,
	})

	if len(peers) == 0 {
		return &RoutersData{
			Routers:     snapshots,
			GeneratedAt: time.Now().Unix(),
		}
	}

	client := &http.Client{Timeout: 1500 * time.Millisecond}
	var wg sync.WaitGroup
	mu := sync.Mutex{}
	sem := make(chan struct{}, 4)

	for _, peer := range peers {
		peer := peer
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			normalizedURL, err := normalizeRouterURL(peer.URL)
			if err != nil {
				mu.Lock()
				snapshots = append(snapshots, RouterSnapshot{
					Name:  peer.Name,
					URL:   peer.URL,
					Local: false,
					Error: "invalid router url",
				})
				mu.Unlock()
				return
			}

			status, statusErr := fetchRouterStatus(client, normalizedURL)
			repos, reposErr := fetchRouterRepositories(client, normalizedURL)

			errMsg := ""
			if statusErr != nil && reposErr != nil {
				errMsg = "unreachable"
			} else if statusErr != nil {
				errMsg = "status unavailable"
			} else if reposErr != nil {
				errMsg = "repositories unavailable"
			}

			mu.Lock()
			snapshots = append(snapshots, RouterSnapshot{
				Name:         peer.Name,
				URL:          normalizedURL,
				Local:        false,
				Status:       status,
				Repositories: repos,
				Error:        errMsg,
			})
			mu.Unlock()
		}()
	}

	wg.Wait()
	return &RoutersData{
		Routers:     snapshots,
		GeneratedAt: time.Now().Unix(),
	}
}

func normalizeRouterURL(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("empty")
	}
	trimmed := strings.TrimSpace(raw)
	if !strings.Contains(trimmed, "://") {
		trimmed = "http://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid url")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func fetchRouterStatus(client *http.Client, baseURL string) (*StatusData, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var data StatusData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return &data, nil
}

func fetchRouterRepositories(client *http.Client, baseURL string) (*RepositoriesData, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/repositories", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var data RepositoriesData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return &data, nil
}

// sendUIRequest sends a request to the UI handler and waits for response with timeout.
func (r *Router) sendUIRequest(reqType UIRequestType, timeout time.Duration) (interface{}, error) {
	responseChan := make(chan UIResponse, 1)
	req := UIRequest{
		Type:     reqType,
		Response: responseChan,
	}

	select {
	case r.uiRequests <- req:
		// Request sent successfully
	case <-time.After(timeout):
		return nil, ErrUITimeout
	}

	select {
	case resp := <-responseChan:
		return resp.Data, resp.Error
	case <-time.After(timeout):
		return nil, ErrUITimeout
	}
}

var ErrUITimeout = fmt.Errorf("UI request timeout")

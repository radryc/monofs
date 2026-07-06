// Package router provides UI request handling via channels.
package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	case UIRequestDependencies:
		data := r.buildDependenciesData()
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
	uiBases := r.repositoryUIBases()

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

				// Get file count and ingested time from router's tracking
				var filesCount int64
				var ingestedAt time.Time
				var rebalanceState string = "Stable"
				var rebalanceProgress float64 = 1.0
				guardianURL := repoInfo.GuardianUrl
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
				productLink := buildRepositoryProductLink(
					repoInfo.DisplayPath,
					repositoryProductStoredURL(repoInfo.DisplayPath, guardianURL, repoInfo.Source),
					uiBases,
				)

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
					"guardian_url":       guardianURL,
					"product_kind":       productLink.Kind,
					"product_ui_url":     productLink.URL,
					"product_ui_label":   productLink.Label,
					"is_guardian":        productLink.Kind == "guardian",
					"is_doctor":          productLink.Kind == "doctor",
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
		productLink := buildRepositoryProductLink(
			progress.repoID,
			repositoryProductStoredURL(progress.repoID, "", progress.repoURL),
			uiBases,
		)
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
			"product_kind":       productLink.Kind,
			"product_ui_url":     productLink.URL,
			"product_ui_label":   productLink.Label,
			"is_guardian":        productLink.Kind == "guardian",
			"is_doctor":          productLink.Kind == "doctor",
		}
		repos = append(repos, repoInfo)
		progress.mu.RUnlock()
	}

	// Add completed ingestions from nodes (source of truth)
	for _, repoInfo := range repoMap {
		repos = append(repos, repoInfo)
	}

	return &RepositoriesData{
		Repositories:           repos,
		CurrentTopologyVersion: currentVersion,
	}
}

// buildStatusData creates cluster status snapshot (called from UI goroutine).
func (r *Router) buildStatusData() *StatusData {
	// Snapshot nodes quickly, then release lock
	r.mu.RLock()
	nodesSnapshot := make(map[string]*nodeState, len(r.nodes))
	for k, v := range r.nodes {
		nodesSnapshot[k] = v
	}
	r.mu.RUnlock()

	// Build response without holding lock
	nodes := make([]map[string]interface{}, 0, len(nodesSnapshot))
	var (
		nodesTotal      float64
		nodesHealthy    float64
		nodesUnhealthy  float64
		kvsEnabledNodes float64
		kvsHealthyNodes float64
		filesTotal      float64
		diskTotalBytes  float64
		diskUsedBytes   float64
		diskFreeBytes   float64
	)
	for _, state := range nodesSnapshot {
		kvsStatus := normalizedKVSNodeStatus(state.kvsStatus)
		nodesTotal++
		if state.info.Healthy {
			nodesHealthy++
		} else {
			nodesUnhealthy++
		}
		if kvsStatus.GetEnabled() {
			kvsEnabledNodes++
		}
		if kvsStatus.GetHealthy() {
			kvsHealthyNodes++
		}
		filesTotal += float64(state.ownedFilesCount)
		diskTotalBytes += float64(state.diskTotalBytes)
		diskUsedBytes += float64(state.diskUsedBytes)
		diskFreeBytes += float64(state.diskFreeBytes)

		nodeInfo := map[string]interface{}{
			"id":         state.info.NodeId,
			"address":    state.info.Address,
			"healthy":    state.info.Healthy,
			"weight":     state.info.Weight,
			"status":     state.status.String(),
			"file_count": state.ownedFilesCount,
			"disk_used":  state.diskUsedBytes,
			"disk_total": state.diskTotalBytes,
			"disk_free":  state.diskFreeBytes,
			"kvs": map[string]interface{}{
				"enabled":    kvsStatus.GetEnabled(),
				"healthy":    kvsStatus.GetHealthy(),
				"mode":       kvsStatus.GetMode(),
				"role":       kvsStatus.GetRole(),
				"leader_id":  kvsStatus.GetLeaderId(),
				"peer_count": kvsStatus.GetPeerCount(),
				"key_count":  kvsStatus.GetKeyCount(),
			},
		}

		// Add backup info
		if len(state.backingUpNodes) > 0 {
			nodeInfo["backing_up"] = state.backingUpNodes
		}

		// Add "covered_by" for failed nodes
		if !state.info.Healthy {
			if backupNodeID, hasFailover := r.failoverMap.Load(state.info.NodeId); hasFailover {
				nodeInfo["covered_by"] = backupNodeID.(string)
			}
		}

		// Add sync progress for new nodes
		if state.status == NodeSyncing {
			nodeInfo["sync_progress"] = state.syncProgress
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
	failoversActive := float64(len(failovers))

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

	statusMetrics := map[string]float64{
		"nodes_total":           nodesTotal,
		"nodes_healthy":         nodesHealthy,
		"nodes_unhealthy":       nodesUnhealthy,
		"kvs_enabled_nodes":     kvsEnabledNodes,
		"kvs_healthy_nodes":     kvsHealthyNodes,
		"files_total":           filesTotal,
		"failovers_active":      failoversActive,
		"disk_total_bytes":      diskTotalBytes,
		"disk_used_bytes":       diskUsedBytes,
		"disk_free_bytes":       diskFreeBytes,
		"drain_active":          0,
		"policy_gate_enabled":   0,
		"auto_push_enabled":     0,
		"workspace_wal_enabled": 0,
	}

	features := []FeatureInfo{
		{
			ID:          "storage_node_wal_ledger",
			Name:        "Storage-node WAL Ledger",
			Description: "Commit/push ledger persistence is owned by storage nodes and proxied by routers.",
			Enabled:     true,
			Status:      "always-on",
			HelpHint:    "Core data-safety feature. This is always enabled and cannot be toggled from runtime config.",
		},
		{
			ID:          "workspace_job_wal",
			Name:        "Workspace Job WAL Persistence",
			Description: "Workspace sync jobs persist to WAL when workspace-state-dir is configured.",
			Enabled:     strings.TrimSpace(r.config.WorkspaceStateDir) != "",
			Status:      "runtime-config",
			EnableHint:  "--workspace-state-dir=/var/lib/monofs/workspace",
			DisableHint: "--workspace-state-dir=",
			HelpHint:    "Set a persistent path so workspace sync job history survives router restarts.",
		},
		{
			ID:          "hrw_routing",
			Name:        "Rendezvous (HRW) Routing",
			Description: "Node ownership and ledger proxy placement use deterministic HRW hashing.",
			Enabled:     true,
			Status:      "always-on",
			HelpHint:    "Core routing algorithm. This is always enabled and cannot be toggled.",
		},
		{
			ID:          "policy_gate",
			Name:        "Policy-gated Sync",
			Description: "Publish/push/refresh operations are evaluated against workspace policy rules.",
			Enabled:     r.config.PolicyGateEnabled,
			Status:      "runtime-config",
			EnableHint:  "--policy-gate --policy-config=/etc/monofs/workspace-policy.yaml",
			DisableHint: "--policy-gate=false",
			HelpHint:    "Enable this to enforce allow/deny rules for workspace publish, push, and refresh operations.",
		},
		{
			ID:          "auto_push",
			Name:        "Auto Push Worker",
			Description: "Background worker scans pending workspace bundles and pushes when allowed.",
			Enabled:     r.config.AutoPushEnabled,
			Status:      "runtime-config",
			EnableHint:  "--auto-push --auto-push-interval=60s",
			DisableHint: "--auto-push=false",
			HelpHint:    "Runs background push jobs after policy checks. Tune interval based on your CI/CD throughput.",
		},
	}

	if drainStatus["active"] == true {
		statusMetrics["drain_active"] = 1
	}
	if r.config.PolicyGateEnabled {
		statusMetrics["policy_gate_enabled"] = 1
	}
	if r.config.AutoPushEnabled {
		statusMetrics["auto_push_enabled"] = 1
	}
	if strings.TrimSpace(r.config.WorkspaceStateDir) != "" {
		statusMetrics["workspace_wal_enabled"] = 1
	}

	routerClusterNodes.WithLabelValues("total").Set(nodesTotal)
	routerClusterNodes.WithLabelValues("healthy").Set(nodesHealthy)
	routerClusterNodes.WithLabelValues("unhealthy").Set(nodesUnhealthy)
	routerClusterNodes.WithLabelValues("kvs_enabled").Set(kvsEnabledNodes)
	routerClusterNodes.WithLabelValues("kvs_healthy").Set(kvsHealthyNodes)
	routerClusterFilesTotal.Set(filesTotal)
	routerClusterFailoversTotal.Set(failoversActive)
	routerClusterDiskBytes.WithLabelValues("total").Set(diskTotalBytes)
	routerClusterDiskBytes.WithLabelValues("used").Set(diskUsedBytes)
	routerClusterDiskBytes.WithLabelValues("free").Set(diskFreeBytes)

	return &StatusData{
		Nodes:     nodes,
		Failovers: failovers,
		DrainMode: drainStatus,
		Version: map[string]string{
			"version":    r.buildVersion,
			"commit":     r.buildCommit,
			"build_time": r.buildTime,
		},
		Features: features,
		Metrics:  statusMetrics,
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

// buildDependenciesData queries the cluster nodes for files in the
// "dependency" repository and aggregates them into a UI-friendly summary.
func (r *Router) buildDependenciesData() *DependenciesData {
	data := &DependenciesData{}

	// The dependency repo uses a deterministic storageID.
	hash := sha256.Sum256([]byte("dependency"))
	storageID := hex.EncodeToString(hash[:])

	// Snapshot healthy nodes and ingested repo info.
	r.mu.RLock()
	nodesSnapshot := make(map[string]*nodeState, len(r.nodes))
	for k, v := range r.nodes {
		nodesSnapshot[k] = v
	}
	ingestedSnapshot := make(map[string]*ingestedRepo, len(r.ingestedRepos))
	for k, v := range r.ingestedRepos {
		ingestedSnapshot[k] = v
	}
	staleThreshold := r.config.UnhealthyThreshold
	r.mu.RUnlock()

	// Get ingestedAt from router tracking (if discovered).
	if tracked, ok := ingestedSnapshot[storageID]; ok {
		tracked.mu.RLock()
		data.IngestedAt = tracked.ingestedAt.Unix()
		tracked.mu.RUnlock()
	}

	// Query every healthy node for files in the dependency repo.
	type nodeResult struct {
		nodeID string
		files  []string
	}
	var results []nodeResult
	var resMu sync.Mutex
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

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			files, err := streamRepositoryFiles(ctx, state.client, storageID)
			cancel()

			if err != nil {
				r.logger.Warn("failed to get dependency files from node",
					"node_id", state.info.NodeId, "error", err)
				return
			}

			resMu.Lock()
			results = append(results, nodeResult{
				nodeID: state.info.NodeId,
				files:  files,
			})
			resMu.Unlock()
		}()
	}

	wg.Wait()

	// Aggregate: deduplicate files across nodes and group by tool prefix.
	// Paths look like "go/mod/cache/..." where the first segment is the tool.
	uniqueFiles := make(map[string]bool)
	toolCounts := make(map[string]int)

	for _, nr := range results {
		for _, f := range nr.files {
			if uniqueFiles[f] {
				continue // already counted (replication / dual-active)
			}
			uniqueFiles[f] = true

			parts := strings.SplitN(f, "/", 2)
			tool := "unknown"
			if len(parts) >= 1 && parts[0] != "" {
				tool = parts[0]
			}
			toolCounts[tool]++
		}
	}

	data.TotalFiles = len(uniqueFiles)

	// Build per-tool summaries sorted by file count descending.
	for tool, count := range toolCounts {
		data.Tools = append(data.Tools, DepsToolSummary{
			Tool:  tool,
			Files: count,
		})
	}
	sort.Slice(data.Tools, func(i, j int) bool {
		return data.Tools[i].Files > data.Tools[j].Files
	})
	data.Ecosystems = len(data.Tools)

	// Build per-node distribution.
	for _, nr := range results {
		if len(nr.files) > 0 {
			data.Nodes = append(data.Nodes, DepsNodeInfo{
				NodeID: nr.nodeID,
				Files:  len(nr.files),
			})
		}
	}
	sort.Slice(data.Nodes, func(i, j int) bool {
		return data.Nodes[i].Files > data.Nodes[j].Files
	})
	data.NodesWithData = len(data.Nodes)

	return data
}

package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/pkg/authz"
)

const (
	guardianPartitionsCacheTTL = 5 * time.Second
	guardianPartitionsPageMax  = 5000
)

// guardianPartitionView is the JSON shape returned by
// GET /api/guardian/partitions. It intentionally mirrors the repository map
// fields the endpoint exposed before, but is built once per snapshot rather
// than once per request.
type guardianPartitionView struct {
	StorageID         string         `json:"storage_id"`
	RepoID            string         `json:"repo_id"`
	RepoURL           string         `json:"repo_url,omitempty"`
	Branch            string         `json:"branch,omitempty"`
	CommitHash        string         `json:"commit_hash,omitempty"`
	CommitTime        int64          `json:"commit_time,omitempty"`
	CommitMessage     string         `json:"commit_message,omitempty"`
	FilesCount        int64          `json:"files_count"`
	IngestedAt        int64          `json:"ingested_at"`
	Refresh           map[string]any `json:"refresh,omitempty"`
	TopologyVersion   int64          `json:"topology_version"`
	RebalanceState    string         `json:"rebalance_state"`
	RebalanceProgress float64        `json:"rebalance_progress"`
	GuardianURL       string         `json:"guardian_url,omitempty"`
	ProductKind       string         `json:"product_kind,omitempty"`
	ProductUIURL      string         `json:"product_ui_url,omitempty"`
	ProductUILabel    string         `json:"product_ui_label,omitempty"`
	IsGuardian        bool           `json:"is_guardian"`
	IsDoctor          bool           `json:"is_doctor"`
	TotalFiles        int64          `json:"total_files,omitempty"`
	Stage             string         `json:"stage,omitempty"`
	Message           string         `json:"message,omitempty"`
	InProgress        bool           `json:"in_progress,omitempty"`
}

type guardianPartitionSnapshot struct {
	Items      []guardianPartitionView
	Generation uint64
}

func (r *Router) invalidateGuardianPartitions() {
	r.guardianPartitionsMu.Lock()
	r.guardianPartitionsAt = time.Time{}
	r.guardianPartitionsGen++
	r.guardianPartitionsMu.Unlock()
}

func (r *Router) guardianPartitionSnapshot(ctx context.Context) (*guardianPartitionSnapshot, error) {
	r.guardianPartitionsMu.RLock()
	cached := r.guardianPartitionsCache
	cachedAt := r.guardianPartitionsAt
	r.guardianPartitionsMu.RUnlock()
	if cached != nil && time.Since(cachedAt) < guardianPartitionsCacheTTL {
		return cached, nil
	}

	type refreshResult struct {
		snapshot *guardianPartitionSnapshot
		err      error
	}
	resultCh := r.guardianPartitionsGroup.DoChan("refresh", func() (any, error) {
		r.guardianPartitionsMu.RLock()
		cached := r.guardianPartitionsCache
		cachedAt := r.guardianPartitionsAt
		gen := r.guardianPartitionsGen
		r.guardianPartitionsMu.RUnlock()
		if cached != nil && time.Since(cachedAt) < guardianPartitionsCacheTTL {
			return cached, nil
		}

		// Detach from the first caller's request context: a UI client
		// disconnecting must not cancel the shared refresh for every waiter.
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()

		snapshot, refreshErr := r.refreshGuardianPartitions(refreshCtx)
		if refreshErr != nil {
			if cached != nil {
				// Keep serving the last good snapshot on a transient node failure.
				return cached, nil
			}
			return nil, refreshErr
		}
		r.guardianPartitionsMu.Lock()
		// Do not publish a snapshot that was computed before an invalidation.
		if gen == r.guardianPartitionsGen {
			snapshot.Generation = r.guardianPartitionsSeq.Add(1)
			r.guardianPartitionsCache = snapshot
			r.guardianPartitionsAt = time.Now()
			if r.guardianPartitionsHistory == nil {
				r.guardianPartitionsHistory = make(map[uint64]*guardianPartitionSnapshot)
			}
			r.guardianPartitionsHistory[snapshot.Generation] = snapshot
			r.guardianPartitionsOrder = append(r.guardianPartitionsOrder, snapshot.Generation)
			for len(r.guardianPartitionsOrder) > 2 {
				oldest := r.guardianPartitionsOrder[0]
				r.guardianPartitionsOrder = r.guardianPartitionsOrder[1:]
				delete(r.guardianPartitionsHistory, oldest)
			}
		}
		r.guardianPartitionsMu.Unlock()
		return snapshot, nil
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-resultCh:
		if result.Err != nil {
			return nil, result.Err
		}
		snapshot, _ := result.Val.(*guardianPartitionSnapshot)
		if snapshot == nil {
			return nil, fmt.Errorf("guardian partition refresh returned no snapshot")
		}
		return snapshot, nil
	}
}

func (r *Router) guardianPartitionSnapshotByGeneration(generation uint64) (*guardianPartitionSnapshot, bool) {
	r.guardianPartitionsMu.RLock()
	defer r.guardianPartitionsMu.RUnlock()
	snapshot, ok := r.guardianPartitionsHistory[generation]
	return snapshot, ok
}

func (r *Router) refreshGuardianPartitions(ctx context.Context) (*guardianPartitionSnapshot, error) {
	type nodeTarget struct {
		id     string
		client pb.MonoFSClient
	}

	r.mu.RLock()
	targets := make([]nodeTarget, 0, len(r.nodes))
	for id, state := range r.nodes {
		if state == nil || state.client == nil || state.info == nil || !state.info.Healthy {
			continue
		}
		targets = append(targets, nodeTarget{id: id, client: state.client})
	}
	ingested := make(map[string]*ingestedRepo, len(r.ingestedRepos))
	for storageID, repo := range r.ingestedRepos {
		ingested[storageID] = repo
	}
	inProgress := make(map[string]*inProgressIngestion, len(r.inProgressIngestions))
	for storageID, progress := range r.inProgressIngestions {
		inProgress[storageID] = progress
	}
	currentVersion := r.version.Load()
	r.mu.RUnlock()

	if len(targets) == 0 {
		return nil, fmt.Errorf("no healthy nodes available for guardian partition listing")
	}

	records := make(map[string]*pb.GuardianPartitionRecord)
	var recordsMu sync.Mutex
	var wg sync.WaitGroup
	var successCount atomic.Int32
	var failureCount atomic.Int32

	for _, target := range targets {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			pageToken := ""
			ok := false
			for page := 0; page < 100; page++ {
				callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				resp, err := target.client.ListGuardianPartitions(callCtx, &pb.ListGuardianPartitionsRequest{
					PageSize:      guardianPartitionsPageMax,
					PageToken:     pageToken,
					IncludeSystem: true,
				})
				cancel()
				if err != nil {
					r.logger.Warn("failed to list guardian partitions from node",
						"node_id", target.id,
						"error", err)
					failureCount.Add(1)
					return
				}
				ok = true
				recordsMu.Lock()
				for _, record := range resp.GetPartitions() {
					if record == nil || record.GetStorageId() == "" {
						continue
					}
					if _, exists := records[record.GetStorageId()]; !exists {
						records[record.GetStorageId()] = record
					}
				}
				recordsMu.Unlock()
				pageToken = resp.GetNextPageToken()
				if pageToken == "" {
					break
				}
			}
			if ok {
				successCount.Add(1)
			}
		}()
	}
	wg.Wait()

	if successCount.Load() == 0 {
		return nil, fmt.Errorf("guardian partition listing failed on %d node(s)", failureCount.Load())
	}

	uiBases := r.repositoryUIBases()
	items := make([]guardianPartitionView, 0, len(records)+len(inProgress))
	seen := make(map[string]struct{}, len(records)+len(inProgress))
	seenRepo := make(map[string]struct{}, len(records)+len(inProgress))

	for storageID, record := range records {
		if record == nil {
			continue
		}
		displayPath := record.GetDisplayPath()
		if displayPath != "guardian-system" && !strings.HasPrefix(displayPath, "guardian/") {
			continue
		}
		// The API cursor is the display path, so keep that unique even when a
		// display path has more than one storage ID.
		if _, dup := seenRepo[displayPath]; dup {
			continue
		}
		seenRepo[displayPath] = struct{}{}
		view := guardianPartitionView{
			StorageID:       storageID,
			RepoID:          displayPath,
			RepoURL:         record.GetSource(),
			Branch:          record.GetRef(),
			CommitHash:      record.GetCommitHash(),
			CommitTime:      record.GetCommitTime(),
			CommitMessage:   record.GetCommitMessage(),
			IngestedAt:      time.Now().Unix(),
			TopologyVersion: currentVersion,
			RebalanceState:  "Stable",
			GuardianURL:     record.GetGuardianUrl(),
			Refresh: map[string]any{
				"last_probed_at":    0,
				"last_probe_status": "",
				"last_probe_error":  "",
				"last_change_at":    0,
				"changes_total":     0,
				"reingests_total":   0,
				"next_probe_at":     0,
			},
		}
		view.RebalanceProgress = 1.0
		if tracked, ok := ingested[storageID]; ok {
			tracked.mu.RLock()
			view.FilesCount = tracked.filesCount
			if !tracked.ingestedAt.IsZero() {
				view.IngestedAt = tracked.ingestedAt.Unix()
			}
			view.RebalanceState = tracked.rebalanceState.String()
			view.RebalanceProgress = tracked.rebalanceProgress
			tracked.mu.RUnlock()
			view.Refresh = tracked.refreshJSON()
		}
		productLink := buildRepositoryProductLink(
			displayPath,
			repositoryProductStoredURL(displayPath, view.GuardianURL, view.RepoURL),
			uiBases,
		)
		view.ProductKind = productLink.Kind
		view.ProductUIURL = productLink.URL
		view.ProductUILabel = productLink.Label
		view.IsGuardian = productLink.Kind == "guardian"
		view.IsDoctor = productLink.Kind == "doctor"
		items = append(items, view)
		seen[storageID] = struct{}{}
	}

	// Include in-progress partition injections so the UI can show them before
	// the first node listing reflects the new repository.
	for storageID, progress := range inProgress {
		if progress == nil {
			continue
		}
		progress.mu.RLock()
		repoID := progress.repoID
		repoURL := progress.repoURL
		filesProcessed := progress.filesProcessed
		totalFiles := progress.totalFiles
		stage := progress.stage.String()
		message := progress.message
		startedAt := progress.startedAt
		progress.mu.RUnlock()

		if repoID != "guardian-system" && !strings.HasPrefix(repoID, "guardian/") {
			continue
		}
		if _, exists := seen[storageID]; exists {
			continue
		}
		progressValue := 1.0
		if totalFiles > 0 {
			progressValue = float64(filesProcessed) / float64(totalFiles)
		}
		productLink := buildRepositoryProductLink(repoID, repositoryProductStoredURL(repoID, "", repoURL), uiBases)
		view := guardianPartitionView{
			StorageID:         storageID,
			RepoID:            repoID,
			RepoURL:           repoURL,
			FilesCount:        filesProcessed,
			IngestedAt:        startedAt.Unix(),
			TopologyVersion:   currentVersion,
			RebalanceState:    "Ingesting",
			RebalanceProgress: progressValue,
			ProductKind:       productLink.Kind,
			ProductUIURL:      productLink.URL,
			ProductUILabel:    productLink.Label,
			IsGuardian:        productLink.Kind == "guardian",
			IsDoctor:          productLink.Kind == "doctor",
			TotalFiles:        totalFiles,
			Stage:             stage,
			Message:           message,
			InProgress:        true,
		}
		view.Refresh = map[string]any{"stage": stage, "message": message, "in_progress": true}
		items = append(items, view)
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].RepoID != items[j].RepoID {
			return items[i].RepoID < items[j].RepoID
		}
		return items[i].StorageID < items[j].StorageID
	})

	return &guardianPartitionSnapshot{Items: items}, nil
}

func guardianPartitionPageSize(raw string) int {
	// No parameter (or an invalid value) uses the default page. An explicit
	// page_size=0 is an escape hatch for callers that still need the full list.
	if strings.TrimSpace(raw) == "" {
		return 1000
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 1000
	}
	if value == 0 {
		return 0
	}
	if value > guardianPartitionsPageMax {
		return guardianPartitionsPageMax
	}
	return value
}

func (r *Router) handleGuardianPartitionsPage(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pageSize := guardianPartitionPageSize(req.URL.Query().Get("page_size"))
	rawToken := strings.TrimSpace(req.URL.Query().Get("page_token"))
	generation := uint64(0)
	cursor := rawToken
	if idx := strings.Index(rawToken, ":"); idx > 0 {
		if parsed, parseErr := strconv.ParseUint(rawToken[:idx], 10, 64); parseErr == nil {
			generation = parsed
			cursor = rawToken[idx+1:]
		}
	}

	var snapshot *guardianPartitionSnapshot
	if generation > 0 {
		pinned, ok := r.guardianPartitionSnapshotByGeneration(generation)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success":          false,
				"snapshot_expired": true,
				"message":          "pagination snapshot expired; restart from the first page",
			})
			return
		}
		snapshot = pinned
	} else {
		var err error
		snapshot, err = r.guardianPartitionSnapshot(req.Context())
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
	}

	id, _ := authz.IdentityFromContext(req.Context())
	visible := r.guardianPartitionVisibility(id)

	total := -1
	if cursor == "" {
		total = 0
		for _, item := range snapshot.Items {
			if visible(item.RepoID) {
				total++
			}
		}
	}

	start := 0
	if cursor != "" {
		start = sort.Search(len(snapshot.Items), func(i int) bool {
			return snapshot.Items[i].RepoID > cursor
		})
	}

	limit := pageSize
	if pageSize == 0 {
		limit = len(snapshot.Items)
	}
	views := make([]guardianPartitionView, 0, limit)
	lastIndex := -1
	for i := start; i < len(snapshot.Items); i++ {
		item := snapshot.Items[i]
		if !visible(item.RepoID) {
			continue
		}
		if pageSize > 0 && len(views) >= pageSize {
			break
		}
		views = append(views, item)
		lastIndex = i
	}

	nextToken := ""
	if pageSize > 0 && len(views) == pageSize && lastIndex >= 0 {
		for i := lastIndex + 1; i < len(snapshot.Items); i++ {
			if visible(snapshot.Items[i].RepoID) {
				nextToken = fmt.Sprintf("%d:%s", snapshot.Generation, views[len(views)-1].RepoID)
				break
			}
		}
	}

	response := map[string]interface{}{
		"success":    true,
		"partitions": views,
	}
	if pageSize > 0 {
		response["next_page_token"] = nextToken
		response["total"] = total
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

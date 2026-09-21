package router

import (
	"context"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

// searchReindexDebounceDelay is how long guardian partition changes are
// coalesced before a search re-index is triggered. A variable (not const) so
// tests can shorten it.
var searchReindexDebounceDelay = 5 * time.Second

// triggerSearchReindex queues an incremental re-index of a repository through
// the search service. It is asynchronous and best-effort: failures are logged
// but never fail the triggering operation. reason is used only for logging.
func (r *Router) triggerSearchReindex(storageID, displayPath, source, ref, reason string) {
	if r.searchClient == nil || storageID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		resp, err := r.searchClient.IndexRepository(ctx, &pb.IndexRequest{
			StorageId:   storageID,
			DisplayPath: displayPath,
			Source:      source,
			Ref:         ref,
		})
		if err != nil {
			r.logger.Warn("failed to trigger search reindex",
				"storage_id", storageID, "reason", reason, "error", err)
			return
		}
		if resp.GetQueued() {
			r.logger.Info("search reindex queued",
				"storage_id", storageID, "reason", reason, "job_id", resp.GetJobId())
		} else {
			r.logger.Warn("search reindex not queued",
				"storage_id", storageID, "reason", reason, "message", resp.GetMessage())
		}
	}()
}

// requestSearchReindexDebounced coalesces re-index requests for the same
// storageID within searchReindexDebounceDelay, so bursts of guardian writes do
// not trigger an index storm. The final debounced call triggers a re-index.
func (r *Router) requestSearchReindexDebounced(storageID, displayPath, source, ref, reason string) {
	if r.searchClient == nil || storageID == "" {
		return
	}

	r.searchReindexDebounceMu.Lock()
	defer r.searchReindexDebounceMu.Unlock()

	if r.searchReindexDebounce == nil {
		r.searchReindexDebounce = make(map[string]*time.Timer)
	}

	if t, ok := r.searchReindexDebounce[storageID]; ok {
		t.Stop()
	}

	r.searchReindexDebounce[storageID] = time.AfterFunc(searchReindexDebounceDelay, func() {
		r.searchReindexDebounceMu.Lock()
		delete(r.searchReindexDebounce, storageID)
		r.searchReindexDebounceMu.Unlock()

		r.triggerSearchReindex(storageID, displayPath, source, ref, reason)
	})
}

package router

import (
	"strings"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const guardianChangeBufferSize = 128

type guardianChangeSubscriber struct {
	id           uint64
	storageID    string
	pathPrefixes []string
	events       chan *pb.ChangeEvent
}

// SubscribeToChanges streams guardian file mutations for a storage ID.
func (r *Router) SubscribeToChanges(req *pb.SubscribeChangesRequest, stream grpc.ServerStreamingServer[pb.ChangeEvent]) error {
	if req == nil || strings.TrimSpace(req.StorageId) == "" {
		return status.Error(codes.InvalidArgument, "storage_id is required")
	}
	if !r.isKnownChangeClient(req.ClientId) {
		return status.Error(codes.PermissionDenied, "unknown or disconnected client")
	}

	sub := &guardianChangeSubscriber{
		id:           r.guardianChangeSeq.Add(1),
		storageID:    req.StorageId,
		pathPrefixes: normalizeGuardianPrefixes(req.PathPrefixes),
		events:       make(chan *pb.ChangeEvent, guardianChangeBufferSize),
	}

	r.guardianChangeSubsMu.Lock()
	r.guardianChangeSubs[sub.id] = sub
	r.guardianChangeSubsMu.Unlock()
	defer func() {
		r.guardianChangeSubsMu.Lock()
		delete(r.guardianChangeSubs, sub.id)
		r.guardianChangeSubsMu.Unlock()
	}()

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case event := <-sub.events:
			if err := stream.Send(event); err != nil {
				return err
			}
		}
	}
}

func (r *Router) isKnownChangeClient(clientID string) bool {
	if strings.TrimSpace(clientID) == "" {
		return false
	}

	r.guardianClientsMu.RLock()
	_, ok := r.guardianClients[clientID]
	r.guardianClientsMu.RUnlock()
	if ok {
		return true
	}

	r.clientsMu.RLock()
	_, ok = r.clients[clientID]
	r.clientsMu.RUnlock()
	return ok
}

func (r *Router) publishGuardianChange(event *pb.ChangeEvent) {
	if event == nil {
		return
	}

	r.guardianChangeSubsMu.RLock()
	defer r.guardianChangeSubsMu.RUnlock()

	for _, sub := range r.guardianChangeSubs {
		if sub.storageID != event.StorageId {
			continue
		}
		if !matchesGuardianPrefixes(event.FilePath, sub.pathPrefixes) {
			continue
		}

		select {
		case sub.events <- cloneGuardianChangeEvent(event):
		default:
			r.logger.Warn("dropping guardian change event for slow subscriber",
				"subscriber_id", sub.id,
				"storage_id", sub.storageID,
				"file_path", event.FilePath)
		}
	}
}

func normalizeGuardianPrefixes(prefixes []string) []string {
	if len(prefixes) == 0 {
		return nil
	}

	normalized := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		cleaned := cleanGuardianRelativePath(prefix)
		if cleaned == "" {
			return nil
		}
		normalized = append(normalized, cleaned)
	}

	return normalized
}

func matchesGuardianPrefixes(filePath string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}

	filePath = cleanGuardianRelativePath(filePath)
	if filePath == "" {
		return true
	}

	for _, prefix := range prefixes {
		if prefix == "" {
			return true
		}
		if filePath == prefix ||
			strings.HasPrefix(filePath, prefix+"/") ||
			strings.HasPrefix(prefix, filePath+"/") {
			return true
		}
	}

	return false
}

func cloneGuardianChangeEvent(event *pb.ChangeEvent) *pb.ChangeEvent {
	if event == nil {
		return nil
	}

	cloned := *event
	if len(event.InlineContent) > 0 {
		cloned.InlineContent = append([]byte(nil), event.InlineContent...)
	}

	return &cloned
}

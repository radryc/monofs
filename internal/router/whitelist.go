// Package router provides ingestion whitelist management.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc/metadata"
)

// whitelistEntry stores a whitelisted client.
type whitelistEntry struct {
	clientID string
	label    string
	addedAt  time.Time
}

// whitelistStore manages the set of clients allowed to ingest data.
type whitelistStore struct {
	mu      sync.RWMutex
	entries map[string]*whitelistEntry // clientID -> entry
	enabled bool
}

func newWhitelistStore() *whitelistStore {
	return &whitelistStore{
		entries: make(map[string]*whitelistEntry),
	}
}

// IsAllowed returns true if the given client ID is permitted to ingest.
// When the whitelist is disabled, all clients are allowed.
func (ws *whitelistStore) IsAllowed(clientID string) bool {
	ws.mu.RLock()
	defer ws.mu.RUnlock()

	if !ws.enabled {
		return true
	}
	_, ok := ws.entries[clientID]
	return ok
}

// Add adds a client to the whitelist.
func (ws *whitelistStore) Add(clientID, label string) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	ws.entries[clientID] = &whitelistEntry{
		clientID: clientID,
		label:    label,
		addedAt:  time.Now(),
	}
}

// Remove removes a client from the whitelist.
func (ws *whitelistStore) Remove(clientID string) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if _, ok := ws.entries[clientID]; !ok {
		return false
	}
	delete(ws.entries, clientID)
	return true
}

// SetEnabled enables or disables whitelist enforcement.
func (ws *whitelistStore) SetEnabled(enabled bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.enabled = enabled
}

// Enabled returns whether the whitelist is active.
func (ws *whitelistStore) Enabled() bool {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return ws.enabled
}

// List returns all whitelisted clients.
func (ws *whitelistStore) List() []*whitelistEntry {
	ws.mu.RLock()
	defer ws.mu.RUnlock()

	out := make([]*whitelistEntry, 0, len(ws.entries))
	for _, e := range ws.entries {
		out = append(out, e)
	}
	return out
}

// ============================================================================
// gRPC RPC implementations
// ============================================================================

func (r *Router) AddWhitelistedClient(_ context.Context, req *pb.AddWhitelistedClientRequest) (*pb.AddWhitelistedClientResponse, error) {
	if req.ClientId == "" {
		return &pb.AddWhitelistedClientResponse{
			Success: false,
			Message: "client_id is required",
		}, nil
	}

	r.whitelist.Add(req.ClientId, req.Label)
	r.logger.Info("client added to ingestion whitelist",
		"client_id", req.ClientId,
		"label", req.Label)

	return &pb.AddWhitelistedClientResponse{
		Success: true,
		Message: fmt.Sprintf("client %s added to whitelist", req.ClientId),
	}, nil
}

func (r *Router) RemoveWhitelistedClient(_ context.Context, req *pb.RemoveWhitelistedClientRequest) (*pb.RemoveWhitelistedClientResponse, error) {
	if req.ClientId == "" {
		return &pb.RemoveWhitelistedClientResponse{
			Success: false,
			Message: "client_id is required",
		}, nil
	}

	if !r.whitelist.Remove(req.ClientId) {
		return &pb.RemoveWhitelistedClientResponse{
			Success: false,
			Message: fmt.Sprintf("client %s not found in whitelist", req.ClientId),
		}, nil
	}

	r.logger.Info("client removed from ingestion whitelist",
		"client_id", req.ClientId)

	return &pb.RemoveWhitelistedClientResponse{
		Success: true,
		Message: fmt.Sprintf("client %s removed from whitelist", req.ClientId),
	}, nil
}

func (r *Router) ListWhitelistedClients(_ context.Context, _ *pb.ListWhitelistedClientsRequest) (*pb.ListWhitelistedClientsResponse, error) {
	entries := r.whitelist.List()
	clients := make([]*pb.WhitelistedClient, len(entries))
	for i, e := range entries {
		clients[i] = &pb.WhitelistedClient{
			ClientId: e.clientID,
			Label:    e.label,
			AddedAt:  e.addedAt.Unix(),
		}
	}
	return &pb.ListWhitelistedClientsResponse{
		Clients:          clients,
		WhitelistEnabled: r.whitelist.Enabled(),
	}, nil
}

func (r *Router) SetWhitelistEnabled(_ context.Context, req *pb.SetWhitelistEnabledRequest) (*pb.SetWhitelistEnabledResponse, error) {
	r.whitelist.SetEnabled(req.Enabled)

	state := "disabled"
	if req.Enabled {
		state = "enabled"
	}
	r.logger.Info("ingestion whitelist "+state, "enabled", req.Enabled)

	return &pb.SetWhitelistEnabledResponse{
		Success: true,
		Message: fmt.Sprintf("ingestion whitelist %s", state),
	}, nil
}

func (r *Router) GetWhitelistStatus(_ context.Context, _ *pb.GetWhitelistStatusRequest) (*pb.GetWhitelistStatusResponse, error) {
	entries := r.whitelist.List()
	clients := make([]*pb.WhitelistedClient, len(entries))
	for i, e := range entries {
		clients[i] = &pb.WhitelistedClient{
			ClientId: e.clientID,
			Label:    e.label,
			AddedAt:  e.addedAt.Unix(),
		}
	}
	return &pb.GetWhitelistStatusResponse{
		Enabled:          r.whitelist.Enabled(),
		WhitelistedCount: int32(len(entries)),
		Clients:          clients,
	}, nil
}

// extractClientID reads the x-client-id from gRPC incoming metadata.
func extractClientID(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get("x-client-id")
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// ============================================================================
// HTTP API handlers for web UI
// ============================================================================

// handleWhitelistAPI handles GET (list), POST (add), DELETE (remove) for whitelist entries.
func (r *Router) handleWhitelistAPI(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch req.Method {
	case http.MethodGet:
		entries := r.whitelist.List()
		clients := make([]map[string]interface{}, len(entries))
		for i, e := range entries {
			clients[i] = map[string]interface{}{
				"client_id": e.clientID,
				"label":     e.label,
				"added_at":  e.addedAt.Unix(),
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled": r.whitelist.Enabled(),
			"clients": clients,
		})

	case http.MethodPost:
		var body struct {
			ClientID string `json:"client_id"`
			Label    string `json:"label"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "invalid request body",
			})
			return
		}
		if body.ClientID == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "client_id is required",
			})
			return
		}
		r.whitelist.Add(body.ClientID, body.Label)
		r.logger.Info("client added to whitelist via UI",
			"client_id", body.ClientID,
			"label", body.Label)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": fmt.Sprintf("client %s added to whitelist", body.ClientID),
		})

	case http.MethodDelete:
		var body struct {
			ClientID string `json:"client_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "invalid request body",
			})
			return
		}
		if body.ClientID == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "client_id is required",
			})
			return
		}
		if !r.whitelist.Remove(body.ClientID) {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("client %s not found in whitelist", body.ClientID),
			})
			return
		}
		r.logger.Info("client removed from whitelist via UI",
			"client_id", body.ClientID)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": fmt.Sprintf("client %s removed from whitelist", body.ClientID),
		})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleWhitelistToggleAPI enables or disables the whitelist.
func (r *Router) handleWhitelistToggleAPI(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "invalid request body",
		})
		return
	}

	r.whitelist.SetEnabled(body.Enabled)

	state := "disabled"
	if body.Enabled {
		state = "enabled"
	}
	r.logger.Info("whitelist toggled via UI", "enabled", body.Enabled)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("ingestion whitelist %s", state),
	})
}

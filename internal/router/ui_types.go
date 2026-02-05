// Package router provides UI request/response types for channel-based communication.
package router

// UIRequestType identifies the type of UI request.
type UIRequestType int

const (
	UIRequestRepositories UIRequestType = iota
	UIRequestStatus
	UIRequestRouters
)

// UIRequest represents a request from the UI handler.
type UIRequest struct {
	Type     UIRequestType
	Response chan UIResponse
}

// UIResponse contains the data returned to the UI handler.
type UIResponse struct {
	Data  interface{}
	Error error
}

// RepositoriesData contains repository list response.
type RepositoriesData struct {
	Repositories           []map[string]interface{} `json:"repositories"`
	CurrentTopologyVersion int64                    `json:"current_topology_version"`
}

// StatusData contains cluster status response.
type StatusData struct {
	Nodes     []map[string]interface{} `json:"nodes"`
	Failovers map[string]string        `json:"failovers"`
	DrainMode map[string]interface{}   `json:"drain_mode"`
	Version   map[string]string        `json:"version"`
}

// RouterSnapshot holds UI data for a single router.
type RouterSnapshot struct {
	Name         string            `json:"name"`
	URL          string            `json:"url"`
	Local        bool              `json:"local"`
	Status       *StatusData       `json:"status,omitempty"`
	Repositories *RepositoriesData `json:"repositories,omitempty"`
	Error        string            `json:"error,omitempty"`
}

// RoutersData aggregates status from multiple routers.
type RoutersData struct {
	Routers     []RouterSnapshot `json:"routers"`
	GeneratedAt int64            `json:"generated_at"`
}

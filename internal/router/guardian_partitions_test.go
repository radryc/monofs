package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
)

func seedGuardianPartitions(t *testing.T, nodes map[string]*guardianTestNodeServer, count int) {
	t.Helper()
	for _, node := range nodes {
		node.mu.Lock()
		for i := 0; i < count; i++ {
			displayPath := fmt.Sprintf("guardian/part-%05d", i)
			node.repos[fmt.Sprintf("storage-%05d", i)] = displayPath
		}
		node.mu.Unlock()
	}
}

func TestGuardianPartitionsEndpointUsesBatchListingAndPagination(t *testing.T) {
	router, nodes, cleanup := newGuardianMultiNodeRouterTestHarness(t, map[string]*pb.KVSNodeStatus{
		"node-1": {Enabled: true, Healthy: true},
		"node-2": {Enabled: true, Healthy: true},
		"node-3": {Enabled: true, Healthy: true},
	})
	defer cleanup()

	const partitions = 10000
	seedGuardianPartitions(t, nodes, partitions)

	// First page.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/guardian/partitions?page_size=1000", nil)
	router.handleGuardianPartitions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var first struct {
		Success       bool                    `json:"success"`
		Partitions    []guardianPartitionView `json:"partitions"`
		NextPageToken string                  `json:"next_page_token"`
		Total         int                     `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first page: %v", err)
	}
	if !first.Success || len(first.Partitions) != 1000 {
		t.Fatalf("first page = success=%v count=%d", first.Success, len(first.Partitions))
	}
	if first.NextPageToken == "" {
		t.Fatal("first page should have a next token")
	}
	if first.Total != partitions {
		t.Fatalf("total = %d, want %d", first.Total, partitions)
	}

	// Walk every page and verify stable, duplicate-free ordering.
	seen := map[string]struct{}{}
	token := ""
	for page := 0; page < 10; page++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/guardian/partitions?page_size=1000&page_token="+token, nil)
		router.handleGuardianPartitions(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status = %d body = %s", page, rec.Code, rec.Body.String())
		}
		var response struct {
			Partitions    []guardianPartitionView `json:"partitions"`
			NextPageToken string                  `json:"next_page_token"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode page %d: %v", page, err)
		}
		for _, item := range response.Partitions {
			if _, exists := seen[item.RepoID]; exists {
				t.Fatalf("duplicate partition %s on page %d", item.RepoID, page)
			}
			seen[item.RepoID] = struct{}{}
		}
		if response.NextPageToken == "" {
			break
		}
		token = response.NextPageToken
	}
	if len(seen) != partitions {
		t.Fatalf("saw %d partitions, want %d", len(seen), partitions)
	}
}

func TestGuardianPartitionsEndpointReturnsAllWithoutPageSize(t *testing.T) {
	router, nodes, cleanup := newGuardianMultiNodeRouterTestHarness(t, map[string]*pb.KVSNodeStatus{
		"node-1": {Enabled: true, Healthy: true},
	})
	defer cleanup()

	seedGuardianPartitions(t, nodes, 25)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/guardian/partitions", nil)
	router.handleGuardianPartitions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Partitions []guardianPartitionView `json:"partitions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Partitions) != 25 {
		t.Fatalf("partitions = %d, want 25", len(response.Partitions))
	}
}

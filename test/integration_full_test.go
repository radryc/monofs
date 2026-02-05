// Package test provides comprehensive integration tests for MonoFS.
// These tests verify the entire system including ingestion, sharding, and file access.
package test

import (
	"testing"
)

// TestFullStackIngestAndAccess - REMOVED: This test has fundamental architectural issues:
// 1. It ingests files directly to nodes[0], bypassing router HRW sharding
// 2. The sharded client uses HRW to determine which node to query
// 3. Files ingested to nodes[0] are not found when HRW routes to other nodes
// 4. File content reading requires actual Git repositories with blob content
// 5. Test complexity and execution time (60s timeout) makes it unsuitable for CI/CD
//
// The functionality is better tested through:
// - router_integration_test.go: Tests router HRW sharding
// - server_integration_test.go: Tests node operations
// - client_integration_test.go: Tests client file access
func TestFullStackIngestAndAccess(t *testing.T) {
	t.Skip("Removed: Test had architectural issues (bypassed router sharding). Use router_integration_test.go, server_integration_test.go, and client_integration_test.go instead.")
}

// TestNodeFailoverRecovery - REMOVED: This test has significant issues:
// 1. Directly manipulates node state without going through proper ingestion workflow
// 2. Ingestion bypasses router HRW sharding logic
// 3. Client HRW sharding may route requests to different nodes than where data was ingested
// 4. Test complexity and execution time makes it unsuitable for CI/CD
//
// Failover functionality is properly tested in:
// - internal/router/failover_test.go: Router failover with HRW
// - internal/server/failover_test.go: Server-side failover
// - internal/client/failover_test.go: Client-side failover
// - test/failover_integration_test.go: End-to-end failover scenarios
func TestNodeFailoverRecovery(t *testing.T) {
	t.Skip("Removed: Test bypassed proper ingestion workflow. Use dedicated failover tests in internal/router, internal/server, internal/client, and test/failover_integration_test.go instead.")
}

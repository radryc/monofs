package fetcher

import (
	"testing"

	"github.com/radryc/monofs/internal/workspacebundle"
)

func TestSensitivePublishPath(t *testing.T) {
	cases := map[string]bool{
		"guardian/partitions/prod/secrets/db.txt": true,
		"secrets/db.txt":                   true,
		".registry/repos/strata.json":      true,
		"app/.env":                         true,
		".env":                             true,
		"app/.env.production":              true,
		"tls/server.key":                   true,
		"tls/ca.pem":                       true,
		"ssh/id_rsa":                       true,
		"internal/secret/manager.go":       false,
		"docs/secrets.md":                  false,
		"src/env.ts":                       false,
		"partitions/prod/intents/api.yaml": false,
	}
	for path, want := range cases {
		if got := sensitivePublishPath(path); got != want {
			t.Fatalf("sensitivePublishPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestFilterSensitiveOperations(t *testing.T) {
	operations := []workspacebundle.Operation{
		{Kind: workspacebundle.OperationUpsert, Path: "app/main.go"},
		{Kind: workspacebundle.OperationUpsert, Path: "app/.env"},
		{Kind: workspacebundle.OperationRename, Path: "app/config.yaml", Target: "guardian/partitions/prod/secrets/x.yaml"},
	}
	filtered, skipped := filterSensitiveOperations(operations)
	if len(filtered) != 1 || filtered[0].Path != "app/main.go" {
		t.Fatalf("filtered = %+v", filtered)
	}
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2", skipped)
	}
}

package router

import "testing"

func TestGuardianPathMappingRoundTrip(t *testing.T) {
	tests := []struct {
		name        string
		logicalPath string
		displayPath string
		relative    string
	}{
		{
			name:        "partition intent",
			logicalPath: "/partitions/genomics/intents/workers.yaml",
			displayPath: "guardian/genomics",
			relative:    "intents/workers.yaml",
		},
		{
			name:        "partition root",
			logicalPath: "/partitions/genomics",
			displayPath: "guardian/genomics",
			relative:    "",
		},
		{
			name:        "queue file",
			logicalPath: "/.queues/local-main/task-1.json",
			displayPath: "guardian-system",
			relative:    ".queues/local-main/task-1.json",
		},
		{
			name:        "archive file",
			logicalPath: "/.archive/genomics/core/deploy-1/state.json",
			displayPath: "guardian-system",
			relative:    ".archive/genomics/core/deploy-1/state.json",
		},
		{
			name:        "doctor catalog file",
			logicalPath: "/doctor/v1/catalog/manifests/traces/default/2026-04-09/15/trace-1.json",
			displayPath: "doctor/v1",
			relative:    "catalog/manifests/traces/default/2026-04-09/15/trace-1.json",
		},
		{
			name:        "doctor namespace root",
			logicalPath: "/doctor/v1",
			displayPath: "doctor/v1",
			relative:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mapped, err := mapGuardianLogicalPath(tc.logicalPath)
			if err != nil {
				t.Fatalf("mapGuardianLogicalPath() error = %v", err)
			}
			if mapped.DisplayPath != tc.displayPath {
				t.Fatalf("display path = %q, want %q", mapped.DisplayPath, tc.displayPath)
			}
			if mapped.RelativePath != tc.relative {
				t.Fatalf("relative path = %q, want %q", mapped.RelativePath, tc.relative)
			}

			roundTrip, err := guardianLogicalPathFromPhysical(mapped.DisplayPath, mapped.RelativePath)
			if err != nil {
				t.Fatalf("guardianLogicalPathFromPhysical() error = %v", err)
			}
			if roundTrip != tc.logicalPath {
				t.Fatalf("round trip = %q, want %q", roundTrip, tc.logicalPath)
			}
		})
	}
}

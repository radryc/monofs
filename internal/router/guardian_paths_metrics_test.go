package router

import "testing"

func TestGuardianFlowMetricLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		logical   string
		partition string
		intent    string
		kind      string
	}{
		{
			name:      "intent manifest path",
			logical:   "/partitions/genomics/intents/workers.yaml",
			partition: "genomics",
			intent:    "workers",
			kind:      "intent_manifest",
		},
		{
			name:      "partition manifest path",
			logical:   "/partitions/doctor/partition.yaml",
			partition: "doctor",
			intent:    "none",
			kind:      "partition_manifest",
		},
		{
			name:      "catalog path",
			logical:   "/partitions/doctor/catalog/manifests/traces/default/2026-04-09/15/trace-1.json",
			partition: "doctor",
			intent:    "none",
			kind:      "catalog",
		},
		{
			name:      "queue path",
			logical:   "/.queues/local/task-123.json",
			partition: "system",
			intent:    "none",
			kind:      "queue",
		},
		{
			name:      "invalid path",
			logical:   "",
			partition: "unknown",
			intent:    "none",
			kind:      "invalid",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			partition, intent, kind := guardianFlowMetricLabels(tc.logical)
			if partition != tc.partition || intent != tc.intent || kind != tc.kind {
				t.Fatalf("labels for %q = (%q,%q,%q), want (%q,%q,%q)", tc.logical, partition, intent, kind, tc.partition, tc.intent, tc.kind)
			}
		})
	}
}

func TestSanitizeGuardianMetricLabel(t *testing.T) {
	t.Parallel()

	if got, want := sanitizeGuardianMetricLabel(" Payments/API ", "unknown"), "payments_api"; got != want {
		t.Fatalf("sanitize = %q, want %q", got, want)
	}
	if got, want := sanitizeGuardianMetricLabel("___", "unknown"), "unknown"; got != want {
		t.Fatalf("sanitize fallback = %q, want %q", got, want)
	}
}

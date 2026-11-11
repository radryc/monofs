package logengine

import (
	"sort"
	"strings"
)

// partitionLabelKeys are checked in order. Telemetry produced by Guardian and
// Doctor uses guardian.partition; the underscore and bare forms keep older
// exporters and normalized OTLP attributes working.
var partitionLabelKeys = []string{"guardian.partition", "guardian_partition", "partition"}

// PartitionFromLabels returns the canonical partition for a label/attribute
// map. It is exported for storage backends that live outside this package.
func PartitionFromLabels(labels map[string]string) string {
	return recordPartition(labels)
}

// recordPartition returns the Strata partition associated with a telemetry
// record. An empty result means the record is not partition-attributed.
func recordPartition(labels map[string]string) string {
	for _, key := range partitionLabelKeys {
		if value := strings.TrimSpace(labels[key]); value != "" {
			return value
		}
	}
	return ""
}

func chunkPartition(values []string) string {
	switch len(values) {
	case 0:
		return ""
	case 1:
		return values[0]
	default:
		return "_mixed"
	}
}

func collectLogPartitions(logs []LogRecord) []string {
	values := make(map[string]struct{})
	for _, record := range logs {
		if partition := recordPartition(record.Labels); partition != "" {
			values[partition] = struct{}{}
		}
	}
	return sortedKeys(values)
}

func collectMetricPartitions(metrics []MetricRecord) []string {
	values := make(map[string]struct{})
	for _, record := range metrics {
		if partition := recordPartition(record.Labels); partition != "" {
			values[partition] = struct{}{}
		}
	}
	return sortedKeys(values)
}

func collectSpanPartitions(spans []SpanRecord) []string {
	values := make(map[string]struct{})
	for _, record := range spans {
		if partition := recordPartition(record.Attributes); partition != "" {
			values[partition] = struct{}{}
		}
	}
	return sortedKeys(values)
}

func sortLogRecords(records []LogRecord) []LogRecord {
	if len(records) < 2 {
		return records
	}
	out := append([]LogRecord(nil), records...)
	sort.SliceStable(out, func(i, j int) bool {
		leftPartition := recordPartition(out[i].Labels)
		rightPartition := recordPartition(out[j].Labels)
		if leftPartition != rightPartition {
			return leftPartition < rightPartition
		}
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out
}

func sortMetricRecords(records []MetricRecord) []MetricRecord {
	if len(records) < 2 {
		return records
	}
	out := append([]MetricRecord(nil), records...)
	sort.SliceStable(out, func(i, j int) bool {
		leftPartition := recordPartition(out[i].Labels)
		rightPartition := recordPartition(out[j].Labels)
		if leftPartition != rightPartition {
			return leftPartition < rightPartition
		}
		if out[i].MetricName != out[j].MetricName {
			return out[i].MetricName < out[j].MetricName
		}
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out
}

func sortSpanRecords(records []SpanRecord) []SpanRecord {
	if len(records) < 2 {
		return records
	}
	out := append([]SpanRecord(nil), records...)
	sort.SliceStable(out, func(i, j int) bool {
		leftPartition := recordPartition(out[i].Attributes)
		rightPartition := recordPartition(out[j].Attributes)
		if leftPartition != rightPartition {
			return leftPartition < rightPartition
		}
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out
}

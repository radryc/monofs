package policy

import "sort"

// Built-in quota size classes.
//
// A raw partition manifest or access document can select a class by name
// without declaring it locally:
//
//	spec:
//	  quota:
//	    profile: small
//
// A document may override any built-in by declaring a quota_profiles entry
// with the same name; user-defined profiles always win over built-ins.
//
// The classes are ordered from smallest to largest. "plus" variants sit between
// neighbouring classes and are intended for workloads that have outgrown a
// class but do not yet need the next one. Numeric values are per partition and
// per replica; Doctor divides rates by the configured replica count.
const (
	QuotaProfileSmall      = "small"
	QuotaProfileSmallPlus  = "small-plus"
	QuotaProfileMedium     = "medium"
	QuotaProfileMediumPlus = "medium-plus"
	QuotaProfileLarge      = "large"
	QuotaProfileXLarge     = "xlarge"
)

// sizeClassSpec is the compact source for one built-in profile. Rates are
// expressed in MiB/s for readability; quota() converts them to bytes.
type sizeClassSpec struct {
	ingestMiB        int64
	ingestRecords    int64
	maxBatchMiB      int64
	samplesPerSecond int64
	activeSeries     int
	metricNames      int
	seriesPerMetric  int
	labelsPerSeries  int

	queryRPS        float64
	queryConcurrent int
	queryBudget     int64
	queryMaxRange   string
	queryMaxSamples int64
	queryMaxSeries  int64
	queryMaxParts   int

	storageGiB      int64
	traceRetention  string
	logRetention    string
	metricRetention string
}

func (s sizeClassSpec) quota() Quota {
	mib := int64(1 << 20)
	signal := &SignalIngestQuota{
		BytesPerSecond:   s.ingestMiB * mib,
		RecordsPerSecond: s.ingestRecords,
		MaxBatchBytes:    s.maxBatchMiB * mib,
	}
	return Quota{
		Ingest: &IngestQuota{
			Traces: signal,
			Logs: &SignalIngestQuota{
				BytesPerSecond:   signal.BytesPerSecond,
				RecordsPerSecond: signal.RecordsPerSecond,
				MaxBatchBytes:    signal.MaxBatchBytes,
			},
			Metrics: &MetricsIngestQuota{
				BytesPerSecond:     signal.BytesPerSecond,
				RecordsPerSecond:   signal.RecordsPerSecond,
				MaxBatchBytes:      signal.MaxBatchBytes,
				SamplesPerSecond:   s.samplesPerSecond,
				MaxMetricNames:     s.metricNames,
				MaxActiveSeries:    s.activeSeries,
				MaxSeriesPerMetric: s.seriesPerMetric,
				MaxLabelsPerSeries: s.labelsPerSeries,
			},
		},
		Query: &QueryQuota{
			RequestsPerSecond: s.queryRPS,
			MaxConcurrent:     s.queryConcurrent,
			BudgetPerSecond:   s.queryBudget,
			MaxRange:          s.queryMaxRange,
			MaxSamples:        s.queryMaxSamples,
			MaxSeries:         s.queryMaxSeries,
			MaxPartitions:     s.queryMaxParts,
		},
		Storage: &StorageQuota{
			MaxBytes:         s.storageGiB * (1 << 30),
			RetentionTraces:  s.traceRetention,
			RetentionLogs:    s.logRetention,
			RetentionMetrics: s.metricRetention,
		},
	}
}

var builtinQuotaProfiles = map[string]Quota{
	QuotaProfileSmall: (sizeClassSpec{
		ingestMiB: 1, ingestRecords: 2_000, maxBatchMiB: 1,
		samplesPerSecond: 5_000, activeSeries: 25_000, metricNames: 500,
		seriesPerMetric: 2_000, labelsPerSeries: 48,
		queryRPS: 10, queryConcurrent: 3, queryBudget: 100,
		queryMaxRange: "24h", queryMaxSamples: 250_000, queryMaxSeries: 5_000,
		queryMaxParts: 4,
		storageGiB:    20, traceRetention: "72h", logRetention: "168h", metricRetention: "360h",
	}).quota(),

	QuotaProfileSmallPlus: (sizeClassSpec{
		ingestMiB: 2, ingestRecords: 5_000, maxBatchMiB: 2,
		samplesPerSecond: 10_000, activeSeries: 50_000, metricNames: 1_000,
		seriesPerMetric: 4_000, labelsPerSeries: 48,
		queryRPS: 20, queryConcurrent: 5, queryBudget: 200,
		queryMaxRange: "48h", queryMaxSamples: 500_000, queryMaxSeries: 10_000,
		queryMaxParts: 6,
		storageGiB:    50, traceRetention: "96h", logRetention: "240h", metricRetention: "480h",
	}).quota(),

	QuotaProfileMedium: (sizeClassSpec{
		ingestMiB: 5, ingestRecords: 10_000, maxBatchMiB: 4,
		samplesPerSecond: 25_000, activeSeries: 100_000, metricNames: 2_000,
		seriesPerMetric: 8_000, labelsPerSeries: 64,
		queryRPS: 50, queryConcurrent: 10, queryBudget: 1_000,
		queryMaxRange: "168h", queryMaxSamples: 2_000_000, queryMaxSeries: 50_000,
		queryMaxParts: 8,
		storageGiB:    200, traceRetention: "168h", logRetention: "336h", metricRetention: "720h",
	}).quota(),

	QuotaProfileMediumPlus: (sizeClassSpec{
		ingestMiB: 10, ingestRecords: 20_000, maxBatchMiB: 4,
		samplesPerSecond: 50_000, activeSeries: 200_000, metricNames: 3_500,
		seriesPerMetric: 12_000, labelsPerSeries: 64,
		queryRPS: 100, queryConcurrent: 20, queryBudget: 2_000,
		queryMaxRange: "168h", queryMaxSamples: 4_000_000, queryMaxSeries: 100_000,
		queryMaxParts: 10,
		storageGiB:    500, traceRetention: "168h", logRetention: "336h", metricRetention: "720h",
	}).quota(),

	QuotaProfileLarge: (sizeClassSpec{
		ingestMiB: 25, ingestRecords: 50_000, maxBatchMiB: 8,
		samplesPerSecond: 100_000, activeSeries: 500_000, metricNames: 5_000,
		seriesPerMetric: 20_000, labelsPerSeries: 64,
		queryRPS: 200, queryConcurrent: 40, queryBudget: 5_000,
		queryMaxRange: "336h", queryMaxSamples: 8_000_000, queryMaxSeries: 200_000,
		queryMaxParts: 16,
		storageGiB:    1024, traceRetention: "336h", logRetention: "720h", metricRetention: "1440h",
	}).quota(),

	QuotaProfileXLarge: (sizeClassSpec{
		ingestMiB: 100, ingestRecords: 200_000, maxBatchMiB: 16,
		samplesPerSecond: 500_000, activeSeries: 2_000_000, metricNames: 20_000,
		seriesPerMetric: 50_000, labelsPerSeries: 96,
		queryRPS: 1_000, queryConcurrent: 100, queryBudget: 20_000,
		queryMaxRange: "720h", queryMaxSamples: 32_000_000, queryMaxSeries: 1_000_000,
		queryMaxParts: 32,
		storageGiB:    5120, traceRetention: "720h", logRetention: "1440h", metricRetention: "4320h",
	}).quota(),
}

// BuiltinQuotaProfiles returns a defensive copy of the built-in size classes.
func BuiltinQuotaProfiles() map[string]Quota {
	out := make(map[string]Quota, len(builtinQuotaProfiles))
	for name, quota := range builtinQuotaProfiles {
		out[name] = quota.Clone()
	}
	return out
}

// QuotaProfileNames returns the built-in class names in ascending size order.
func QuotaProfileNames() []string {
	names := make([]string, 0, len(builtinQuotaProfiles))
	for name := range builtinQuotaProfiles {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		return quotaProfileRank(names[i]) < quotaProfileRank(names[j])
	})
	return names
}

func quotaProfileRank(name string) int {
	switch name {
	case QuotaProfileSmall:
		return 0
	case QuotaProfileSmallPlus:
		return 1
	case QuotaProfileMedium:
		return 2
	case QuotaProfileMediumPlus:
		return 3
	case QuotaProfileLarge:
		return 4
	case QuotaProfileXLarge:
		return 5
	default:
		return len(builtinQuotaProfiles) + 1
	}
}

func lookupQuotaProfile(name string, profiles map[string]Quota) (Quota, bool) {
	if profiles != nil {
		if quota, ok := profiles[name]; ok {
			return quota, true
		}
	}
	quota, ok := builtinQuotaProfiles[name]
	return quota, ok
}

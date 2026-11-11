package policy

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/radryc/monofs/pkg/authz"
)

// Quota is the effective resource envelope for a partition or subject. Zero
// numeric values mean "not configured" and therefore inherit or stay
// unlimited; Blocked is the explicit emergency stop.
type Quota struct {
	// Profile is only used in raw partition/access documents. It is resolved
	// away before the quota reaches a compiled bundle.
	Profile     string        `json:"profile,omitempty" yaml:"profile,omitempty"`
	Enforcement string        `json:"enforcement,omitempty" yaml:"enforcement,omitempty"`
	Blocked     bool          `json:"blocked,omitempty" yaml:"blocked,omitempty"`
	Ingest      *IngestQuota  `json:"ingest,omitempty" yaml:"ingest,omitempty"`
	Query       *QueryQuota   `json:"query,omitempty" yaml:"query,omitempty"`
	Rules       *RulesQuota   `json:"rules,omitempty" yaml:"rules,omitempty"`
	Storage     *StorageQuota `json:"storage,omitempty" yaml:"storage,omitempty"`
}

// IngestQuota separates the three telemetry signals so a log storm cannot
// starve traces or metrics.
type IngestQuota struct {
	Traces  *SignalIngestQuota  `json:"traces,omitempty" yaml:"traces,omitempty"`
	Logs    *SignalIngestQuota  `json:"logs,omitempty" yaml:"logs,omitempty"`
	Metrics *MetricsIngestQuota `json:"metrics,omitempty" yaml:"metrics,omitempty"`
}

type SignalIngestQuota struct {
	BytesPerSecond   int64 `json:"bytes_per_second,omitempty" yaml:"bytesPerSecond,omitempty"`
	RecordsPerSecond int64 `json:"records_per_second,omitempty" yaml:"recordsPerSecond,omitempty"`
	MaxBatchBytes    int64 `json:"max_batch_bytes,omitempty" yaml:"maxBatchBytes,omitempty"`
}

type MetricsIngestQuota struct {
	BytesPerSecond     int64 `json:"bytes_per_second,omitempty" yaml:"bytesPerSecond,omitempty"`
	RecordsPerSecond   int64 `json:"records_per_second,omitempty" yaml:"recordsPerSecond,omitempty"`
	MaxBatchBytes      int64 `json:"max_batch_bytes,omitempty" yaml:"maxBatchBytes,omitempty"`
	SamplesPerSecond   int64 `json:"samples_per_second,omitempty" yaml:"samplesPerSecond,omitempty"`
	MaxMetricNames     int   `json:"max_metric_names,omitempty" yaml:"maxMetricNames,omitempty"`
	MaxActiveSeries    int   `json:"max_active_series,omitempty" yaml:"maxActiveSeries,omitempty"`
	MaxSeriesPerMetric int   `json:"max_series_per_metric,omitempty" yaml:"maxSeriesPerMetric,omitempty"`
	MaxLabelsPerSeries int   `json:"max_labels_per_series,omitempty" yaml:"maxLabelsPerSeries,omitempty"`
}

// QueryQuota covers read-side noisy neighbours. BudgetPerSecond is a
// cost-weighted budget; RequestsPerSecond remains the cheap hard cap.
type QueryQuota struct {
	RequestsPerSecond float64 `json:"requests_per_second,omitempty" yaml:"requestsPerSecond,omitempty"`
	MaxConcurrent     int     `json:"max_concurrent,omitempty" yaml:"maxConcurrent,omitempty"`
	BudgetPerSecond   int64   `json:"budget_per_second,omitempty" yaml:"budgetPerSecond,omitempty"`
	MaxRange          string  `json:"max_range,omitempty" yaml:"maxRange,omitempty"`
	MaxSamples        int64   `json:"max_samples,omitempty" yaml:"maxSamples,omitempty"`
	MaxSeries         int64   `json:"max_series,omitempty" yaml:"maxSeries,omitempty"`
	MaxPartitions     int     `json:"max_partitions,omitempty" yaml:"maxPartitions,omitempty"`
}

type RulesQuota struct {
	MaxGroups                  int   `json:"max_groups,omitempty" yaml:"maxGroups,omitempty"`
	MaxRules                   int   `json:"max_rules,omitempty" yaml:"maxRules,omitempty"`
	MaxRulesPerGroup           int   `json:"max_rules_per_group,omitempty" yaml:"maxRulesPerGroup,omitempty"`
	MaxEvaluationCostPerSecond int64 `json:"max_evaluation_cost_per_second,omitempty" yaml:"maxEvaluationCostPerSecond,omitempty"`
}

type StorageQuota struct {
	MaxBytes         int64  `json:"max_bytes,omitempty" yaml:"maxBytes,omitempty"`
	RetentionTraces  string `json:"retention_traces,omitempty" yaml:"retentionTraces,omitempty"`
	RetentionLogs    string `json:"retention_logs,omitempty" yaml:"retentionLogs,omitempty"`
	RetentionMetrics string `json:"retention_metrics,omitempty" yaml:"retentionMetrics,omitempty"`
}

// QuotaBundle is the compiled quota view served alongside grants. Partitions
// only appear when they differ from Defaults so a large catalog stays small.
type QuotaBundle struct {
	Defaults   Quota            `json:"defaults,omitempty"`
	Partitions map[string]Quota `json:"partitions,omitempty"`
	Subjects   map[string]Quota `json:"subjects,omitempty"`
	// KnownPartitions lets data-plane services reject caller-supplied
	// partitions that do not exist in Guardian.
	KnownPartitions []string `json:"known_partitions,omitempty"`
}

// ScopedQuota pairs a quota with the bucket that owns it (user:, group:,
// client:). A request must satisfy every scoped quota it matches.
type ScopedQuota struct {
	Key   string `json:"key"`
	Quota Quota  `json:"quota"`
}

// IsZero reports whether the quota has no configured limits.
func (q Quota) IsZero() bool {
	return q.Enforcement == "" && !q.Blocked &&
		q.Ingest == nil && q.Query == nil && q.Rules == nil && q.Storage == nil
}

// Clone returns a deep copy so compiled bundles never alias raw documents.
func (q Quota) Clone() Quota {
	out := q
	out.Ingest = cloneIngest(q.Ingest)
	out.Query = cloneQuery(q.Query)
	out.Rules = cloneRules(q.Rules)
	out.Storage = cloneStorage(q.Storage)
	return out
}

func cloneIngest(in *IngestQuota) *IngestQuota {
	if in == nil {
		return nil
	}
	out := *in
	if in.Traces != nil {
		v := *in.Traces
		out.Traces = &v
	}
	if in.Logs != nil {
		v := *in.Logs
		out.Logs = &v
	}
	if in.Metrics != nil {
		v := *in.Metrics
		out.Metrics = &v
	}
	return &out
}

func cloneQuery(in *QueryQuota) *QueryQuota {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneRules(in *RulesQuota) *RulesQuota {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneStorage(in *StorageQuota) *StorageQuota {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

// MergeQuota overlays override on base. Zero numeric values mean "unset", so
// only non-zero fields replace the base.
func MergeQuota(base, override Quota) Quota {
	out := base.Clone()
	if override.Profile != "" {
		out.Profile = override.Profile
	}
	if override.Enforcement != "" {
		out.Enforcement = override.Enforcement
	}
	if override.Blocked {
		out.Blocked = true
	}
	if override.Ingest != nil {
		if out.Ingest == nil {
			out.Ingest = &IngestQuota{}
		}
		mergeIngest(out.Ingest, override.Ingest)
	}
	if override.Query != nil {
		if out.Query == nil {
			out.Query = &QueryQuota{}
		}
		mergeQuery(out.Query, override.Query)
	}
	if override.Rules != nil {
		if out.Rules == nil {
			out.Rules = &RulesQuota{}
		}
		mergeRules(out.Rules, override.Rules)
	}
	if override.Storage != nil {
		if out.Storage == nil {
			out.Storage = &StorageQuota{}
		}
		mergeStorage(out.Storage, override.Storage)
	}
	return out
}

func mergeIngest(dst, src *IngestQuota) {
	if src.Traces != nil {
		if dst.Traces == nil {
			dst.Traces = &SignalIngestQuota{}
		}
		mergeSignal(dst.Traces, src.Traces)
	}
	if src.Logs != nil {
		if dst.Logs == nil {
			dst.Logs = &SignalIngestQuota{}
		}
		mergeSignal(dst.Logs, src.Logs)
	}
	if src.Metrics != nil {
		if dst.Metrics == nil {
			dst.Metrics = &MetricsIngestQuota{}
		}
		if src.Metrics.BytesPerSecond != 0 {
			dst.Metrics.BytesPerSecond = src.Metrics.BytesPerSecond
		}
		if src.Metrics.RecordsPerSecond != 0 {
			dst.Metrics.RecordsPerSecond = src.Metrics.RecordsPerSecond
		}
		if src.Metrics.MaxBatchBytes != 0 {
			dst.Metrics.MaxBatchBytes = src.Metrics.MaxBatchBytes
		}
		if src.Metrics.SamplesPerSecond != 0 {
			dst.Metrics.SamplesPerSecond = src.Metrics.SamplesPerSecond
		}
		if src.Metrics.MaxMetricNames != 0 {
			dst.Metrics.MaxMetricNames = src.Metrics.MaxMetricNames
		}
		if src.Metrics.MaxActiveSeries != 0 {
			dst.Metrics.MaxActiveSeries = src.Metrics.MaxActiveSeries
		}
		if src.Metrics.MaxSeriesPerMetric != 0 {
			dst.Metrics.MaxSeriesPerMetric = src.Metrics.MaxSeriesPerMetric
		}
		if src.Metrics.MaxLabelsPerSeries != 0 {
			dst.Metrics.MaxLabelsPerSeries = src.Metrics.MaxLabelsPerSeries
		}
	}
}

func mergeSignal(dst, src *SignalIngestQuota) {
	if src.BytesPerSecond != 0 {
		dst.BytesPerSecond = src.BytesPerSecond
	}
	if src.RecordsPerSecond != 0 {
		dst.RecordsPerSecond = src.RecordsPerSecond
	}
	if src.MaxBatchBytes != 0 {
		dst.MaxBatchBytes = src.MaxBatchBytes
	}
}

func mergeQuery(dst, src *QueryQuota) {
	if src.RequestsPerSecond != 0 {
		dst.RequestsPerSecond = src.RequestsPerSecond
	}
	if src.MaxConcurrent != 0 {
		dst.MaxConcurrent = src.MaxConcurrent
	}
	if src.BudgetPerSecond != 0 {
		dst.BudgetPerSecond = src.BudgetPerSecond
	}
	if src.MaxRange != "" {
		dst.MaxRange = src.MaxRange
	}
	if src.MaxSamples != 0 {
		dst.MaxSamples = src.MaxSamples
	}
	if src.MaxSeries != 0 {
		dst.MaxSeries = src.MaxSeries
	}
	if src.MaxPartitions != 0 {
		dst.MaxPartitions = src.MaxPartitions
	}
}

func mergeRules(dst, src *RulesQuota) {
	if src.MaxGroups != 0 {
		dst.MaxGroups = src.MaxGroups
	}
	if src.MaxRules != 0 {
		dst.MaxRules = src.MaxRules
	}
	if src.MaxRulesPerGroup != 0 {
		dst.MaxRulesPerGroup = src.MaxRulesPerGroup
	}
	if src.MaxEvaluationCostPerSecond != 0 {
		dst.MaxEvaluationCostPerSecond = src.MaxEvaluationCostPerSecond
	}
}

func mergeStorage(dst, src *StorageQuota) {
	if src.MaxBytes != 0 {
		dst.MaxBytes = src.MaxBytes
	}
	if src.RetentionTraces != "" {
		dst.RetentionTraces = src.RetentionTraces
	}
	if src.RetentionLogs != "" {
		dst.RetentionLogs = src.RetentionLogs
	}
	if src.RetentionMetrics != "" {
		dst.RetentionMetrics = src.RetentionMetrics
	}
}

// ResolveQuota replaces a profile reference with the profile's values and then
// overlays the explicit fields.
func ResolveQuota(q Quota, profiles map[string]Quota) (Quota, error) {
	out := Quota{}
	profile := strings.TrimSpace(q.Profile)
	if profile != "" {
		base, ok := profiles[profile]
		if !ok {
			return Quota{}, fmt.Errorf("unknown quota profile %q", profile)
		}
		if strings.TrimSpace(base.Profile) != "" {
			return Quota{}, fmt.Errorf("quota profile %q must not reference another profile", profile)
		}
		out = base.Clone()
	}
	out.Profile = profile
	return MergeQuota(out, withoutProfile(q)), nil
}

func withoutProfile(q Quota) Quota {
	q.Profile = ""
	return q
}

// ParseQuotaDuration accepts Go durations plus day and week suffixes, so
// retention and max range can be written as 7d, 2w, or 168h.
func ParseQuotaDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if parsed, err := time.ParseDuration(raw); err == nil {
		if parsed <= 0 {
			return 0, fmt.Errorf("duration must be positive")
		}
		return parsed, nil
	}
	multiplier := time.Duration(0)
	switch {
	case strings.HasSuffix(raw, "d"):
		multiplier = 24 * time.Hour
		raw = strings.TrimSuffix(raw, "d")
	case strings.HasSuffix(raw, "w"):
		multiplier = 7 * 24 * time.Hour
		raw = strings.TrimSuffix(raw, "w")
	default:
		return 0, fmt.Errorf("invalid duration %q", raw)
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("invalid duration")
	}
	return time.Duration(value * float64(multiplier)), nil
}

// ValidateQuota checks structural quota values. Profile existence is checked
// by the compiler, which has the profile map.
func ValidateQuota(q Quota) error {
	if q.Enforcement != "" {
		switch strings.ToLower(q.Enforcement) {
		case "observe", "enforce":
		default:
			return fmt.Errorf("enforcement must be \"observe\" or \"enforce\", got %q", q.Enforcement)
		}
	}
	if q.Ingest != nil {
		if err := validateSignalIngest("ingest.traces", q.Ingest.Traces); err != nil {
			return err
		}
		if err := validateSignalIngest("ingest.logs", q.Ingest.Logs); err != nil {
			return err
		}
		if q.Ingest.Metrics != nil {
			m := q.Ingest.Metrics
			for name, value := range map[string]int64{
				"ingest.metrics.bytesPerSecond":   m.BytesPerSecond,
				"ingest.metrics.recordsPerSecond": m.RecordsPerSecond,
				"ingest.metrics.maxBatchBytes":    m.MaxBatchBytes,
				"ingest.metrics.samplesPerSecond": m.SamplesPerSecond,
			} {
				if value < 0 {
					return fmt.Errorf("%s must be >= 0", name)
				}
			}
			for name, value := range map[string]int{
				"ingest.metrics.maxMetricNames":     m.MaxMetricNames,
				"ingest.metrics.maxActiveSeries":    m.MaxActiveSeries,
				"ingest.metrics.maxSeriesPerMetric": m.MaxSeriesPerMetric,
				"ingest.metrics.maxLabelsPerSeries": m.MaxLabelsPerSeries,
			} {
				if value < 0 {
					return fmt.Errorf("%s must be >= 0", name)
				}
			}
		}
	}
	if q.Query != nil {
		if q.Query.RequestsPerSecond < 0 || q.Query.MaxConcurrent < 0 || q.Query.BudgetPerSecond < 0 ||
			q.Query.MaxSamples < 0 || q.Query.MaxSeries < 0 || q.Query.MaxPartitions < 0 {
			return fmt.Errorf("query limits must be >= 0")
		}
		if q.Query.MaxRange != "" {
			if _, err := ParseQuotaDuration(q.Query.MaxRange); err != nil {
				return fmt.Errorf("query.maxRange must be a positive duration, got %q", q.Query.MaxRange)
			}
		}
	}
	if q.Rules != nil {
		if q.Rules.MaxGroups < 0 || q.Rules.MaxRules < 0 || q.Rules.MaxRulesPerGroup < 0 || q.Rules.MaxEvaluationCostPerSecond < 0 {
			return fmt.Errorf("rules limits must be >= 0")
		}
	}
	if q.Storage != nil {
		if q.Storage.MaxBytes < 0 {
			return fmt.Errorf("storage.maxBytes must be >= 0")
		}
		for name, value := range map[string]string{
			"storage.retentionTraces":  q.Storage.RetentionTraces,
			"storage.retentionLogs":    q.Storage.RetentionLogs,
			"storage.retentionMetrics": q.Storage.RetentionMetrics,
		} {
			if value == "" {
				continue
			}
			if _, err := ParseQuotaDuration(value); err != nil {
				return fmt.Errorf("%s must be a positive duration, got %q", name, value)
			}
		}
	}
	return nil
}

func validateSignalIngest(name string, q *SignalIngestQuota) error {
	if q == nil {
		return nil
	}
	for field, value := range map[string]int64{
		"bytesPerSecond":   q.BytesPerSecond,
		"recordsPerSecond": q.RecordsPerSecond,
		"maxBatchBytes":    q.MaxBatchBytes,
	} {
		if value < 0 {
			return fmt.Errorf("%s.%s must be >= 0", name, field)
		}
	}
	return nil
}

// QuotaDefaults returns the bundle defaults.
func (p *Policy) QuotaDefaults() Quota {
	if p == nil || p.bundle.Quotas == nil {
		return Quota{}
	}
	return p.bundle.Quotas.Defaults
}

// KnownPartitions returns the catalog partition names carried by the bundle.
func (p *Policy) KnownPartitions() []string {
	if p == nil || p.bundle.Quotas == nil {
		return nil
	}
	return append([]string(nil), p.bundle.Quotas.KnownPartitions...)
}

// QuotaForPartition returns the effective quota for a partition, falling back
// to the bundle defaults.
func (p *Policy) QuotaForPartition(name string) Quota {
	if p == nil || p.bundle.Quotas == nil {
		return Quota{}
	}
	name = strings.TrimSpace(name)
	if quota, ok := p.bundle.Quotas.Partitions[name]; ok {
		return quota.Clone()
	}
	return p.bundle.Quotas.Defaults.Clone()
}

// SubjectQuotas returns every quota that applies to the identity. A request
// must pass all of them; groups act as shared aggregate budgets.
func (p *Policy) SubjectQuotas(id authz.Identity) []ScopedQuota {
	if p == nil || p.bundle.Quotas == nil || id.IsAnonymous() {
		return nil
	}
	out := make([]ScopedQuota, 0, 2+len(id.Groups))
	add := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		quota, ok := p.bundle.Quotas.Subjects[key]
		if !ok || quota.IsZero() {
			return
		}
		out = append(out, ScopedQuota{Key: key, Quota: quota.Clone()})
	}
	if id.Subject != "" {
		add("user:" + id.Subject)
	}
	if id.Email != "" && id.Email != id.Subject {
		add("user:" + id.Email)
	}
	for _, group := range id.Groups {
		add("group:" + group)
	}
	if id.ClientID != "" {
		add("client:" + id.ClientID)
	}
	return out
}

package policy

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// KnownBackend lists the backend IDs the coordinator understands. Unknown
// values are warnings so a new driver can be rolled out before the policy
// schema is updated everywhere.
var KnownBackends = []string{
	"omni",
	"cloudwatch_logs_insights",
	"cloudwatch_metrics",
	"bigquery",
	"clickhouse",
	"monofs_local",
}

// CoreAttributes is the reserved vocabulary. Additional keys are allowed and
// participate in selectors.
var CoreAttributes = []string{
	"role", "stack", "environment", "region", "account", "data_classification",
}

// PartitionPolicy is the platform-owned definition of one partition: its
// attributes, shards, placements, and quota.
type PartitionPolicy struct {
	Name       string            `json:"name" yaml:"name"`
	Attributes map[string]string `json:"attributes,omitempty" yaml:"attributes,omitempty"`
	Quota      *Quota            `json:"quota,omitempty" yaml:"quota,omitempty"`
	Shards     []Shard           `json:"shards,omitempty" yaml:"shards,omitempty"`
}

// Shard is one disjoint slice of a partition's data for one signal.
type Shard struct {
	Name       string              `json:"name" yaml:"name"`
	Signal     Signal              `json:"signal" yaml:"signal"`
	Platform   string              `json:"platform,omitempty" yaml:"platform,omitempty"`
	Account    string              `json:"account,omitempty" yaml:"account,omitempty"`
	Region     string              `json:"region,omitempty" yaml:"region,omitempty"`
	Project    string              `json:"project,omitempty" yaml:"project,omitempty"`
	Attributes map[string]string   `json:"attributes,omitempty" yaml:"attributes,omitempty"`
	Range      *PartitionTimeRange `json:"range,omitempty" yaml:"range,omitempty"`
	Template   string              `json:"template,omitempty" yaml:"template,omitempty"`
	Placements []Placement         `json:"placements,omitempty" yaml:"placements,omitempty"`
}

// Placement is one backend copy of a shard.
type Placement struct {
	ID        string   `json:"id" yaml:"id"`
	Backend   string   `json:"backend" yaml:"backend"`
	Source    string   `json:"source,omitempty" yaml:"source,omitempty"`
	LogGroups []string `json:"log_groups,omitempty" yaml:"log_groups,omitempty"`
	ReplicaOf string   `json:"replica_of,omitempty" yaml:"replica_of,omitempty"`
	// AssumeRole is an IAM role ARN the router assumes before querying this
	// placement. It enables cross-account Omni/Logs Insights reads.
	AssumeRole    string            `json:"assume_role,omitempty" yaml:"assume_role,omitempty"`
	Columns       map[string]string `json:"columns,omitempty" yaml:"columns,omitempty"`
	Expressions   map[string]string `json:"expressions,omitempty" yaml:"expressions,omitempty"`
	Partition     *PartitionBinding `json:"partition_binding,omitempty" yaml:"partition_binding,omitempty"`
	IndexedFields map[string]string `json:"indexed_fields,omitempty" yaml:"indexed_fields,omitempty"`
	DataSources   []string          `json:"data_sources,omitempty" yaml:"data_sources,omitempty"`
}

// PartitionBinding describes how a placement expresses the partition.
type PartitionBinding struct {
	Kind string `json:"kind" yaml:"kind"`

	Column string   `json:"column,omitempty" yaml:"column,omitempty"`
	Path   []string `json:"path,omitempty" yaml:"path,omitempty"`

	AccountField    string `json:"account_field,omitempty" yaml:"account_field,omitempty"`
	RegionField     string `json:"region_field,omitempty" yaml:"region_field,omitempty"`
	DataSourceField string `json:"data_source_field,omitempty" yaml:"data_source_field,omitempty"`
	LogGroupField   string `json:"log_group_field,omitempty" yaml:"log_group_field,omitempty"`

	Values []string `json:"values,omitempty" yaml:"values,omitempty"`
}

// PartitionTimeRange is a half-open [From, To) interval in RFC3339.
type PartitionTimeRange struct {
	From string `json:"from" yaml:"from"`
	To   string `json:"to" yaml:"to"`
}

// ShardTemplate removes repetition for common platform shapes.
type ShardTemplate struct {
	Platform   string            `json:"platform,omitempty" yaml:"platform,omitempty"`
	Signal     Signal            `json:"signal,omitempty" yaml:"signal,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty" yaml:"attributes,omitempty"`
	Placements []Placement       `json:"placements,omitempty" yaml:"placements,omitempty"`
}

// PartitionDefaults are merged before every per-partition definition.
type PartitionDefaults struct {
	Attributes     map[string]string        `json:"attributes,omitempty" yaml:"attributes,omitempty"`
	Quota          *Quota                   `json:"quota,omitempty" yaml:"quota,omitempty"`
	ShardTemplates map[string]ShardTemplate `json:"shard_templates,omitempty" yaml:"shard_templates,omitempty"`
}

// ParsePartitionPolicy parses one per-partition file.
func ParsePartitionPolicy(data []byte) (*PartitionPolicy, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var p PartitionPolicy
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("partition policy: parse: %w", err)
	}
	return &p, nil
}

// ParsePartitionDefaults parses the global defaults file. Both the documented
// nested form (defaults: {...}) and a flat form are accepted.
func ParsePartitionDefaults(data []byte) (*PartitionDefaults, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc struct {
		PartitionDefaults `yaml:",inline"`
		Defaults          *PartitionDefaults `yaml:"defaults,omitempty"`
	}
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("partition defaults: parse: %w", err)
	}
	if doc.Defaults != nil {
		return doc.Defaults, nil
	}
	return &doc.PartitionDefaults, nil
}

// Apply merges global defaults and templates into a partition definition. It
// never mutates the input.
func (d *PartitionDefaults) Apply(p *PartitionPolicy) (*PartitionPolicy, error) {
	if p == nil {
		return nil, fmt.Errorf("partition policy is nil")
	}
	out := p.clone()
	if d == nil {
		return out, nil
	}
	out.Attributes = mergeAttributes(d.Attributes, out.Attributes)
	switch {
	case d.Quota != nil && out.Quota != nil:
		q := MergeQuota(*d.Quota, *out.Quota)
		out.Quota = &q
	case d.Quota != nil:
		q := d.Quota.Clone()
		out.Quota = &q
	}
	for i := range out.Shards {
		shard := &out.Shards[i]
		if strings.TrimSpace(shard.Template) == "" {
			continue
		}
		tmpl, ok := d.ShardTemplates[shard.Template]
		if !ok {
			return nil, fmt.Errorf("partition %q shard %q: unknown template %q", out.Name, shard.Name, shard.Template)
		}
		if shard.Platform == "" {
			shard.Platform = tmpl.Platform
		}
		if shard.Signal == "" {
			shard.Signal = tmpl.Signal
		}
		shard.Attributes = mergeAttributes(tmpl.Attributes, shard.Attributes)
		if len(shard.Placements) == 0 {
			shard.Placements = clonePlacements(tmpl.Placements)
		}
		shard.Template = ""
	}
	return out, nil
}

// ValidatePartitions validates a set of definitions, including duplicate names.
func ValidatePartitions(partitions []PartitionPolicy) []Issue {
	var issues []Issue
	seen := map[string]bool{}
	for _, p := range partitions {
		if seen[p.Name] {
			issues = append(issues, Issue{Severity: "error", Message: fmt.Sprintf("partition %q is defined more than once", p.Name)})
		}
		seen[p.Name] = true
		issues = append(issues, p.Validate()...)
	}
	return issues
}

// Validate checks one partition definition.
func (p PartitionPolicy) Validate() []Issue {
	var issues []Issue
	add := func(severity, format string, args ...any) {
		issues = append(issues, Issue{Severity: severity, Message: fmt.Sprintf(format, args...)})
	}
	if strings.TrimSpace(p.Name) == "" {
		add("error", "partition name is required")
	}
	if len(p.Shards) == 0 {
		add("warning", "partition %q has no shards", p.Name)
	}
	seenShards := map[string]bool{}
	for _, shard := range p.Shards {
		label := fmt.Sprintf("partition %q shard %q", p.Name, shard.Name)
		if strings.TrimSpace(shard.Name) == "" {
			add("error", "%s: name is required", label)
		} else if seenShards[shard.Name] {
			add("error", "%s: duplicate shard name", label)
		}
		seenShards[shard.Name] = true
		switch shard.Signal {
		case SignalLogs, SignalTraces, SignalMetrics:
		default:
			add("error", "%s: invalid signal %q", label, shard.Signal)
		}
		if strings.TrimSpace(shard.Platform) == "" {
			add("error", "%s: platform is required", label)
		}
		if shard.Range != nil {
			from, to, err := shard.Range.parse()
			if err != nil {
				add("error", "%s: %v", label, err)
			} else if !to.After(from) {
				add("error", "%s: range.to must be after range.from", label)
			}
		}
		if len(shard.Placements) == 0 {
			add("error", "%s: at least one placement is required", label)
			continue
		}
		seenPlacements := map[string]bool{}
		primary := ""
		for _, placement := range shard.Placements {
			pid := placement.ID
			if strings.TrimSpace(pid) == "" {
				add("error", "%s: placement id is required", label)
			} else if seenPlacements[pid] {
				add("error", "%s: duplicate placement id %q", label, pid)
			}
			seenPlacements[pid] = true
			if strings.TrimSpace(placement.Backend) == "" {
				add("error", "%s placement %q: backend is required", label, pid)
			} else if !knownBackend(placement.Backend) {
				add("warning", "%s placement %q: unknown backend %q", label, pid, placement.Backend)
			}
			if placement.ReplicaOf == "" {
				if primary != "" {
					add("error", "%s: multiple non-replica placements (%q and %q)", label, primary, pid)
				} else {
					primary = pid
				}
			}
			if placement.Partition == nil {
				add("warning", "%s placement %q: no partition binding; restricted queries will be refused", label, pid)
			} else {
				for _, issue := range validatePartitionBinding(placement) {
					issue.Message = fmt.Sprintf("%s placement %q: %s", label, pid, issue.Message)
					issues = append(issues, issue)
				}
			}
			if placement.Backend == "cloudwatch_logs_insights" && len(placement.LogGroups) > 50 {
				add("warning", "%s placement %q: %d log groups; the documented maximum is 50", label, pid, len(placement.LogGroups))
			}
		}
		if primary == "" {
			add("error", "%s: at least one primary (non-replica) placement is required", label)
		}
		for _, placement := range shard.Placements {
			if placement.ReplicaOf == "" {
				continue
			}
			if placement.ReplicaOf == placement.ID {
				add("error", "%s placement %q: replica_of cannot reference itself", label, placement.ID)
				continue
			}
			found := false
			for _, other := range shard.Placements {
				if other.ID == placement.ReplicaOf && other.ReplicaOf == "" {
					found = true
					break
				}
			}
			if !found {
				add("error", "%s placement %q: replica_of %q is not a primary placement in this shard", label, placement.ID, placement.ReplicaOf)
			}
		}
	}

	return issues
}

func validatePartitionBinding(placement Placement) []Issue {
	b := placement.Partition
	var issues []Issue
	add := func(format string, args ...any) {
		issues = append(issues, Issue{Severity: "error", Message: fmt.Sprintf(format, args...)})
	}
	switch b.Kind {
	case "column":
		if strings.TrimSpace(b.Column) == "" {
			add("partition binding column is required for kind=column")
		}
	case "attribute":
		if len(b.Path) == 0 {
			add("partition binding path is required for kind=attribute")
		}
	case "account_region":
		if b.AccountField == "" && b.RegionField == "" {
			add("partition binding account_field or region_field is required for kind=account_region")
		}
	case "data_source":
		if b.DataSourceField == "" {
			add("partition binding data_source_field is required for kind=data_source")
		}
	case "log_group":
		if b.LogGroupField == "" && len(placement.LogGroups) == 0 {
			add("partition binding log_group_field or placement log_groups is required for kind=log_group")
		}
	case "fixed":
		if len(b.Values) == 0 {
			add("partition binding values are required for kind=fixed")
		}
	default:
		add("unknown partition binding kind %q", b.Kind)
	}
	return issues
}

func knownBackend(name string) bool {
	for _, b := range KnownBackends {
		if b == name {
			return true
		}
	}
	return false
}

func (r *PartitionTimeRange) parse() (time.Time, time.Time, error) {
	from, err := time.Parse(time.RFC3339, r.From)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("range.from: %w", err)
	}
	to, err := time.Parse(time.RFC3339, r.To)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("range.to: %w", err)
	}
	return from.UTC(), to.UTC(), nil
}

func (p *PartitionPolicy) clone() *PartitionPolicy {
	if p == nil {
		return nil
	}
	out := *p
	out.Attributes = cloneMap(p.Attributes)
	if p.Quota != nil {
		q := p.Quota.Clone()
		out.Quota = &q
	}
	out.Shards = make([]Shard, len(p.Shards))
	for i, shard := range p.Shards {
		out.Shards[i] = shard.clone()
	}
	return &out
}

func (s Shard) clone() Shard {
	out := s
	out.Attributes = cloneMap(s.Attributes)
	if s.Range != nil {
		r := *s.Range
		out.Range = &r
	}
	out.Placements = clonePlacements(s.Placements)
	return out
}

func clonePlacements(in []Placement) []Placement {
	out := make([]Placement, len(in))
	for i, placement := range in {
		out[i] = placement
		out[i].LogGroups = append([]string(nil), placement.LogGroups...)
		out[i].Columns = cloneMap(placement.Columns)
		out[i].Expressions = cloneMap(placement.Expressions)
		out[i].IndexedFields = cloneMap(placement.IndexedFields)
		out[i].DataSources = append([]string(nil), placement.DataSources...)
		if placement.Partition != nil {
			b := *placement.Partition
			b.Path = append([]string(nil), placement.Partition.Path...)
			b.Values = append([]string(nil), placement.Partition.Values...)
			out[i].Partition = &b
		}
	}
	return out
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mergeAttributes(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

// Partitions returns the compiled partition definitions.
func (p *Policy) Partitions() []PartitionPolicy {
	if p == nil {
		return nil
	}
	return append([]PartitionPolicy(nil), p.bundle.Partitions...)
}

// Partition looks up one compiled definition.
func (p *Policy) Partition(name string) (PartitionPolicy, bool) {
	for _, partition := range p.Partitions() {
		if partition.Name == name {
			return partition, true
		}
	}
	return PartitionPolicy{}, false
}

// SortedPartitionNames returns the compiled partition names in order.
func (p *Policy) SortedPartitionNames() []string {
	names := make([]string, 0)
	for _, partition := range p.Partitions() {
		names = append(names, partition.Name)
	}
	sort.Strings(names)
	return names
}

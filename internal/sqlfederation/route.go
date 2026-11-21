package sqlfederation

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
	"github.com/radryc/sqlagregator/pkg/sqlag"
)

// BackendResolver supplies the executor for one selected placement. The router
// owns the driver configuration; the planner only decides what to run.
type BackendResolver interface {
	Resolve(ctx context.Context, routed RoutedEndpoint) (sqlag.Backend, error)
}

// RouteRequest selects and resolves partitions for one query.
type RouteRequest struct {
	Dataset string
	// Visible partitions from the access policy. Empty means all compiled
	// partitions, which is only appropriate for an admin caller.
	Visible []string
	// AllowedBackends is an optional request-level allowlist. It is
	// intersected with the policy grant.
	AllowedBackends []string
}

// RoutedEndpoint is one selected placement with its shard context.
type RoutedEndpoint struct {
	Endpoint  *sqlag.Endpoint
	Partition policy.PartitionPolicy
	Shard     policy.Shard
	Placement policy.Placement
}

// RoutePlan is the result of shard resolution.
type RoutePlan struct {
	Endpoints     []RoutedEndpoint
	Partitions    []string
	Coverage      map[string]*sqlag.CoverageGroup
	MissingShards []string
	Warnings      []string
}

// Route resolves visible partitions into in-scope shards and one primary
// placement per shard. Out-of-scope shards are not read and do not count as
// coverage gaps.
func Route(pol *policy.Policy, id authz.Identity, req RouteRequest) (*RoutePlan, error) {
	if pol == nil {
		return nil, fmt.Errorf("routing: nil policy")
	}
	signal, err := datasetSignal(req.Dataset)
	if err != nil {
		return nil, err
	}
	visible := append([]string(nil), req.Visible...)
	if len(visible) == 0 {
		visible = pol.SortedPartitionNames()
	}
	plan := &RoutePlan{
		Partitions: append([]string(nil), visible...),
		Coverage:   map[string]*sqlag.CoverageGroup{},
	}
	if len(visible) == 0 {
		return plan, fmt.Errorf("routing: no visible partitions")
	}

	for _, name := range visible {
		partition, ok := pol.Partition(name)
		if !ok {
			return nil, fmt.Errorf("routing: partition %q is not defined", name)
		}
		policyBackends, granted := pol.AllowedBackends(id, name)
		if !granted {
			continue
		}
		allowed := intersectBackends(policyBackends, req.AllowedBackends)

		groupName := name + "/" + string(signal)
		var (
			inScope  int
			selected int
		)
		for _, shard := range partition.Shards {
			if shard.Signal != signal {
				continue
			}
			attrs := mergeAttrs(partition.Attributes, shard.Attributes)
			if !pol.ShardAllowed(id, name, attrs) {
				continue
			}
			inScope++
			placement, warning, ok := choosePlacement(shard, allowed)
			if warning != "" {
				plan.Warnings = append(plan.Warnings, warning)
			}
			if !ok {
				plan.MissingShards = append(plan.MissingShards, name+"/"+shard.Name)
				continue
			}
			selected++
			plan.Endpoints = append(plan.Endpoints, RoutedEndpoint{
				Endpoint:  buildEndpoint(req.Dataset, partition, shard, placement, attrs),
				Partition: partition,
				Shard:     shard,
				Placement: placement,
			})
		}
		if inScope == 0 {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("partition %q has no in-scope %s shards", name, signal))
			continue
		}
		plan.Coverage[groupName] = &sqlag.CoverageGroup{
			Mode:         "hash",
			Members:      inScope,
			AllowMissing: true,
		}
		if selected < inScope {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"partition %q: %d of %d %s shards have an allowed placement",
				name, selected, inScope, signal))
		}
	}
	sort.Slice(plan.Endpoints, func(i, j int) bool {
		return plan.Endpoints[i].Endpoint.ID < plan.Endpoints[j].Endpoint.ID
	})
	sort.Strings(plan.MissingShards)
	return plan, nil
}

// Catalog builds a coordinator catalog for a route plan.
func (plan *RoutePlan) Catalog(dataset string) (*sqlag.Catalog, error) {
	if plan == nil {
		return nil, fmt.Errorf("routing: nil plan")
	}
	defs, ok := datasetSchemas[dataset]
	if !ok {
		return nil, fmt.Errorf("routing: unsupported dataset %q", dataset)
	}
	columns := make([]sqlag.Column, len(defs))
	for i, def := range defs {
		columns[i] = sqlag.Column{Name: def.name, Type: def.typ}
	}
	table := &sqlag.Table{
		Name:       dataset,
		Signal:     dataset,
		TimeColumn: "timestamp",
		Columns:    columns,
	}
	endpoints := make([]*sqlag.Endpoint, 0, len(plan.Endpoints))
	backends := map[string]*sqlag.BackendDefaults{}
	for _, routed := range plan.Endpoints {
		endpoints = append(endpoints, routed.Endpoint)
		backends[routed.Endpoint.Backend] = &sqlag.BackendDefaults{
			Signals:                []string{dataset},
			EnforcesPartitionScope: true,
		}
	}
	cat, err := sqlag.NewCatalog(
		map[string]*sqlag.Table{dataset: table},
		endpoints,
		plan.Coverage,
		backends,
		sqlag.Defaults{},
	)
	if err != nil {
		return nil, err
	}
	return cat, nil
}

func buildEndpoint(dataset string, partition policy.PartitionPolicy, shard policy.Shard, placement policy.Placement, attrs map[string]string) *sqlag.Endpoint {
	id := strings.Join([]string{partition.Name, string(shard.Signal), shard.Name, placement.ID}, "/")
	tags := mergeAttrs(attrs, map[string]string{
		"partition": partition.Name,
		"signal":    string(shard.Signal),
		"shard":     shard.Name,
		"backend":   placement.Backend,
		"platform":  shard.Platform,
	})
	return &sqlag.Endpoint{
		ID:            id,
		Backend:       placement.Backend,
		Tables:        []string{dataset},
		Role:          sqlag.RoleShard,
		CoverageGroup: partition.Name + "/" + string(shard.Signal),
		Source:        placementSource(placement),
		Tags:          tags,
		Columns:       endpointColumns(dataset, placement),
		Partition:     convertBinding(placement.Partition),
		Binding:       buildBindingValue(partition, shard, placement, attrs),
		Caps: sqlag.Capabilities{
			Signals:                []string{dataset},
			EnforcesPartitionScope: true,
		},
	}
}

// endpointColumns merges the placement's physical mapping with the MonoFS
// local view's column names. The local log view exposes `time` (timestamp) and
// `timestamp` (int64 nanos), so a generic `timestamp` predicate must target
// `time`.
func endpointColumns(dataset string, placement policy.Placement) map[string]string {
	if !strings.EqualFold(placement.Backend, "monofs_local") {
		return placement.Columns
	}
	merged := map[string]string{}
	for _, def := range datasetSchemas[dataset] {
		if def.physical != "" {
			merged[def.name] = def.physical
		}
	}
	for logical, physical := range placement.Columns {
		merged[logical] = physical
	}
	return merged
}

// placementSource returns the FROM source for a placement. Logs Insights and
// Omni can derive it from the placement's log groups when no explicit source
// is set.
func placementSource(placement policy.Placement) string {
	source := strings.TrimSpace(placement.Source)
	if source != "" {
		return source
	}
	if strings.EqualFold(placement.Backend, "cloudwatch_logs_insights") && len(placement.LogGroups) > 0 {
		quoted := make([]string, 0, len(placement.LogGroups))
		for _, group := range placement.LogGroups {
			quoted = append(quoted, "'"+strings.ReplaceAll(group, "'", "''")+"'")
		}
		return "logGroups(logGroupIdentifier: [" + strings.Join(quoted, ", ") + "])"
	}
	return ""
}

func buildBindingValue(partition policy.PartitionPolicy, shard policy.Shard, placement policy.Placement, attrs map[string]string) *sqlag.PartitionBindingValue {
	account := shard.Account
	if account == "" {
		account = attrs["account"]
	}
	region := shard.Region
	if region == "" {
		region = attrs["region"]
	}
	return &sqlag.PartitionBindingValue{
		Partition:  partition.Name,
		Attributes: attrs,
		Account:    account,
		Region:     region,
		DataSource: attrs["data_source"],
		LogGroup:   firstString(placement.LogGroups),
		LogGroups:  append([]string(nil), placement.LogGroups...),
	}
}

func convertBinding(binding *policy.PartitionBinding) *sqlag.PartitionBinding {
	if binding == nil {
		return nil
	}
	return &sqlag.PartitionBinding{
		Kind:            binding.Kind,
		Column:          binding.Column,
		Path:            append([]string(nil), binding.Path...),
		AccountField:    binding.AccountField,
		RegionField:     binding.RegionField,
		DataSourceField: binding.DataSourceField,
		LogGroupField:   binding.LogGroupField,
		Values:          append([]string(nil), binding.Values...),
	}
}

func datasetSignal(dataset string) (policy.Signal, error) {
	switch strings.ToLower(strings.TrimSpace(dataset)) {
	case "logs":
		return policy.SignalLogs, nil
	case "traces":
		return policy.SignalTraces, nil
	case "metrics":
		return policy.SignalMetrics, nil
	default:
		return "", fmt.Errorf("routing: unsupported dataset %q", dataset)
	}
}

func choosePlacement(shard policy.Shard, allowed []string) (policy.Placement, string, bool) {
	var primary, replica *policy.Placement
	for i := range shard.Placements {
		placement := &shard.Placements[i]
		if !backendAllowed(allowed, placement.Backend) {
			continue
		}
		if placement.ReplicaOf == "" {
			if primary != nil {
				return policy.Placement{}, "", false
			}
			primary = placement
			continue
		}
		if replica == nil {
			replica = placement
		}
	}
	if primary != nil {
		return *primary, "", true
	}
	if replica != nil {
		return *replica, fmt.Sprintf("shard %q: primary backend is not allowed; using replica %q", shard.Name, replica.ID), true
	}
	return policy.Placement{}, "", false
}

func backendAllowed(allowed []string, backend string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, item := range allowed {
		if item == backend {
			return true
		}
	}
	return false
}

func intersectBackends(policyAllowed, requested []string) []string {
	switch {
	case len(policyAllowed) == 0:
		return append([]string(nil), requested...)
	case len(requested) == 0:
		return append([]string(nil), policyAllowed...)
	default:
		var out []string
		for _, backend := range policyAllowed {
			for _, req := range requested {
				if backend == req {
					out = append(out, backend)
					break
				}
			}
		}
		return out
	}
}

func mergeAttrs(base, override map[string]string) map[string]string {
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

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

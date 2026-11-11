package logengine

import "context"

// QueryScope carries the caller's authorized telemetry partitions into the
// storage layer. A zero scope (Restricted=false) means "all partitions".
//
// Metrics are always visible under the Strata policy and are not filtered by
// this scope. Logs and traces are filtered per record so legacy chunks without
// a partition column cannot leak data when a caller is restricted.
type QueryScope struct {
	Partitions []string
	Restricted bool
}

type queryScopeKey struct{}

// WithQueryScope attaches a query scope to ctx.
func WithQueryScope(ctx context.Context, scope QueryScope) context.Context {
	return context.WithValue(ctx, queryScopeKey{}, scope)
}

// QueryScopeFromContext returns the scope attached to ctx. The second result
// reports whether a scope was present at all.
func QueryScopeFromContext(ctx context.Context) (QueryScope, bool) {
	scope, ok := ctx.Value(queryScopeKey{}).(QueryScope)
	return scope, ok
}

// AllowsPartition reports whether the scope permits a single partition value.
func (s QueryScope) AllowsPartition(partition string) bool {
	if !s.Restricted {
		return true
	}
	if partition == "" {
		return false
	}
	for _, allowed := range s.Partitions {
		if allowed == partition {
			return true
		}
	}
	return false
}

func (s QueryScope) allowsLog(record LogRecord) bool {
	if !s.Restricted {
		return true
	}
	return s.AllowsPartition(recordPartition(record.Labels))
}

func (s QueryScope) allowsSpan(record SpanRecord) bool {
	if !s.Restricted {
		return true
	}
	return s.AllowsPartition(recordPartition(record.Attributes))
}

// AllowsPartitionLabels reports whether the scope permits the partition label
// carried by a log labels or span attributes map. Backends that push the scope
// into their query language still use this as a per-row guard.
func (s QueryScope) AllowsPartitionLabels(labels map[string]string) bool {
	return s.AllowsPartition(recordPartition(labels))
}

// manifestMatchesScope prunes chunks when the manifest carries a complete
// partition dictionary. Legacy manifests without partitions are kept and
// filtered row-by-row.
func manifestMatchesScope(manifest ChunkManifest, scope QueryScope) bool {
	if !scope.Restricted || len(manifest.Partitions) == 0 {
		return true
	}
	for _, partition := range manifest.Partitions {
		if scope.AllowsPartition(partition) {
			return true
		}
	}
	return false
}

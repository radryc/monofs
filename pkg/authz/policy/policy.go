// Package policy implements the Strata access policy: a repo-managed ACL
// (access.yaml) compiled into a versioned bundle that Guardian, MonoFS and
// Doctor consume to decide who can see which partitions, MonoFS paths, logs
// and traces. Metrics are always visible and are not configurable.
package policy

import (
	"sort"
	"strings"

	"github.com/radryc/monofs/pkg/authz"
)

// Scope names used by the ACL and the compiled bundle.
const (
	ScopePartitions = "partitions"
	ScopeLogs       = "logs"
	ScopeTraces     = "traces"
	ScopeMonoFS     = "monofs"
)

// AllScopes lists the configurable scopes (metrics deliberately excluded).
var AllScopes = []string{ScopePartitions, ScopeLogs, ScopeTraces, ScopeMonoFS}

// Signal is a telemetry signal for read decisions.
type Signal string

const (
	SignalMetrics Signal = "metrics"
	SignalLogs    Signal = "logs"
	SignalTraces  Signal = "traces"
)

// Defaults captures the fallback decision per scope for subjects without a
// matching grant. Metrics are always allowed and have no default.
type Defaults struct {
	Partitions bool `json:"partitions"`
	Logs       bool `json:"logs"`
	Traces     bool `json:"traces"`
	MonoFS     bool `json:"monofs"`
}

// allows reports the default decision for a scope name.
func (d Defaults) allows(scope string) bool {
	switch scope {
	case ScopePartitions:
		return d.Partitions
	case ScopeLogs:
		return d.Logs
	case ScopeTraces:
		return d.Traces
	case ScopeMonoFS:
		return d.MonoFS
	}
	return false
}

// AWSBinding maps an AWS account/region to a Guardian partition so federated
// CloudWatch/X-Ray records can be attributed to partitions.
type AWSBinding struct {
	Partition string `json:"partition"`
	Account   string `json:"account,omitempty"`
	Region    string `json:"region,omitempty"`
}

// Grant is one compiled subject entry. Exactly one of Group/User/Client is set.
// Scope lists contain concrete partition names (or "*"); MonoFS entries are
// display-path patterns. Deny lists scope names denied wholesale.
type Grant struct {
	Group      string   `json:"group,omitempty"`
	User       string   `json:"user,omitempty"`
	Client     string   `json:"client,omitempty"`
	Partitions []string `json:"partitions,omitempty"`
	Logs       []string `json:"logs,omitempty"`
	Traces     []string `json:"traces,omitempty"`
	MonoFS     []string `json:"monofs,omitempty"`
	Role       string   `json:"role,omitempty"`
	Deny       []string `json:"deny,omitempty"`
}

// Bundle is the compiled, versioned policy consumed by services.
type Bundle struct {
	FormatVersion int          `json:"format_version"`
	Version       string       `json:"version"`
	GeneratedAt   string       `json:"generated_at,omitempty"`
	Defaults      Defaults     `json:"defaults"`
	Grants        []Grant      `json:"grants"`
	AWSBindings   []AWSBinding `json:"aws_bindings,omitempty"`
	CatalogDigest string       `json:"catalog_digest,omitempty"`
}

// Policy is a parsed bundle with evaluation helpers. It is immutable.
type Policy struct {
	bundle Bundle
}

// New wraps a bundle.
func New(bundle Bundle) *Policy { return &Policy{bundle: bundle} }

// Bundle returns the underlying bundle.
func (p *Policy) Bundle() Bundle { return p.bundle }

// Version returns the bundle version (content hash).
func (p *Policy) Version() string {
	if p == nil {
		return ""
	}
	return p.bundle.Version
}

// Defaults returns the compiled defaults.
func (p *Policy) Defaults() Defaults {
	if p == nil {
		return Defaults{}
	}
	return p.bundle.Defaults
}

// AWSBindings returns the compiled account/region bindings.
func (p *Policy) AWSBindings() []AWSBinding {
	if p == nil {
		return nil
	}
	return append([]AWSBinding(nil), p.bundle.AWSBindings...)
}

// PartitionForAWS resolves a federated AWS record's account/region to a
// partition name. It returns ok=false when no binding matches; an empty
// account and region never matches (that would wildcard every binding).
func (p *Policy) PartitionForAWS(account, region string) (string, bool) {
	if p == nil {
		return "", false
	}
	account = strings.TrimSpace(account)
	region = strings.TrimSpace(region)
	if account == "" && region == "" {
		return "", false
	}
	for _, b := range p.bundle.AWSBindings {
		if b.Account != "" && account != "" && b.Account != account {
			continue
		}
		if b.Region != "" && region != "" && b.Region != region {
			continue
		}
		if b.Partition != "" {
			return b.Partition, true
		}
	}
	return "", false
}

// IsAdmin reports whether the identity holds the admin role (or is listed in
// a grant with Role=admin), which bypasses all filtering.
func (p *Policy) IsAdmin(id authz.Identity) bool {
	if p == nil || id.IsAnonymous() {
		return false
	}
	for _, g := range p.bundle.Grants {
		if g.Role == string(authz.RoleAdmin) && g.matches(id) {
			return true
		}
	}
	return false
}

// CanSeePartition reports whether the identity may see the partition.
func (p *Policy) CanSeePartition(id authz.Identity, partition string) bool {
	return p.can(id, ScopePartitions, partition)
}

// CanReadSignal reports whether the identity may read a telemetry signal for a
// partition. Metrics are always visible.
func (p *Policy) CanReadSignal(id authz.Identity, partition string, signal Signal) bool {
	switch signal {
	case SignalMetrics:
		return true
	case SignalLogs:
		return p.can(id, ScopeLogs, partition)
	case SignalTraces:
		return p.can(id, ScopeTraces, partition)
	}
	return false
}

// CanReadPath reports whether the identity may read a MonoFS display path.
func (p *Policy) CanReadPath(id authz.Identity, displayPath string) bool {
	if p == nil || id.IsAnonymous() {
		return false
	}
	if p.IsAdmin(id) {
		return true
	}
	if p.denied(id, ScopeMonoFS) {
		return false
	}
	for _, g := range p.matching(id) {
		if pathAllowed(g.MonoFS, displayPath) {
			return true
		}
	}
	return p.bundle.Defaults.MonoFS
}

// Role returns the highest role the identity holds on the partition ("" when
// none). Operate actions (ingest/modify) are derived from it via authz.Role.
func (p *Policy) Role(id authz.Identity, partition string) authz.Role {
	if p == nil || id.IsAnonymous() {
		return ""
	}
	best := authz.Role("")
	for _, g := range p.matching(id) {
		if g.Role == "" || g.deniesScope(ScopePartitions) {
			continue
		}
		if !scopeAllows(g.Partitions, partition) {
			continue
		}
		if roleRank(authz.Role(g.Role)) > roleRank(best) {
			best = authz.Role(g.Role)
		}
	}
	return best
}

// VisiblePartitions returns the partition names the identity can see. When the
// identity's grants include a wildcard the second return is true and callers
// should treat the catalog as fully visible.
func (p *Policy) VisiblePartitions(id authz.Identity) ([]string, bool) {
	if p == nil || id.IsAnonymous() {
		return nil, false
	}
	if p.IsAdmin(id) {
		return nil, true
	}
	if p.denied(id, ScopePartitions) {
		return nil, false
	}
	seen := map[string]bool{}
	for _, g := range p.matching(id) {
		for _, name := range g.Partitions {
			if name == "*" {
				return nil, true
			}
			seen[name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, false
}

// ResolveVisiblePartitions expands wildcard visibility against a catalog.
func (p *Policy) ResolveVisiblePartitions(id authz.Identity, catalog []string) []string {
	names, all := p.VisiblePartitions(id)
	if all {
		return append([]string(nil), catalog...)
	}
	return names
}

// Explain describes why an identity has the access it has.
type Explanation struct {
	Admin    bool     `json:"admin"`
	Matches  []Grant  `json:"matches"`
	Defaults Defaults `json:"defaults"`
}

// Explain returns the grants matching the identity plus the defaults.
func (p *Policy) Explain(id authz.Identity) Explanation {
	exp := Explanation{Admin: p.IsAdmin(id), Defaults: p.Defaults()}
	if p == nil {
		return exp
	}
	exp.Matches = p.matching(id)
	return exp
}

func (p *Policy) can(id authz.Identity, scope, partition string) bool {
	if p == nil || id.IsAnonymous() {
		return false
	}
	if p.IsAdmin(id) {
		return true
	}
	if p.denied(id, scope) {
		return false
	}
	for _, g := range p.matching(id) {
		if g.deniesScope(scope) {
			continue
		}
		var list []string
		switch scope {
		case ScopePartitions:
			list = g.Partitions
		case ScopeLogs:
			list = g.Logs
		case ScopeTraces:
			list = g.Traces
		}
		if scopeAllows(list, partition) {
			return true
		}
	}
	return p.bundle.Defaults.allows(scope)
}

// denied reports whether any matching grant denies the scope wholesale.
func (p *Policy) denied(id authz.Identity, scope string) bool {
	for _, g := range p.matching(id) {
		if g.deniesScope(scope) {
			return true
		}
	}
	return false
}

func (g Grant) deniesScope(scope string) bool {
	for _, d := range g.Deny {
		if d == scope {
			return true
		}
	}
	return false
}

// matching returns all grants whose identity matcher applies to id.
func (p *Policy) matching(id authz.Identity) []Grant {
	var out []Grant
	for _, g := range p.bundle.Grants {
		if g.matches(id) {
			out = append(out, g)
		}
	}
	return out
}

func (g Grant) matches(id authz.Identity) bool {
	if id.IsAnonymous() {
		return false
	}
	switch {
	case g.Client != "":
		return id.ClientID != "" && g.Client == id.ClientID
	case g.User != "":
		if id.Subject != "" && g.User == id.Subject {
			return true
		}
		if id.Email != "" && strings.EqualFold(g.User, id.Email) {
			return true
		}
		return false
	case g.Group != "":
		return id.HasGroup(g.Group)
	}
	return false
}

func scopeAllows(list []string, name string) bool {
	for _, item := range list {
		if item == "*" || item == name {
			return true
		}
	}
	return false
}

func roleRank(r authz.Role) int {
	switch r {
	case authz.RoleViewer:
		return 1
	case authz.RoleIngester:
		return 2
	case authz.RoleMaintainer:
		return 3
	case authz.RoleAdmin:
		return 4
	}
	return 0
}

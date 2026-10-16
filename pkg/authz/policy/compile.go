package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/radryc/monofs/pkg/authz"
)

// Partition is one catalog entry: a Guardian partition with its labels.
type Partition struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Catalog is the partition catalog selectors are resolved against.
type Catalog struct {
	Partitions []Partition `json:"partitions"`
}

// Issue is a lint finding.
type Issue struct {
	Severity string `json:"severity"` // error | warning | info
	Message  string `json:"message"`
}

func hasErrors(issues []Issue) bool {
	for _, issue := range issues {
		if issue.Severity == "error" {
			return true
		}
	}
	return false
}

// Compile validates a raw policy and resolves it against the catalog into a
// versioned bundle. Structural problems are returned as an error; lint
// findings (warnings/infos) are returned alongside the bundle.
func Compile(file *File, catalog *Catalog, now time.Time) (*Bundle, []Issue, error) {
	if file == nil {
		return nil, nil, fmt.Errorf("policy: no policy file")
	}
	issues := validate(file, catalog)
	if hasErrors(issues) {
		return nil, issues, fmt.Errorf("policy: validation failed with %d error(s)", countErrors(issues))
	}
	bundle, err := build(file, catalog, now)
	if err != nil {
		return nil, issues, err
	}
	return bundle, issues, nil
}

// Lint validates a raw policy and returns all findings, including structural
// errors, without producing a bundle.
func Lint(file *File, catalog *Catalog) []Issue {
	_, issues, err := Compile(file, catalog, time.Now())
	if err != nil && !hasErrors(issues) {
		issues = append(issues, Issue{Severity: "error", Message: err.Error()})
	}
	return issues
}

func countErrors(issues []Issue) int {
	n := 0
	for _, issue := range issues {
		if issue.Severity == "error" {
			n++
		}
	}
	return n
}

type compileCtx struct {
	file    *File
	catalog *Catalog
	byName  map[string]Partition
	issues  []Issue
}

func validate(file *File, catalog *Catalog) []Issue {
	ctx := &compileCtx{file: file, catalog: catalog, byName: map[string]Partition{}}
	if catalog != nil {
		for _, p := range catalog.Partitions {
			ctx.byName[p.Name] = p
		}
	}

	for scope, value := range file.Defaults {
		if err := validateScope(scope); err != nil {
			ctx.errorf("%v", err)
			continue
		}
		if value != "allow" && value != "deny" {
			ctx.errorf("defaults.%s must be \"allow\" or \"deny\", got %q", scope, value)
		}
	}

	ctx.validateSubjects("group", file.Groups)
	ctx.validateSubjects("user", file.Users)
	ctx.validateSubjects("client", file.Clients)

	for _, b := range file.AWSBindings {
		if strings.TrimSpace(b.Partition) == "" {
			ctx.errorf("aws_bindings: partition is required")
			continue
		}
		if b.Account == "" && b.Region == "" {
			ctx.errorf("aws_bindings: %s needs an account and/or region", b.Partition)
		}
		if catalog != nil {
			if _, ok := ctx.byName[b.Partition]; !ok {
				ctx.warnf("aws_bindings: partition %q is not in the catalog", b.Partition)
			}
		}
	}
	return ctx.issues
}

func (c *compileCtx) validateSubjects(kind string, subjects map[string]Subject) {
	for name, sub := range subjects {
		label := fmt.Sprintf("%s %q", kind, name)
		if sub.Role != "" {
			if sub.Role == string(authz.RoleAdmin) {
				c.infof("%s has the admin role and sees every partition", label)
			} else if !authz.Role(sub.Role).Valid() {
				c.errorf("%s has invalid role %q (viewer, ingester, maintainer or admin)", label, sub.Role)
			}
		}
		seen := map[string]bool{}
		for _, scope := range sub.Deny {
			if err := validateScope(scope); err != nil {
				c.errorf("%s deny: %v", label, err)
			}
			seen[scope] = true
		}
		allowed := map[string]bool{}
		for _, scope := range []struct {
			name string
			list []string
		}{
			{ScopePartitions, sub.Partitions},
			{ScopeLogs, sub.Logs},
			{ScopeTraces, sub.Traces},
		} {
			for _, entry := range scope.list {
				allowed[scope.name] = true
				c.checkEntry(label, scope.name, entry)
			}
		}
		for scope := range seen {
			if !allowed[scope] {
				c.warnf("%s denies scope %q but does not grant it anywhere", label, scope)
			}
		}
		if len(sub.Partitions)+len(sub.Logs)+len(sub.Traces)+len(sub.MonoFS) == 0 && sub.Role == "" && len(sub.Deny) == 0 {
			c.warnf("%s has no scopes and no role", label)
		}
	}
}

// checkEntry warns about selector refs/globs that resolve to nothing and
// partition names missing from the catalog.
func (c *compileCtx) checkEntry(label, scope, entry string) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		c.errorf("%s %s: empty entry", label, scope)
		return
	}
	if entry == "*" {
		return
	}
	if _, isSelector := c.file.Selectors[entry]; isSelector {
		if c.catalog != nil && len(c.matchSelector(c.file.Selectors[entry])) == 0 {
			c.warnf("%s %s: selector %q matches no partition", label, scope, entry)
		}
		return
	}
	if strings.Contains(entry, "*") {
		if c.catalog != nil && len(c.expandGlob(entry)) == 0 {
			c.warnf("%s %s: glob %q matches no partition", label, scope, entry)
		}
		return
	}
	if c.catalog != nil {
		if _, ok := c.byName[entry]; !ok {
			c.warnf("%s %s: partition %q is not in the catalog", label, scope, entry)
		}
	}
}

func (c *compileCtx) matchSelector(sel Selector) []string {
	var out []string
	for _, p := range c.catalog.Partitions {
		matches := true
		for k, v := range sel.Labels {
			if p.Labels[k] != v {
				matches = false
				break
			}
		}
		if matches {
			out = append(out, p.Name)
		}
	}
	sort.Strings(out)
	return out
}

func (c *compileCtx) expandGlob(pattern string) []string {
	var out []string
	for _, p := range c.catalog.Partitions {
		if ok, err := path.Match(pattern, p.Name); err == nil && ok {
			out = append(out, p.Name)
		}
	}
	sort.Strings(out)
	return out
}

func (c *compileCtx) errorf(format string, args ...any) {
	c.issues = append(c.issues, Issue{Severity: "error", Message: fmt.Sprintf(format, args...)})
}

func (c *compileCtx) warnf(format string, args ...any) {
	c.issues = append(c.issues, Issue{Severity: "warning", Message: fmt.Sprintf(format, args...)})
}

func (c *compileCtx) infof(format string, args ...any) {
	c.issues = append(c.issues, Issue{Severity: "info", Message: fmt.Sprintf(format, args...)})
}

func build(file *File, catalog *Catalog, now time.Time) (*Bundle, error) {
	defaults := Defaults{}
	for scope, value := range file.Defaults {
		allow := value == "allow"
		switch scope {
		case ScopePartitions:
			defaults.Partitions = allow
		case ScopeLogs:
			defaults.Logs = allow
		case ScopeTraces:
			defaults.Traces = allow
		case ScopeMonoFS:
			defaults.MonoFS = allow
		}
	}

	bundle := &Bundle{
		FormatVersion: FormatVersion,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Defaults:      defaults,
	}

	resolve := func(sub Subject) (partitions, logs, traces, monoFS []string) {
		partitions = resolveScope(file, catalog, sub.Partitions)
		logs = resolveScope(file, catalog, sub.Logs)
		traces = resolveScope(file, catalog, sub.Traces)
		monoFS = dedupeSorted(sub.MonoFS)
		return
	}

	groupNames := sortedKeys(file.Groups)
	for _, name := range groupNames {
		sub := file.Groups[name]
		p, l, t, m := resolve(sub)
		bundle.Grants = append(bundle.Grants, Grant{
			Group: name, Partitions: p, Logs: l, Traces: t, MonoFS: m,
			Role: sub.Role, Deny: dedupeSorted(sub.Deny),
		})
	}
	userNames := sortedKeys(file.Users)
	for _, name := range userNames {
		sub := file.Users[name]
		p, l, t, m := resolve(sub)
		bundle.Grants = append(bundle.Grants, Grant{
			User: name, Partitions: p, Logs: l, Traces: t, MonoFS: m,
			Role: sub.Role, Deny: dedupeSorted(sub.Deny),
		})
	}
	clientNames := sortedKeys(file.Clients)
	for _, name := range clientNames {
		sub := file.Clients[name]
		p, l, t, m := resolve(sub)
		bundle.Grants = append(bundle.Grants, Grant{
			Client: name, Partitions: p, Logs: l, Traces: t, MonoFS: m,
			Role: sub.Role, Deny: dedupeSorted(sub.Deny),
		})
	}

	bundle.CatalogDigest = catalogDigest(catalog)
	bundle.AWSBindings = compileAWSBindings(file, catalog)
	bundle.Version = computeVersion(bundle)
	return bundle, nil
}

// resolveScope resolves one scope list: "*" stays, selector refs and globs are
// expanded against the catalog, plain names are kept.
func resolveScope(file *File, catalog *Catalog, list []string) []string {
	if len(list) == 0 {
		return nil
	}
	var out []string
	for _, entry := range list {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == "*" {
			return []string{"*"}
		}
		if sel, ok := file.Selectors[entry]; ok {
			if catalog == nil {
				continue
			}
			ctx := &compileCtx{file: file, catalog: catalog}
			out = append(out, ctx.matchSelector(sel)...)
			continue
		}
		if strings.Contains(entry, "*") {
			if catalog == nil {
				continue
			}
			ctx := &compileCtx{file: file, catalog: catalog}
			out = append(out, ctx.expandGlob(entry)...)
			continue
		}
		out = append(out, entry)
	}
	return dedupeSorted(out)
}

func compileAWSBindings(file *File, catalog *Catalog) []AWSBinding {
	byPartition := map[string]AWSBinding{}
	if catalog != nil {
		for _, p := range catalog.Partitions {
			account := p.Labels["aws.account"]
			region := p.Labels["aws.region"]
			if account == "" && region == "" {
				continue
			}
			byPartition[p.Name] = AWSBinding{Partition: p.Name, Account: account, Region: region}
		}
	}
	for _, b := range file.AWSBindings {
		byPartition[b.Partition] = b
	}
	if len(byPartition) == 0 {
		return nil
	}
	out := make([]AWSBinding, 0, len(byPartition))
	for _, b := range byPartition {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Partition < out[j].Partition })
	return out
}

func catalogDigest(catalog *Catalog) string {
	if catalog == nil {
		return ""
	}
	partitions := append([]Partition(nil), catalog.Partitions...)
	sort.Slice(partitions, func(i, j int) bool { return partitions[i].Name < partitions[j].Name })
	data, err := json.Marshal(partitions)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// computeVersion hashes the policy content (excluding version/timestamp).
func computeVersion(bundle *Bundle) string {
	c := *bundle
	c.Version = ""
	c.GeneratedAt = ""
	data, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func dedupeSorted(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(list))
	for _, item := range list {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

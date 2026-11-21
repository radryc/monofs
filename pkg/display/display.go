// Package display implements the presentation-only display-key mapping defined
// in docs/display-key-mapping-design.md. It never changes values, storage,
// ACLs, audit rows, or query execution.
package display

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// HeaderName and QueryParam are the two ways a caller requests a profile.
const (
	HeaderName          = "X-Strata-Display-Profile"
	QueryParam          = "displayProfile"
	VersionHeader       = "X-Strata-Display-Version"
	KeepCanonicalHeader = "X-Strata-Display-Keep-Canonical"
)

// Known canonical keys that may be renamed.
var KnownKeys = map[string]bool{
	"partition":           true,
	"intent":              true,
	"asset":               true,
	"guardian_partition":  true,
	"guardian_intent":     true,
	"guardian_asset":      true,
	"guardian_asset_type": true,
	"canonical_partition": true,
	"canonical_intent":    true,
	"canonical_asset":     true,
}

// Reserved display keys may never be used.
var ReservedKeys = map[string]bool{
	"apiVersion":     true,
	"kind":           true,
	"canonical":      true,
	"view":           true,
	"displayProfile": true,
	"__proto__":      true,
	"constructor":    true,
	"prototype":      true,
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Profile is the stored DisplayProfile document.
type Profile struct {
	APIVersion       string                       `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty"`
	Kind             string                       `json:"kind,omitempty" yaml:"kind,omitempty"`
	ID               string                       `json:"id" yaml:"id"`
	Name             string                       `json:"name,omitempty" yaml:"name,omitempty"`
	Scope            string                       `json:"scope,omitempty" yaml:"scope,omitempty"` // personal | team | global
	Owner            string                       `json:"owner,omitempty" yaml:"owner,omitempty"`
	Readers          []string                     `json:"readers,omitempty" yaml:"readers,omitempty"`
	IncludeCanonical bool                         `json:"includeCanonical,omitempty" yaml:"includeCanonical,omitempty"`
	Keys             map[string]string            `json:"keys" yaml:"keys"`
	Surfaces         map[string]map[string]string `json:"surfaces,omitempty" yaml:"surfaces,omitempty"`
}

// CompiledProfile is the per-subject form embedded in a bundle.
type CompiledProfile struct {
	ID               string                       `json:"id"`
	Subjects         []string                     `json:"subjects,omitempty"`
	Keys             map[string]string            `json:"keys"`
	Surfaces         map[string]map[string]string `json:"surfaces,omitempty"`
	IncludeCanonical bool                         `json:"includeCanonical,omitempty"`
}

// Bundle is the compiled, versioned profile set consumed by Guardian and
// Doctor.
type Bundle struct {
	Version  string            `json:"version"`
	Profiles []CompiledProfile `json:"profiles"`
	Defaults map[string]string `json:"defaults,omitempty"`
}

// Validate normalizes and validates a profile.
func (p *Profile) Validate() error {
	p.ID = strings.TrimSpace(p.ID)
	if p.ID == "" {
		return fmt.Errorf("display profile id is required")
	}
	if !identifier.MatchString(strings.ReplaceAll(p.ID, "-", "_")) {
		return fmt.Errorf("display profile id %q is not a valid identifier", p.ID)
	}
	if p.Scope == "" {
		p.Scope = "personal"
	}
	switch p.Scope {
	case "personal", "team", "global":
	default:
		return fmt.Errorf("display profile scope %q must be personal, team, or global", p.Scope)
	}
	if len(p.Keys) == 0 && len(p.Surfaces) == 0 {
		return fmt.Errorf("display profile has no key mappings")
	}
	seen := map[string]string{}
	for canonical, display := range p.Keys {
		if !KnownKeys[canonical] {
			return fmt.Errorf("unknown display key %q", canonical)
		}
		if err := validateDisplayKey(display, "keys."+canonical); err != nil {
			return err
		}
		if other, ok := seen[display]; ok {
			return fmt.Errorf("display key %q is used for both %q and %q", display, other, canonical)
		}
		seen[display] = canonical
	}
	for surface, keys := range p.Surfaces {
		if strings.TrimSpace(surface) == "" {
			return fmt.Errorf("surface name must not be empty")
		}
		for canonical, display := range keys {
			if !KnownKeys[canonical] {
				return fmt.Errorf("unknown display key %q in surface %q", canonical, surface)
			}
			if err := validateDisplayKey(display, "surfaces."+surface+"."+canonical); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateDisplayKey(value, field string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	if ReservedKeys[value] {
		return fmt.Errorf("%s %q is reserved", field, value)
	}
	if !identifier.MatchString(value) {
		return fmt.Errorf("%s %q is not a valid JSON/SQL/Prometheus identifier", field, value)
	}
	return nil
}

// KeysFor returns the effective base key map for one profile/surface.
func (p *CompiledProfile) KeysFor(surface string) map[string]string {
	out := make(map[string]string, len(p.Keys))
	for k, v := range p.Keys {
		out[k] = v
	}
	if p.Surfaces != nil {
		if keys, ok := p.Surfaces[surface]; ok {
			for k, v := range keys {
				out[k] = v
			}
		}
	}
	return out
}

// Profile returns the named compiled profile, or the caller's default when id
// is empty. The second result is false when no profile applies.
func (b *Bundle) Profile(subject, id string) (*CompiledProfile, bool) {
	if b == nil {
		return nil, false
	}
	id = strings.TrimSpace(id)
	if id == "" && subject != "" && b.Defaults != nil {
		id = strings.TrimSpace(b.Defaults[subject])
	}
	if id == "" || id == "canonical" {
		return nil, false
	}
	for i := range b.Profiles {
		p := &b.Profiles[i]
		if p.ID != id {
			continue
		}
		if len(p.Subjects) == 0 {
			return p, true
		}
		for _, s := range p.Subjects {
			if s == subject || s == "*" {
				return p, true
			}
		}
		return nil, false
	}
	return nil, false
}

// MapOptions controls one rendering pass.
type MapOptions struct {
	// Surface selects a per-surface override map (sql, metrics, logs, traces).
	Surface string
	// KeepCanonical keeps the original canonical key alongside the display key.
	// The web UIs use this so their data accessors keep working while labels
	// and exported JSON can still show the user's vocabulary.
	KeepCanonical bool
}

// MapJSON renames known display keys in a JSON document. Unknown keys and all
// values are preserved.
func MapJSON(raw []byte, p *CompiledProfile, surface string) ([]byte, error) {
	return MapJSONWithOptions(raw, p, MapOptions{Surface: surface})
}

// MapJSONWithOptions is MapJSON with UI-safe canonical companions.
func MapJSONWithOptions(raw []byte, p *CompiledProfile, opts MapOptions) ([]byte, error) {
	if p == nil || len(raw) == 0 {
		return raw, nil
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	ctx := mapCtx{
		keys:             p.KeysFor(opts.Surface),
		includeCanonical: p.IncludeCanonical,
		keepCanonical:    opts.KeepCanonical,
	}
	mapped := mapValue(value, ctx)
	out, err := json.Marshal(mapped)
	if err != nil {
		return nil, err
	}
	return out, nil
}

type mapCtx struct {
	keys             map[string]string
	includeCanonical bool
	keepCanonical    bool
}

func (c mapCtx) put(out map[string]any, key string, value any) {
	if display, ok := c.keys[key]; ok {
		out[display] = value
		if c.keepCanonical {
			out[key] = value
		}
		if c.includeCanonical {
			canonicalKey := "canonical_" + key
			if _, exists := out[canonicalKey]; !exists {
				out[canonicalKey] = value
			}
		}
		return
	}
	out[key] = value
}

func mapValue(value any, ctx mapCtx) any {
	switch v := value.(type) {
	case map[string]any:
		if columns, ok := v["columns"]; ok {
			if rows, ok := v["rows"]; ok {
				mappedColumns, companions := mapSQLColumns(columns, ctx)
				out := make(map[string]any, len(v))
				for key, child := range v {
					switch key {
					case "columns":
						out[key] = mappedColumns
					case "rows":
						out[key] = appendSQLCompanions(rows, companions)
					default:
						ctx.put(out, key, mapValue(child, ctx))
					}
				}
				return out
			}
		}
		out := make(map[string]any, len(v))
		for key, child := range v {
			switch key {
			case "labels", "attributes", "resource_attributes", "metric":
				ctx.put(out, key, mapLabelMap(child, ctx))
			case "columns":
				ctx.put(out, key, mapColumns(child, ctx))
			case "partitions", "intents", "assets", "recentEvents", "nodes", "rows",
				"data", "result", "results", "series":
				ctx.put(out, key, mapValue(child, ctx))
			case "body", "payload", "metadata":
				ctx.put(out, key, child)
			default:
				ctx.put(out, key, mapValue(child, ctx))
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = mapValue(child, ctx)
		}
		return out
	default:
		return value
	}
}

func mapLabelMap(value any, ctx mapCtx) any {
	obj, ok := value.(map[string]any)
	if !ok {
		return mapValue(value, ctx)
	}
	out := make(map[string]any, len(obj))
	for key, child := range obj {
		ctx.put(out, key, child)
	}
	return out
}

type sqlCompanion struct {
	index int
	name  string
}

func mapSQLColumns(value any, ctx mapCtx) (any, []sqlCompanion) {
	list, ok := value.([]any)
	if !ok {
		return value, nil
	}
	out := make([]any, 0, len(list))
	var companions []sqlCompanion
	for _, child := range list {
		name, ok := child.(string)
		if !ok {
			out = append(out, child)
			continue
		}
		if display, ok := ctx.keys[name]; ok {
			sourceIndex := len(out)
			out = append(out, display)
			if ctx.keepCanonical {
				companions = append(companions, sqlCompanion{index: sourceIndex, name: name})
			}
			if ctx.includeCanonical {
				companions = append(companions, sqlCompanion{index: sourceIndex, name: "canonical_" + name})
			}
			continue
		}
		out = append(out, name)
	}
	if len(companions) > 0 {
		for _, companion := range companions {
			out = append(out, companion.name)
		}
	}
	// The companion entries above are appended to the column list; rows are
	// extended using the same indexes in order.
	return out, companions
}

func appendSQLCompanions(value any, companions []sqlCompanion) any {
	if len(companions) == 0 {
		return value
	}
	rows, ok := value.([]any)
	if !ok {
		return value
	}
	out := make([]any, len(rows))
	for i, raw := range rows {
		row, ok := raw.([]any)
		if !ok {
			out[i] = raw
			continue
		}
		extended := make([]any, len(row), len(row)+len(companions))
		copy(extended, row)
		for _, companion := range companions {
			var value any
			if companion.index < len(row) {
				value = row[companion.index]
			}
			extended = append(extended, value)
		}
		out[i] = extended
	}
	return out
}

func mapColumns(value any, ctx mapCtx) any {
	out, _ := mapSQLColumns(value, ctx)
	return out
}

func mapKey(key string, keys map[string]string) string {
	if mapped, ok := keys[key]; ok {
		return mapped
	}
	return key
}

// SurfaceForPath derives the per-surface override name from an API path.
func SurfaceForPath(path string) string {
	switch {
	case strings.Contains(path, "/sql"):
		return "sql"
	case strings.HasPrefix(path, "/api/v1") || strings.Contains(path, "/metrics"):
		return "metrics"
	case strings.HasPrefix(path, "/loki") || strings.Contains(path, "/logs"):
		return "logs"
	case strings.HasPrefix(path, "/api/v2") || strings.Contains(path, "/traces"):
		return "traces"
	}
	return ""
}

// CanonicalKeys returns the sorted list of keys that a profile renames.
func CanonicalKeys(p *CompiledProfile) []string {
	if p == nil {
		return nil
	}
	set := map[string]bool{}
	for k := range p.KeysFor("") {
		set[k] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Field reads a value by canonical key or by its display key.
func Field(obj map[string]any, canonical string, p *CompiledProfile) any {
	if obj == nil {
		return nil
	}
	if p != nil {
		if display, ok := p.Keys[canonical]; ok {
			if value, ok := obj[display]; ok {
				return value
			}
		}
	}
	return obj[canonical]
}

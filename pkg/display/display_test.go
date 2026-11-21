package display

import (
	"encoding/json"
	"testing"
)

func TestProfileValidate(t *testing.T) {
	p := Profile{ID: "engineering", Keys: map[string]string{"partition": "eng_service", "intent": "deployment"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	bad := Profile{ID: "x", Keys: map[string]string{"partition": "__proto__"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected reserved key rejection")
	}
	dup := Profile{ID: "x", Keys: map[string]string{"partition": "same", "intent": "same"}}
	if err := dup.Validate(); err == nil {
		t.Fatal("expected duplicate display key rejection")
	}
	unknown := Profile{ID: "x", Keys: map[string]string{"nope": "value"}}
	if err := unknown.Validate(); err == nil {
		t.Fatal("expected unknown canonical key rejection")
	}
}

func TestMapJSON(t *testing.T) {
	p := &CompiledProfile{ID: "eng", Keys: map[string]string{
		"partition":          "eng_service",
		"guardian_partition": "guardian_eng_service",
	}}
	raw := []byte(`{"partition":"p0","intent":[{"partition":"p1"}],"labels":{"guardian_partition":"p0"},"body":{"partition":"keep-me"}}`)
	out, err := MapJSON(raw, p, "")
	if err != nil {
		t.Fatalf("MapJSON() error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["eng_service"] != "p0" {
		t.Fatalf("top-level partition not mapped: %v", got)
	}
	if _, ok := got["partition"]; ok {
		t.Fatal("canonical partition key still present")
	}
	labels, _ := got["labels"].(map[string]any)
	if labels["guardian_eng_service"] != "p0" {
		t.Fatalf("label key not mapped: %v", labels)
	}
	body, _ := got["body"].(map[string]any)
	if body["partition"] != "keep-me" {
		t.Fatalf("body keys must not be mapped: %v", body)
	}
}

func TestProfileLookup(t *testing.T) {
	b := &Bundle{
		Profiles: []CompiledProfile{{ID: "eng", Subjects: []string{"user:alice"}, Keys: map[string]string{"partition": "eng_service"}}},
		Defaults: map[string]string{"user:alice": "eng"},
	}
	if p, ok := b.Profile("user:alice", ""); !ok || p.ID != "eng" {
		t.Fatalf("default profile lookup failed: %+v", p)
	}
	if _, ok := b.Profile("user:bob", "eng"); ok {
		t.Fatal("profile must not leak to a non-subject")
	}
}

func TestMapJSONIncludeCanonical(t *testing.T) {
	p := &CompiledProfile{
		ID:               "eng",
		IncludeCanonical: true,
		Keys:             map[string]string{"partition": "eng_service", "guardian_partition": "guardian_eng_service"},
	}
	raw := []byte(`{"partition":"p0","labels":{"guardian_partition":"p0"}}`)
	out, err := MapJSON(raw, p, "")
	if err != nil {
		t.Fatalf("MapJSON() error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["eng_service"] != "p0" || got["canonical_partition"] != "p0" {
		t.Fatalf("canonical companions missing: %v", got)
	}
	labels := got["labels"].(map[string]any)
	if labels["guardian_eng_service"] != "p0" || labels["canonical_guardian_partition"] != "p0" {
		t.Fatalf("label canonical companion missing: %v", labels)
	}
}

func TestMapJSONSQLColumnsIncludeCanonical(t *testing.T) {
	p := &CompiledProfile{
		ID:               "eng",
		IncludeCanonical: true,
		Keys:             map[string]string{"partition": "eng_service"},
	}
	raw := []byte(`{"columns":["partition","count"],"rows":[["p0",42],["p1",7]]}`)
	out, err := MapJSON(raw, p, "sql")
	if err != nil {
		t.Fatalf("MapJSON() error = %v", err)
	}
	var got struct {
		Columns []string `json:"columns"`
		Rows    [][]any  `json:"rows"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Columns) != 3 || got.Columns[0] != "eng_service" || got.Columns[2] != "canonical_partition" {
		t.Fatalf("columns = %v", got.Columns)
	}
	if len(got.Rows[0]) != 3 || got.Rows[0][2] != "p0" || got.Rows[1][2] != "p1" {
		t.Fatalf("rows = %v", got.Rows)
	}
}

func TestSurfaceForPath(t *testing.T) {
	cases := map[string]string{
		"/v1/sql/query":      "sql",
		"/api/v1/query":      "metrics",
		"/loki/api/v1/query": "logs",
		"/api/v2/traces":     "traces",
		"/v1/partitions":     "",
	}
	for path, want := range cases {
		if got := SurfaceForPath(path); got != want {
			t.Fatalf("SurfaceForPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestSurfaceOverride(t *testing.T) {
	p := &CompiledProfile{
		ID:   "eng",
		Keys: map[string]string{"partition": "eng_service"},
		Surfaces: map[string]map[string]string{
			"sql": {"partition": "sql_partition"},
		},
	}
	raw := []byte(`{"partition":"p0"}`)
	out, err := MapJSON(raw, p, "sql")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["sql_partition"] != "p0" {
		t.Fatalf("sql surface override not applied: %v", got)
	}
}

func TestCanonicalBypass(t *testing.T) {
	b := &Bundle{Profiles: []CompiledProfile{{ID: "eng", Subjects: []string{"*"}, Keys: map[string]string{"partition": "eng_service"}}}}
	if _, ok := b.Profile("user:a", "canonical"); ok {
		t.Fatal("canonical must bypass mapping")
	}
}

func TestMapJSONKeepCanonical(t *testing.T) {
	p := &CompiledProfile{ID: "eng", Keys: map[string]string{"partition": "eng_service"}}
	out, err := MapJSONWithOptions([]byte(`{"partition":"p0","labels":{"guardian_partition":"p0"}}`), p, MapOptions{KeepCanonical: true})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["eng_service"] != "p0" || got["partition"] != "p0" {
		t.Fatalf("keep-canonical mapping = %v", got)
	}
}

func TestMapJSONSQLKeepCanonical(t *testing.T) {
	p := &CompiledProfile{ID: "eng", Keys: map[string]string{"partition": "eng_service"}}
	out, err := MapJSONWithOptions([]byte(`{"columns":["partition","count"],"rows":[["p0",42]]}`), p, MapOptions{KeepCanonical: true})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Columns []string `json:"columns"`
		Rows    [][]any  `json:"rows"`
	}
	_ = json.Unmarshal(out, &got)
	if len(got.Columns) != 3 || got.Columns[0] != "eng_service" || got.Columns[1] != "count" || got.Columns[2] != "partition" {
		t.Fatalf("columns = %v", got.Columns)
	}
	if len(got.Rows[0]) != 3 || got.Rows[0][0] != "p0" || got.Rows[0][1] != float64(42) || got.Rows[0][2] != "p0" {
		t.Fatalf("rows = %v", got.Rows)
	}
}

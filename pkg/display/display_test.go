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

package authz

import "testing"

func TestParseOwnersValid(t *testing.T) {
	data := []byte(`
version: 1
viewers:
  - "@everyone"
ingesters:
  - "@ci-bots"
  - "svc-ingest@example.com"
maintainers:
  - "@platform-eng"
  - "alice@example.com"
`)
	o, err := ParseOwners(data)
	if err != nil {
		t.Fatalf("ParseOwners: %v", err)
	}
	if len(o.Viewers) != 1 || !o.Viewers[0].IsTeam() || o.Viewers[0].Team != "everyone" {
		t.Fatalf("unexpected viewers: %+v", o.Viewers)
	}
	if len(o.Ingesters) != 2 || o.Ingesters[1].Subject != "svc-ingest@example.com" {
		t.Fatalf("unexpected ingesters: %+v", o.Ingesters)
	}
	byRole := o.ByRole()
	if len(byRole[RoleMaintainer]) != 2 {
		t.Fatalf("expected 2 maintainers, got %d", len(byRole[RoleMaintainer]))
	}
	if byRole[RoleMaintainer][0].String() != "@platform-eng" {
		t.Fatalf("unexpected maintainer string: %q", byRole[RoleMaintainer][0].String())
	}
}

func TestParseOwnersErrors(t *testing.T) {
	cases := map[string][]byte{
		"missing version": []byte(`viewers: ["@a"]`),
		"wrong version":   []byte("version: 2\nviewers: [\"@a\"]"),
		"empty owners":    []byte("version: 1"),
		"empty team":      []byte("version: 1\nmaintainers: [\"@\"]"),
		"bad yaml":        []byte("version: 1\nviewers: [oops"),
	}
	for name, data := range cases {
		if _, err := ParseOwners(data); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestParseOwnerRef(t *testing.T) {
	team, err := parseOwnerRef("  @platform-eng ")
	if err != nil || !team.IsTeam() || team.Team != "platform-eng" {
		t.Fatalf("team parse failed: %+v err=%v", team, err)
	}
	subj, err := parseOwnerRef("alice@example.com")
	if err != nil || subj.IsTeam() || subj.Subject != "alice@example.com" {
		t.Fatalf("subject parse failed: %+v err=%v", subj, err)
	}
	if _, err := parseOwnerRef("   "); err == nil {
		t.Error("expected error for empty entry")
	}
}

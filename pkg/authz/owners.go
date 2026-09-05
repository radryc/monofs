package authz

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// OwnersFileName is the conventional file name for an OWNERS file.
const OwnersFileName = "OWNERS"

// OwnersDir is the directory (relative to a partition or subtree root) that
// holds the OWNERS file, e.g. /<partition>/.guardian/OWNERS.
const OwnersDir = ".guardian"

// OwnerRef references either a team (an IdP group handle, written "@team") or an
// individual subject (a bare principal id or email). Exactly one field is set.
type OwnerRef struct {
	// Team is a group handle without the leading "@".
	Team string
	// Subject is an individual principal id or email.
	Subject string
}

// IsTeam reports whether the reference is a team handle.
func (r OwnerRef) IsTeam() bool { return r.Team != "" }

// String renders the reference in OWNERS syntax.
func (r OwnerRef) String() string {
	if r.IsTeam() {
		return "@" + r.Team
	}
	return r.Subject
}

// parseOwnerRef parses a single OWNERS entry. "@team" becomes a Team ref;
// anything else becomes a Subject ref.
func parseOwnerRef(raw string) (OwnerRef, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return OwnerRef{}, fmt.Errorf("authz: empty owner entry")
	}
	if strings.HasPrefix(s, "@") {
		team := strings.TrimSpace(strings.TrimPrefix(s, "@"))
		if team == "" {
			return OwnerRef{}, fmt.Errorf("authz: owner entry %q has empty team handle", raw)
		}
		return OwnerRef{Team: team}, nil
	}
	return OwnerRef{Subject: s}, nil
}

// OwnersFile is a parsed OWNERS document mapping roles to owner references.
type OwnersFile struct {
	Version     int
	Viewers     []OwnerRef
	Ingesters   []OwnerRef
	Maintainers []OwnerRef
}

type ownersYAML struct {
	Version     int      `yaml:"version"`
	Viewers     []string `yaml:"viewers"`
	Ingesters   []string `yaml:"ingesters"`
	Maintainers []string `yaml:"maintainers"`
}

// ByRole returns the owner references grouped by the role they are granted.
func (o *OwnersFile) ByRole() map[Role][]OwnerRef {
	return map[Role][]OwnerRef{
		RoleViewer:     o.Viewers,
		RoleIngester:   o.Ingesters,
		RoleMaintainer: o.Maintainers,
	}
}

// ParseOwners parses an OWNERS document. It requires version 1 and at least one
// owner entry, and rejects malformed entries.
func ParseOwners(data []byte) (*OwnersFile, error) {
	var doc ownersYAML
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("authz: parse OWNERS: %w", err)
	}
	if doc.Version != 1 {
		return nil, fmt.Errorf("authz: unsupported OWNERS version %d (expected 1)", doc.Version)
	}

	parseList := func(role string, raw []string) ([]OwnerRef, error) {
		refs := make([]OwnerRef, 0, len(raw))
		for _, entry := range raw {
			ref, err := parseOwnerRef(entry)
			if err != nil {
				return nil, fmt.Errorf("authz: %s: %w", role, err)
			}
			refs = append(refs, ref)
		}
		return refs, nil
	}

	viewers, err := parseList("viewers", doc.Viewers)
	if err != nil {
		return nil, err
	}
	ingesters, err := parseList("ingesters", doc.Ingesters)
	if err != nil {
		return nil, err
	}
	maintainers, err := parseList("maintainers", doc.Maintainers)
	if err != nil {
		return nil, err
	}
	if len(viewers)+len(ingesters)+len(maintainers) == 0 {
		return nil, fmt.Errorf("authz: OWNERS file has no owners")
	}

	return &OwnersFile{
		Version:     doc.Version,
		Viewers:     viewers,
		Ingesters:   ingesters,
		Maintainers: maintainers,
	}, nil
}

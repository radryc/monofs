package authz

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// TeamMapping maps an OWNERS team handle (without "@") to the IdP group name
// that grant evaluation matches against identity group membership.
type TeamMapping map[string]string

type teamMappingYAML struct {
	Version int               `yaml:"version"`
	Teams   map[string]string `yaml:"teams"`
}

// ParseTeamMapping parses a team-to-group mapping document.
func ParseTeamMapping(data []byte) (TeamMapping, error) {
	var doc teamMappingYAML
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("authz: parse team mapping: %w", err)
	}
	if doc.Version != 1 {
		return nil, fmt.Errorf("authz: unsupported team mapping version %d (expected 1)", doc.Version)
	}
	mapping := make(TeamMapping, len(doc.Teams))
	for team, group := range doc.Teams {
		mapping[team] = group
	}
	return mapping, nil
}

// GroupFor resolves a team handle to its IdP group. When no mapping exists the
// handle itself is used and ok is false, so callers can surface a warning.
func (m TeamMapping) GroupFor(team string) (group string, ok bool) {
	if m != nil {
		if g, found := m[team]; found {
			return g, true
		}
	}
	return team, false
}

// CompileGrants turns a partition's OWNERS file into partition-scoped grants,
// resolving team handles to IdP groups via mapping. It returns any warnings for
// teams that lacked an explicit mapping (the handle is used as the group name).
// Grants are emitted in a deterministic role order (viewer, ingester, maintainer).
func CompileGrants(partition string, owners *OwnersFile, mapping TeamMapping) (grants []Grant, warnings []string) {
	if owners == nil {
		return nil, nil
	}
	byRole := owners.ByRole()
	for _, role := range []Role{RoleViewer, RoleIngester, RoleMaintainer} {
		for _, ref := range byRole[role] {
			if ref.IsTeam() {
				group, ok := mapping.GroupFor(ref.Team)
				if !ok {
					warnings = append(warnings, fmt.Sprintf(
						"team %q on partition %q has no group mapping; using handle as group name",
						ref.Team, partition))
				}
				grants = append(grants, Grant{Group: group, Partition: partition, Role: role})
			} else {
				grants = append(grants, Grant{Subject: ref.Subject, Partition: partition, Role: role})
			}
		}
	}
	return grants, warnings
}

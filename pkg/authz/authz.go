// Package authz provides the shared identity and authorization primitives used
// across MonoFS and Guardian to enforce partition-scoped access control.
//
// The package is deliberately dependency-light: it defines the identity model,
// the role/action vocabulary, and the interfaces (TokenVerifier, GrantEvaluator)
// that concrete implementations plug into. Enforcement wiring lives in the
// router/control-plane packages that consume these primitives.
package authz

import "strings"

// Action is a coarse capability requested against a partition (or subtree).
type Action string

const (
	// ActionView covers reading or mounting partition content.
	ActionView Action = "view"
	// ActionIngest covers writing/ingesting content into a partition.
	ActionIngest Action = "ingest"
	// ActionModify covers modifying/deploying/reconciling a partition and
	// managing its grants (control-plane changes).
	ActionModify Action = "modify"
)

// Role is a named bundle of actions granted on a partition.
type Role string

const (
	// RoleViewer may view/read/mount a partition.
	RoleViewer Role = "viewer"
	// RoleIngester may view and ingest content into a partition.
	RoleIngester Role = "ingester"
	// RoleMaintainer may view, ingest, and modify a partition.
	RoleMaintainer Role = "maintainer"
	// RoleAdmin is a global role that implies every action on every partition.
	RoleAdmin Role = "admin"
)

// roleActions declares which actions each role permits on its partition.
var roleActions = map[Role]map[Action]bool{
	RoleViewer:     {ActionView: true},
	RoleIngester:   {ActionView: true, ActionIngest: true},
	RoleMaintainer: {ActionView: true, ActionIngest: true, ActionModify: true},
	RoleAdmin:      {ActionView: true, ActionIngest: true, ActionModify: true},
}

// Allows reports whether the role permits the given action.
func (r Role) Allows(action Action) bool {
	return roleActions[r][action]
}

// Valid reports whether the role is a recognized value.
func (r Role) Valid() bool {
	_, ok := roleActions[r]
	return ok
}

// Identity is a verified caller: a human (Subject/Email/Groups) or a machine
// client (ClientID). It is produced by a TokenVerifier from a bearer token.
type Identity struct {
	// Subject is the stable IdP subject identifier (OIDC "sub").
	Subject string
	// Email is the caller's email, when present.
	Email string
	// Groups are IdP group memberships used to resolve team-based grants.
	Groups []string
	// ClientID identifies a machine principal (OAuth client), when present.
	ClientID string
}

// IsAnonymous reports whether the identity carries no verified principal.
func (id Identity) IsAnonymous() bool {
	return id.Subject == "" && id.ClientID == ""
}

// HasGroup reports whether the identity belongs to the given group
// (case-insensitive).
func (id Identity) HasGroup(group string) bool {
	group = strings.TrimSpace(group)
	if group == "" {
		return false
	}
	for _, g := range id.Groups {
		if strings.EqualFold(strings.TrimSpace(g), group) {
			return true
		}
	}
	return false
}

// PrincipalID returns a stable identifier for the principal, preferring the
// machine ClientID and falling back to the human Subject. Returns "" when
// anonymous.
func (id Identity) PrincipalID() string {
	if id.ClientID != "" {
		return id.ClientID
	}
	return id.Subject
}

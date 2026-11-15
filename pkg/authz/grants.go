package authz

// Grant authorizes a subject or group with a Role on a partition. It is used to
// represent OWNERS/CODEOWNERS-derived ownership; runtime authorization is
// decided by the repo-managed access policy (pkg/authz/policy).
type Grant struct {
	// Subject matches an individual principal by Subject, ClientID, or Email.
	Subject string `json:"subject,omitempty"`
	// Group matches any principal whose IdP groups include this value.
	Group string `json:"group,omitempty"`
	// Partition is the partition name this grant applies to, or "*" for all.
	Partition string `json:"partition"`
	// Role is the role granted on the partition.
	Role Role `json:"role"`
}

// WildcardPartition matches every partition in OWNERS-derived grants.
const WildcardPartition = "*"

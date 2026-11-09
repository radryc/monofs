package policy

import "testing"

func TestPartitionVisibilityMatchesCanSeePartition(t *testing.T) {
	p := compileTestPolicy(t, testPolicyYAML, testCatalog())

	cases := []struct {
		name       string
		partitions []string
	}{
		{name: "anonymous"},
		{name: "user", partitions: []string{"aws-x", "k8s-z", "nonexistent"}},
		{name: "contractor", partitions: []string{"aws-x", "k8s-z"}},
		{name: "admin", partitions: []string{"aws-x", "k8s-z", "nonexistent"}},
		{name: "group", partitions: []string{"aws-x", "k8s-z", "k8s-other"}},
	}

	identitiesByCase := map[string]authzIdentity{
		"anonymous":  {},
		"user":       {email: "foo@acme.com"},
		"contractor": {email: "contractor@vendor.com"},
		"admin":      {email: "admin@acme.com"},
		"group":      {email: "someone@acme.com", groups: []string{"sre"}},
	}

	for _, tc := range cases {
		ident := identitiesByCase[tc.name]
		id := identity(ident.email, ident.groups...)
		visibility := p.PartitionVisibility(id)
		for _, partition := range tc.partitions {
			want := p.CanSeePartition(id, partition)
			if got := visibility.CanSee(partition); got != want {
				t.Fatalf("%s partition %q: visibility=%v CanSeePartition=%v", tc.name, partition, got, want)
			}
		}
	}
}

type authzIdentity struct {
	email  string
	groups []string
}

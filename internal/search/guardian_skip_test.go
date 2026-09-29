package search

import "testing"

func TestIsGuardianRepo(t *testing.T) {
	tests := []struct {
		displayPath string
		source      string
		want        bool
	}{
		{"guardian/agent", "", true},
		{"guardian", "", true},
		{"guardian/guardian-configs", "guardian://guardian/guardian-configs", true},
		{"doctor/v1", "guardian://doctor/v1", true},
		{"", "guardian://guardian-system", true},
		{"", "some remote-guardian remote", true},
		{"github.com/owner/repo", "https://github.com/owner/repo.git", false},
		{"monofs", "https://example.com/repo.git", false},
	}
	for _, tt := range tests {
		if got := isGuardianRepo(tt.displayPath, tt.source); got != tt.want {
			t.Errorf("isGuardianRepo(%q, %q) = %v, want %v", tt.displayPath, tt.source, got, tt.want)
		}
	}
}

func TestShouldSkipGuardian(t *testing.T) {
	// Disabled by default: guardian partitions are skipped.
	s := &Service{}
	if !s.shouldSkipGuardian("guardian/agent", "guardian://guardian/agent") {
		t.Fatal("guardian repo should be skipped when IndexGuardian is false")
	}
	// Non-guardian repos are never skipped.
	if s.shouldSkipGuardian("github.com/owner/repo", "https://github.com/owner/repo.git") {
		t.Fatal("non-guardian repo should not be skipped")
	}
	// Explicitly enabling guardian indexing stops the skip.
	s.indexGuardian = true
	if s.shouldSkipGuardian("guardian/agent", "guardian://guardian/agent") {
		t.Fatal("guardian repo should not be skipped when IndexGuardian is true")
	}
}

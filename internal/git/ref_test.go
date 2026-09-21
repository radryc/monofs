package git

import "testing"

func TestClassifyRef(t *testing.T) {
	cases := []struct {
		ref  string
		kind RefKind
	}{
		{"main", RefNamed},
		{"feature/foo", RefNamed},
		{"refs/tags/v1.0.0", RefTag},
		{"refs/heads/main", RefNamed},
		{"refs/remotes/origin/main", RefNamed},
		{"0123456789012345678901234567890123456789", RefSHA}, // 40 hex
		{"0123456789012345678901234567890123456789012345678901234567890123", RefSHA}, // 64 hex
	}
	for _, c := range cases {
		if got := ClassifyRef(c.ref); got != c.kind {
			t.Errorf("ClassifyRef(%q) = %v, want %v", c.ref, got, c.kind)
		}
	}
}

func TestIsHexSHA(t *testing.T) {
	valid := []string{"0123456789012345678901234567890123456789", "abcdef0000000000000000000000000000000000000000000000000000000000"}
	for _, s := range valid {
		if !IsHexSHA(s) {
			t.Errorf("IsHexSHA(%q) = false, want true", s)
		}
	}
	invalid := []string{"main", "v1.0.0", "012345678901234567890123456789012345678", "zzzz", "123456789012345678901234567890123456789G"}
	for _, s := range invalid {
		if IsHexSHA(s) {
			t.Errorf("IsHexSHA(%q) = true, want false", s)
		}
	}
}

func TestTrimRefPrefix(t *testing.T) {
	cases := map[string]string{
		"refs/tags/v1.0.0":         "v1.0.0",
		"refs/heads/main":          "main",
		"refs/remotes/origin/main": "origin/main",
		"main":                     "main",
	}
	for in, want := range cases {
		if got := TrimRefPrefix(in); got != want {
			t.Errorf("TrimRefPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

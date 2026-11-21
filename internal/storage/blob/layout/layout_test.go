package layout

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeDigest(t *testing.T) {
	hex := strings.Repeat("ab", 32)
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "bare", in: hex, want: hex},
		{name: "prefixed", in: "sha256:" + hex, want: hex},
		{name: "uppercase", in: "SHA256:" + strings.ToUpper(hex), want: hex},
		{name: "too short", in: "abc", wantErr: true},
		{name: "non hex", in: strings.Repeat("zz", 32), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeDigest(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShardAndKeys(t *testing.T) {
	digest := "ab" + strings.Repeat("0", 62)
	shard, err := Shard(digest)
	if err != nil {
		t.Fatal(err)
	}
	if shard != "ab" {
		t.Fatalf("shard = %q, want ab", shard)
	}
	if got, want := ManifestKey(shard), "packs/v2/sha256/ab/MANIFEST.json"; got != want {
		t.Fatalf("manifest key = %q, want %q", got, want)
	}
	if got, want := PackKey(shard, "123-0000"), "packs/v2/sha256/ab/123-0000.pack"; got != want {
		t.Fatalf("pack key = %q, want %q", got, want)
	}
	if got, want := IndexKey(shard, "123-0000"), "packs/v2/sha256/ab/123-0000.pack.idx"; got != want {
		t.Fatalf("index key = %q, want %q", got, want)
	}
}

func TestValidateShard(t *testing.T) {
	for _, ok := range []string{"00", "9f", "ab", "ff"} {
		if err := ValidateShard(ok); err != nil {
			t.Fatalf("ValidateShard(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a", "abc", "AB", "zz", "a/"} {
		if err := ValidateShard(bad); err == nil {
			t.Fatalf("ValidateShard(%q) expected error", bad)
		}
	}
}

func TestNewPackIDIsOrdered(t *testing.T) {
	base := time.Unix(0, 100)
	a := NewPackID(base, 0)
	b := NewPackID(base, 1)
	c := NewPackID(base.Add(time.Nanosecond), 0)
	if !(a < b && b < c) {
		t.Fatalf("pack IDs not ordered: %s %s %s", a, b, c)
	}
}

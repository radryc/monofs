package authz

import (
	"context"
	"reflect"
	"testing"
)

// mapOwnersSource maps a directory to its OWNERS file for tests.
type mapOwnersSource map[string]*OwnersFile

func (m mapOwnersSource) LoadOwners(_ context.Context, dir string) (*OwnersFile, error) {
	return m[dir], nil
}

func TestAncestorDirs(t *testing.T) {
	got := ancestorDirs("/a/b/c.txt")
	want := []string{"a/b/c.txt", "a/b", "a", ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ancestorDirs = %v, want %v", got, want)
	}
	if got := ancestorDirs(""); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("root ancestorDirs = %v", got)
	}
}

func TestOwnershipResolverNearestAndAncestor(t *testing.T) {
	src := mapOwnersSource{
		"":    &OwnersFile{Version: 1, Maintainers: []OwnerRef{{Team: "platform-eng"}}},
		"a/b": &OwnersFile{Version: 1, Maintainers: []OwnerRef{{Subject: "alice"}}},
	}
	mapping := TeamMapping{"platform-eng": "strata-platform-eng"}
	r := NewOwnershipResolver(src, mapping)
	ctx := context.Background()

	alice := Identity{Subject: "alice"}
	eng := Identity{Subject: "bob", Groups: []string{"strata-platform-eng"}}
	stranger := Identity{Subject: "carol"}

	// alice owns via nearest a/b OWNERS.
	owned, dir, err := r.IsOwner(ctx, "a/b/c.txt", alice)
	if err != nil || !owned || dir != "a/b" {
		t.Fatalf("alice a/b/c.txt: owned=%v dir=%q err=%v", owned, dir, err)
	}

	// platform-eng owns everything via root OWNERS.
	owned, dir, _ = r.IsOwner(ctx, "a/b/c.txt", eng)
	if !owned || dir != "" {
		t.Fatalf("eng a/b/c.txt: owned=%v dir=%q", owned, dir)
	}

	// alice does NOT own paths outside a/b.
	if owned, _, _ := r.IsOwner(ctx, "x/y.txt", alice); owned {
		t.Fatal("alice should not own x/y.txt")
	}

	// stranger owns nothing.
	if owned, _, _ := r.IsOwner(ctx, "a/b/c.txt", stranger); owned {
		t.Fatal("stranger should own nothing")
	}

	// anonymous owns nothing.
	if owned, _, _ := r.IsOwner(ctx, "a/b/c.txt", Identity{}); owned {
		t.Fatal("anonymous should own nothing")
	}
}

func TestOwnsAll(t *testing.T) {
	src := mapOwnersSource{
		"":    &OwnersFile{Version: 1, Maintainers: []OwnerRef{{Team: "platform-eng"}}},
		"a/b": &OwnersFile{Version: 1, Maintainers: []OwnerRef{{Subject: "alice"}}},
	}
	r := NewOwnershipResolver(src, TeamMapping{"platform-eng": "strata-platform-eng"})
	ctx := context.Background()
	alice := Identity{Subject: "alice"}

	// alice owns all changes within a/b.
	ownsAll, unowned, err := r.OwnsAll(ctx, []string{"a/b/c.txt", "a/b/d.txt"}, alice)
	if err != nil || !ownsAll || len(unowned) != 0 {
		t.Fatalf("alice within a/b: ownsAll=%v unowned=%v err=%v", ownsAll, unowned, err)
	}

	// A change outside a/b makes alice a non-owner of that path -> merge request.
	ownsAll, unowned, _ = r.OwnsAll(ctx, []string{"a/b/c.txt", "x/y.txt"}, alice)
	if ownsAll || !reflect.DeepEqual(unowned, []string{"x/y.txt"}) {
		t.Fatalf("alice mixed: ownsAll=%v unowned=%v", ownsAll, unowned)
	}
}

func TestOwnersPath(t *testing.T) {
	if got := OwnersPath(""); got != ".guardian/OWNERS" {
		t.Fatalf("root OwnersPath = %q", got)
	}
	if got := OwnersPath("/a/b/"); got != "a/b/.guardian/OWNERS" {
		t.Fatalf("subtree OwnersPath = %q", got)
	}
}

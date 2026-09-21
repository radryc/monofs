package fuse

import (
	"strings"
	"testing"
)

func merge(t *testing.T, base, ours, theirs string) (string, int) {
	t.Helper()
	merged, conflicts := Merge3([]byte(base), []byte(ours), []byte(theirs))
	return string(merged), conflicts
}

func TestMerge3OursOnly(t *testing.T) {
	base := "line1\nline2\nline3\n"
	ours := "line1\nline2-modified\nline3\n"

	merged, conflicts := merge(t, base, ours, base)
	if conflicts != 0 {
		t.Fatalf("conflicts = %d, want 0", conflicts)
	}
	if merged != ours {
		t.Errorf("merged = %q, want ours content", merged)
	}
}

func TestMerge3TheirsOnly(t *testing.T) {
	base := "line1\nline2\nline3\n"
	theirs := "line1\nline2-upstream\nline3\n"

	merged, conflicts := merge(t, base, base, theirs)
	if conflicts != 0 {
		t.Fatalf("conflicts = %d, want 0", conflicts)
	}
	if merged != theirs {
		t.Errorf("merged = %q, want theirs content", merged)
	}
}

func TestMerge3IdenticalChanges(t *testing.T) {
	base := "line1\nline2\nline3\n"
	same := "line1\nline2-changed\nline3\n"

	merged, conflicts := merge(t, base, same, same)
	if conflicts != 0 {
		t.Fatalf("conflicts = %d, want 0", conflicts)
	}
	if merged != same {
		t.Errorf("merged = %q, want shared change", merged)
	}
}

func TestMerge3NonOverlappingChanges(t *testing.T) {
	base := "a\nb\nc\nd\ne\nf\ng\n"
	// ours changes the head, theirs changes the tail.
	ours := "A\nb\nc\nd\ne\nf\ng\n"
	theirs := "a\nb\nc\nd\ne\nf\nG\n"

	merged, conflicts := merge(t, base, ours, theirs)
	if conflicts != 0 {
		t.Fatalf("conflicts = %d, want 0", conflicts)
	}
	want := "A\nb\nc\nd\ne\nf\nG\n"
	if merged != want {
		t.Errorf("merged = %q, want %q", merged, want)
	}
}

func TestMerge3InterleavedInsertions(t *testing.T) {
	base := "one\nfour\n"
	// ours inserts after "one", theirs inserts after "one" too but at a
	// different position in the same region.
	ours := "one\ntwo\nfour\n"
	theirs := "one\nthree\nfour\n"

	merged, conflicts := merge(t, base, ours, theirs)
	if conflicts == 0 {
		t.Fatal("expected a conflict when both sides insert in the same region")
	}
	if !strings.Contains(merged, mergeConflictStart) || !strings.Contains(merged, mergeConflictEnd) {
		t.Errorf("merged output missing conflict markers:\n%s", merged)
	}
	if !strings.Contains(merged, "two") || !strings.Contains(merged, "three") {
		t.Errorf("merged output must contain both sides:\n%s", merged)
	}
}

func TestMerge3ConflictSameLine(t *testing.T) {
	base := "alpha\nbeta\ngamma\n"
	ours := "alpha\nLOCAL\ngamma\n"
	theirs := "alpha\nUPSTREAM\ngamma\n"

	merged, conflicts := merge(t, base, ours, theirs)
	if conflicts != 1 {
		t.Fatalf("conflicts = %d, want 1", conflicts)
	}
	if !strings.Contains(merged, "<<<<<<< MONOFS-LOCAL") ||
		!strings.Contains(merged, "||||||| BASE") ||
		!strings.Contains(merged, "=======") ||
		!strings.Contains(merged, ">>>>>>> MONOFS-UPSTREAM") {
		t.Errorf("merged output missing diff3 markers:\n%s", merged)
	}
	if !strings.Contains(merged, "beta") {
		t.Errorf("merged output should include the base region:\n%s", merged)
	}
}

func TestMerge3AdjacentChangesMerge(t *testing.T) {
	// ours modifies line 2, theirs modifies line 4 — adjacent but
	// distinct hunks should merge cleanly.
	base := "1\n2\n3\n4\n5\n"
	ours := "1\ntwo\n3\n4\n5\n"
	theirs := "1\n2\n3\nFOUR\n5\n"

	merged, conflicts := merge(t, base, ours, theirs)
	if conflicts != 0 {
		t.Fatalf("conflicts = %d, want 0: %s", conflicts, merged)
	}
	want := "1\ntwo\n3\nFOUR\n5\n"
	if merged != want {
		t.Errorf("merged = %q, want %q", merged, want)
	}
}

func TestMerge3EmptyBaseAddAdd(t *testing.T) {
	ours := "created locally\n"
	theirs := "created upstream\n"

	merged, conflicts := merge(t, "", ours, theirs)
	if conflicts == 0 {
		t.Fatal("add/add with different content should conflict")
	}
	if !strings.Contains(merged, "created locally") || !strings.Contains(merged, "created upstream") {
		t.Errorf("merged output missing both sides:\n%s", merged)
	}
}

func TestMerge3EmptySides(t *testing.T) {
	merged, conflicts := merge(t, "a\n", "a\n", "a\n")
	if conflicts != 0 || merged != "a\n" {
		t.Errorf("merge of identical content = %q, %d", merged, conflicts)
	}

	merged, conflicts = merge(t, "", "", "")
	if conflicts != 0 || merged != "" {
		t.Errorf("merge of empty content = %q, %d", merged, conflicts)
	}
}

func TestMerge3MissingTrailingNewline(t *testing.T) {
	base := "a\nb"
	ours := "a\nB-local"
	theirs := "a\nb-upstream"

	merged, conflicts := merge(t, base, ours, theirs)
	if conflicts == 0 {
		t.Fatal("conflicting edits to the final line should conflict")
	}
	if !strings.Contains(merged, "B-local") || !strings.Contains(merged, "b-upstream") {
		t.Errorf("merged output missing both sides:\n%s", merged)
	}
}

func TestHasConflictMarkers(t *testing.T) {
	clean := []byte("just some code\n")
	if hasConflictMarkers(clean) {
		t.Error("clean content flagged as conflicted")
	}

	conflicted := []byte("code\n" + mergeConflictStart + "\nours\n" + mergeConflictEnd + "\n")
	if !hasConflictMarkers(conflicted) {
		t.Error("conflicted content not detected")
	}
}

package workspacebundle

import "testing"

func TestValidateOperationRenameTarget(t *testing.T) {
	if err := validateOperation("sid", 0, Operation{Kind: OperationRename, Path: "a/b.txt", Target: "c/d.txt"}); err != nil {
		t.Fatalf("valid rename target rejected: %v", err)
	}
	for _, target := range []string{"", "../escape.txt", "/abs.txt", ".git/config", "a/../../x"} {
		op := Operation{Kind: OperationRename, Path: "a/b.txt", Target: target}
		if err := validateOperation("sid", 0, op); err == nil {
			t.Fatalf("rename target %q should be rejected", target)
		}
	}
}

func TestIsSafeRelativePathRejectsGitCaseInsensitively(t *testing.T) {
	for _, p := range []string{".git/config", ".GIT/config", "sub/.Git/x"} {
		if isSafeRelativePath(p) {
			t.Fatalf("path %q should be rejected", p)
		}
	}
	if !isSafeRelativePath("src/main.go") {
		t.Fatal("ordinary path should be allowed")
	}
}

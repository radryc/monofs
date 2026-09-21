package pipeline

import (
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// DetectAffectedPackages returns the transitive closure of packages
// affected by the given changed files. A package is directly affected
// when a changed file lies under its path or one of its declared dep
// paths. It is transitively affected when one of its dep paths lies
// under another (affected) package's path.
func DetectAffectedPackages(meta *PackageMeta, changedFiles []string) []string {
	if meta == nil || len(meta.Packages) == 0 || len(changedFiles) == 0 {
		return nil
	}

	// depEdges[B] = set of packages that depend on B (reverse edges).
	depEdges := buildReverseDependencyEdges(meta)

	direct := make(map[string]bool)
	for _, file := range changedFiles {
		file = filepath.Clean(file)
		for pkgName, pkg := range meta.Packages {
			if isPathAffected(file, pkg.Path) {
				direct[pkgName] = true
				continue
			}
			for _, dep := range pkg.Deps {
				if isPathAffected(file, dep) {
					direct[pkgName] = true
					break
				}
			}
		}
	}
	if len(direct) == 0 {
		return nil
	}

	// Reverse BFS: collect all packages that (transitively) depend on a
	// directly affected package. depEdges[X] is the set of packages
	// that depend on X.
	affected := make(map[string]bool)
	queue := make([]string, 0, len(direct))
	for name := range direct {
		affected[name] = true
		queue = append(queue, name)
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for dependent := range depEdges[current] {
			if affected[dependent] {
				continue
			}
			affected[dependent] = true
			queue = append(queue, dependent)
		}
	}

	result := make([]string, 0, len(affected))
	for name := range affected {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// buildReverseDependencyEdges maps each package to the set of packages
// that depend on it. A depends on B when one of A's dep paths lies
// under B's package path.
func buildReverseDependencyEdges(meta *PackageMeta) map[string]map[string]bool {
	edges := make(map[string]map[string]bool)
	for pkgName, pkg := range meta.Packages {
		for _, dep := range pkg.Deps {
			owner, ok := dependencyOwner(meta, dep, pkgName)
			if !ok {
				continue
			}
			if edges[owner] == nil {
				edges[owner] = make(map[string]bool)
			}
			edges[owner][pkgName] = true
		}
	}
	return edges
}

// dependencyOwner resolves which package a dep path binds to: the
// package whose own path is the longest path-prefix of the dep path.
func dependencyOwner(meta *PackageMeta, depPath, self string) (string, bool) {
	depPath = filepath.Clean(depPath)
	best := ""
	bestLen := -1
	for name, pkg := range meta.Packages {
		if name == self {
			continue
		}
		if cleanPath := filepath.Clean(pkg.Path); isPathAffected(depPath, cleanPath) && len(cleanPath) > bestLen {
			best, bestLen = name, len(cleanPath)
		}
	}
	return best, best != ""
}

func isPathAffected(file, prefix string) bool {
	prefix = filepath.Clean(prefix)
	file = filepath.Clean(file)

	if file == prefix {
		return true
	}
	if strings.HasPrefix(file, prefix+"/") {
		return true
	}
	return false
}

func ComputeChangedFiles(baseRef, headRef string) ([]string, error) {
	cmd := exec.Command("git", "diff", "--name-only", baseRef+".."+headRef)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, nil
	}
	files := strings.Split(raw, "\n")
	result := make([]string, 0, len(files))
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f != "" {
			result = append(result, f)
		}
	}
	return result, nil
}

func ComputeChangedFilesFromHead(n int) ([]string, error) {
	cmd := exec.Command("git", "diff", "--name-only", "HEAD~"+itoa(n))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, nil
	}
	files := strings.Split(raw, "\n")
	result := make([]string, 0, len(files))
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f != "" {
			result = append(result, f)
		}
	}
	return result, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

func ResolveAffectedBuildTargets(meta *PackageMeta, affected []string) []string {
	targets := make([]string, 0, len(affected))
	for _, name := range affected {
		if pkg, ok := meta.Packages[name]; ok && pkg.Build != "" {
			targets = append(targets, pkg.Build)
		}
	}
	return targets
}

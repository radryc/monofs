package pipeline

import (
	"os/exec"
	"path/filepath"
	"strings"
)

func DetectAffectedPackages(meta *PackageMeta, changedFiles []string) []string {
	affected := make(map[string]bool)

	for _, file := range changedFiles {
		file = filepath.Clean(file)
		for pkgName, pkg := range meta.Packages {
			if affected[pkgName] {
				continue
			}
			if isPathAffected(file, pkg.Path) {
				affected[pkgName] = true
				continue
			}
			for _, dep := range pkg.Deps {
				if isPathAffected(file, dep) {
					affected[pkgName] = true
					break
				}
			}
		}
	}

	result := make([]string, 0, len(affected))
	for name := range affected {
		result = append(result, name)
	}
	return result
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

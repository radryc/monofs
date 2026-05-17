package server

import "strings"

const (
	managedDoctorRoot    = "doctor"
	managedDoctorVersion = "v1"
)

var managedNamespaceChildren = map[string][]string{
	"":                {managedDoctorRoot},
	managedDoctorRoot: {managedDoctorVersion},
}

func normalizeManagedNamespacePath(path string) string {
	return strings.Trim(strings.TrimSpace(path), "/")
}

func managedNamespaceEntries(path string) []string {
	entries := managedNamespaceChildren[normalizeManagedNamespacePath(path)]
	if len(entries) == 0 {
		return nil
	}
	out := make([]string, len(entries))
	copy(out, entries)
	return out
}

func isManagedNamespaceDir(path string) bool {
	switch normalizeManagedNamespacePath(path) {
	case managedDoctorRoot, managedDoctorRoot + "/" + managedDoctorVersion:
		return true
	default:
		return false
	}
}

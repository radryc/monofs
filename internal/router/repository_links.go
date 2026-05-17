package router

import (
	"net/url"
	"path"
	"strings"
)

type repositoryUIBases struct {
	Guardian string
	Doctor   string
}

type repositoryProductLink struct {
	Kind  string
	Label string
	URL   string
}

func (r *Router) repositoryUIBases() repositoryUIBases {
	bases := repositoryUIBases{}

	r.guardianClientsMu.RLock()
	for _, state := range r.guardianClients {
		if state == nil {
			continue
		}
		recordRepositoryUIBase(&bases, state.role, state.baseURL)
	}
	r.guardianClientsMu.RUnlock()

	if r.guardianPrincipals == nil {
		return bases
	}

	r.guardianPrincipals.mu.RLock()
	for _, principal := range r.guardianPrincipals.principals {
		if principal == nil || principal.Disabled {
			continue
		}
		recordRepositoryUIBase(&bases, principal.Role, principal.BaseURL)
	}
	r.guardianPrincipals.mu.RUnlock()

	return bases
}

func recordRepositoryUIBase(bases *repositoryUIBases, role, rawBase string) {
	if bases == nil {
		return
	}
	base := strings.TrimRight(strings.TrimSpace(rawBase), "/")
	if !isHTTPURL(base) {
		return
	}
	switch strings.TrimSpace(role) {
	case "doctor":
		if bases.Doctor == "" {
			bases.Doctor = base
		}
	default:
		if bases.Guardian == "" {
			bases.Guardian = base
		}
	}
}

func buildRepositoryProductLink(displayPath, storedURL string, bases repositoryUIBases) repositoryProductLink {
	kind := repositoryProductKind(displayPath)
	if kind == "" {
		return repositoryProductLink{}
	}

	base := strings.TrimSpace(storedURL)
	if !isHTTPURL(base) {
		switch kind {
		case "guardian":
			base = bases.Guardian
		case "doctor":
			base = bases.Doctor
		}
	}
	if !isHTTPURL(base) {
		return repositoryProductLink{
			Kind:  kind,
			Label: repositoryProductLabel(kind),
		}
	}

	switch kind {
	case "guardian":
		partition := repositoryPartitionName(displayPath)
		if partition == "" {
			return repositoryProductLink{
				Kind:  kind,
				Label: repositoryProductLabel(kind),
				URL:   strings.TrimRight(base, "/"),
			}
		}
		return repositoryProductLink{
			Kind:  kind,
			Label: repositoryProductLabel(kind),
			URL:   guardianPartitionDeepLink(base, partition),
		}
	case "doctor":
		return repositoryProductLink{
			Kind:  kind,
			Label: repositoryProductLabel(kind),
			URL:   strings.TrimRight(base, "/"),
		}
	default:
		return repositoryProductLink{}
	}
}

func repositoryProductKind(displayPath string) string {
	switch {
	case displayPath == "guardian-system", strings.HasPrefix(displayPath, "guardian/"):
		return "guardian"
	case strings.HasPrefix(displayPath, "doctor/"):
		return "doctor"
	default:
		return ""
	}
}

func repositoryProductLabel(kind string) string {
	switch kind {
	case "guardian":
		return "Guardian UI"
	case "doctor":
		return "Doctor UI"
	default:
		return ""
	}
}

func repositoryPartitionName(displayPath string) string {
	if !strings.HasPrefix(displayPath, "guardian/") {
		return ""
	}
	partition := strings.TrimPrefix(displayPath, "guardian/")
	if idx := strings.Index(partition, "/"); idx >= 0 {
		partition = partition[:idx]
	}
	return strings.TrimSpace(partition)
}

func guardianPartitionDeepLink(rawBase, partition string) string {
	parsed, err := url.Parse(rawBase)
	if err != nil {
		return ""
	}
	cleanPath := strings.TrimRight(parsed.Path, "/")
	if cleanPath != "" && cleanPath != "/" && path.Base(cleanPath) == partition {
		dir := path.Dir(cleanPath)
		if dir == "." || dir == "/" {
			parsed.Path = ""
		} else {
			parsed.Path = dir
		}
	}
	values := parsed.Query()
	values.Set("partition", partition)
	parsed.RawQuery = values.Encode()
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/")
}

func isHTTPURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

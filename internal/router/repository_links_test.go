package router

import "testing"

func TestBuildRepositoryProductLinkGuardianDeepLink(t *testing.T) {
	t.Parallel()

	link := buildRepositoryProductLink("guardian/demo", "http://guardian.example/demo", repositoryUIBases{})
	if link.Kind != "guardian" {
		t.Fatalf("kind = %q, want guardian", link.Kind)
	}
	if link.URL != "http://guardian.example?partition=demo" {
		t.Fatalf("url = %q", link.URL)
	}
}

func TestBuildRepositoryProductLinkGuardianFallsBackToConnectedBase(t *testing.T) {
	t.Parallel()

	link := buildRepositoryProductLink("guardian/demo", "/demo", repositoryUIBases{
		Guardian: "http://guardian.example/ui",
	})
	if link.URL != "http://guardian.example/ui?partition=demo" {
		t.Fatalf("url = %q", link.URL)
	}
}

func TestBuildRepositoryProductLinkDoctorUsesProductBase(t *testing.T) {
	t.Parallel()

	link := buildRepositoryProductLink("doctor/v1", "doctor/v1", repositoryUIBases{
		Doctor: "http://doctor.example/ui",
	})
	if link.Kind != "doctor" {
		t.Fatalf("kind = %q, want doctor", link.Kind)
	}
	if link.URL != "http://doctor.example/ui" {
		t.Fatalf("url = %q", link.URL)
	}
}

func TestBuildRepositoryProductLinkIgnoresUnmanagedRepos(t *testing.T) {
	t.Parallel()

	link := buildRepositoryProductLink("github.com/acme/repo", "https://github.com/acme/repo", repositoryUIBases{})
	if link.Kind != "" || link.URL != "" {
		t.Fatalf("unexpected link = %+v", link)
	}
}

func TestRepositoryProductStoredURLFallsBackToGuardianSource(t *testing.T) {
	t.Parallel()

	got := repositoryProductStoredURL("guardian/demo", "", "http://localhost:8090")
	if got != "http://localhost:8090" {
		t.Fatalf("stored url = %q", got)
	}
}

func TestRepositoryProductStoredURLIgnoresUnmanagedSource(t *testing.T) {
	t.Parallel()

	got := repositoryProductStoredURL("github.com/acme/repo", "", "https://github.com/acme/repo")
	if got != "" {
		t.Fatalf("stored url = %q, want empty", got)
	}
}

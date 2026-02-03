package main

import (
	"testing"
)

func TestParseGitHubURL(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantRepo   string
		wantBranch string
		wantErr    bool
	}{
		{
			name:       "URL with tree/branch",
			input:      "https://github.com/radryc/prompusher/tree/main",
			wantRepo:   "https://github.com/radryc/prompusher.git",
			wantBranch: "main",
			wantErr:    false,
		},
		{
			name:       "URL with tree/develop",
			input:      "https://github.com/owner/repo/tree/develop",
			wantRepo:   "https://github.com/owner/repo.git",
			wantBranch: "develop",
			wantErr:    false,
		},
		{
			name:       "URL with tree and slash in branch",
			input:      "https://github.com/owner/repo/tree/feature/new-feature",
			wantRepo:   "https://github.com/owner/repo.git",
			wantBranch: "feature/new-feature",
			wantErr:    false,
		},
		{
			name:       "URL without branch defaults to main",
			input:      "https://github.com/owner/repo",
			wantRepo:   "https://github.com/owner/repo.git",
			wantBranch: "main",
			wantErr:    false,
		},
		{
			name:       "URL with .git suffix",
			input:      "https://github.com/owner/repo.git",
			wantRepo:   "https://github.com/owner/repo.git",
			wantBranch: "main",
			wantErr:    false,
		},
		{
			name:       "URL with .git and tree/branch",
			input:      "https://github.com/owner/repo.git/tree/master",
			wantRepo:   "https://github.com/owner/repo.git",
			wantBranch: "master",
			wantErr:    false,
		},
		{
			name:    "Invalid URL - no owner/repo",
			input:   "https://github.com/",
			wantErr: true,
		},
		{
			name:    "Invalid URL - malformed",
			input:   "not-a-url",
			wantErr: true,
		},
		{
			name:       "GitLab URL with tree/branch",
			input:      "https://gitlab.com/owner/project/tree/main",
			wantRepo:   "https://gitlab.com/owner/project.git",
			wantBranch: "main",
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRepo, gotBranch, err := parseGitHubURL(tt.input)

			if (err != nil) != tt.wantErr {
				t.Errorf("parseGitHubURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil {
				return
			}

			if gotRepo != tt.wantRepo {
				t.Errorf("parseGitHubURL() repo = %v, want %v", gotRepo, tt.wantRepo)
			}

			if gotBranch != tt.wantBranch {
				t.Errorf("parseGitHubURL() branch = %v, want %v", gotBranch, tt.wantBranch)
			}
		})
	}
}

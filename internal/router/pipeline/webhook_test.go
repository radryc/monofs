package pipeline

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubPushEvent(t *testing.T) {
	handler := NewWebhookHandler(nil, WebhookConfig{}, "testdata/packages.yaml")
	h := &WebhookHandler{orchestrator: nil, configs: handler.configs, metaPath: handler.metaPath}

	event := gitHubPushEvent{
		Ref:   "refs/heads/main",
		After: "abc123def456",
		Sender: struct {
			Login string `json:"login"`
		}{Login: "devuser"},
		Repository: struct {
			FullName string `json:"full_name"`
			HTMLURL  string `json:"html_url"`
		}{FullName: "org/repo", HTMLURL: "https://github.com/org/repo"},
		Commits: []struct {
			ID       string   `json:"id"`
			Added    []string `json:"added"`
			Removed  []string `json:"removed"`
			Modified []string `json:"modified"`
		}{
			{ID: "abc123", Added: []string{"src/main.go"}, Removed: []string{}, Modified: []string{"go.mod"}},
		},
	}

	body, _ := json.Marshal(event)
	evt := h.parseGitHubPush(body)

	if evt.EventType != "" {
		t.Errorf("parsed event should not have EventType set directly")
	}
	if evt.Branch != "main" {
		t.Errorf("branch = %q, want main", evt.Branch)
	}
	if evt.CommitSHA != "abc123def456" {
		t.Errorf("commit = %q, want abc123def456", evt.CommitSHA)
	}
	if evt.Sender != "devuser" {
		t.Errorf("sender = %q, want devuser", evt.Sender)
	}
	if len(evt.ChangedFiles) != 2 {
		t.Errorf("changed files = %d, want 2", len(evt.ChangedFiles))
	}
}

func TestGitHubPullRequestEvent(t *testing.T) {
	h := &WebhookHandler{}

	event := gitHubPullRequestEvent{
		Action: "opened",
		Number: 42,
		PullRequest: struct {
			Head struct {
				SHA string `json:"sha"`
				Ref string `json:"ref"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		}{
			Head: struct {
				SHA string `json:"sha"`
				Ref string `json:"ref"`
			}{SHA: "feature-sha", Ref: "feature/awesome"},
		},
		Sender: struct {
			Login string `json:"login"`
		}{Login: "coder"},
		Repository: struct {
			FullName string `json:"full_name"`
			HTMLURL  string `json:"html_url"`
		}{FullName: "org/repo", HTMLURL: "https://github.com/org/repo"},
	}

	body, _ := json.Marshal(event)
	evt := h.parseGitHubPullRequest(body)

	if evt.EventType != "" {
		t.Errorf("parsed event should not have EventType set directly")
	}
	if evt.CommitSHA != "feature-sha" {
		t.Errorf("commit = %q, want feature-sha", evt.CommitSHA)
	}
	if evt.PRNumber != 42 {
		t.Errorf("pr = %d, want 42", evt.PRNumber)
	}
	if evt.Branch != "feature/awesome" {
		t.Errorf("branch = %q, want feature/awesome", evt.Branch)
	}
}

func TestGitHubPROpenedOnly(t *testing.T) {
	h := &WebhookHandler{}

	closed := gitHubPullRequestEvent{Action: "closed"}
	body, _ := json.Marshal(closed)
	evt := h.parseGitHubPullRequest(body)

	if evt.CommitSHA != "" {
		t.Error("closed PR should return empty event")
	}
}

func TestGitHubCreateTagEvent(t *testing.T) {
	h := &WebhookHandler{}

	event := gitHubCreateEvent{
		RefType: "tag",
		Ref:     "v1.0.0",
		Sender: struct {
			Login string `json:"login"`
		}{Login: "releaser"},
		Repository: struct {
			FullName string `json:"full_name"`
			HTMLURL  string `json:"html_url"`
		}{FullName: "org/repo", HTMLURL: "https://github.com/org/repo"},
	}

	body, _ := json.Marshal(event)
	evt := h.parseGitHubCreate(body)

	if evt.EventType != "" {
		t.Errorf("parsed event should not have EventType set directly")
	}
	if evt.Tag != "v1.0.0" {
		t.Errorf("tag = %q, want v1.0.0", evt.Tag)
	}
}

func TestGitHubCreateBranchIgnored(t *testing.T) {
	h := &WebhookHandler{}

	branch := gitHubCreateEvent{RefType: "branch", Ref: "feature/x"}
	body, _ := json.Marshal(branch)
	evt := h.parseGitHubCreate(body)

	if evt.Tag != "" {
		t.Error("branch create event should return empty event")
	}
}

func TestGitLabPushEvent(t *testing.T) {
	h := &WebhookHandler{}

	event := gitLabPushEvent{
		Ref:         "refs/heads/main",
		CheckoutSHA: "abc123def789",
		UserName:    "devops",
		Project: struct {
			WebURL string `json:"web_url"`
		}{WebURL: "https://gitlab.com/org/repo"},
		Commits: []struct {
			ID       string   `json:"id"`
			Added    []string `json:"added"`
			Removed  []string `json:"removed"`
			Modified []string `json:"modified"`
		}{
			{ID: "c1", Added: []string{"new.txt"}, Removed: nil, Modified: []string{"old.txt"}},
		},
	}

	body, _ := json.Marshal(event)
	evt := h.parseGitLabPush(body)

	if evt.CommitSHA != "abc123def789" {
		t.Errorf("commit = %q, want abc123def789", evt.CommitSHA)
	}
	if evt.Branch != "main" {
		t.Errorf("branch = %q, want main", evt.Branch)
	}
	if evt.Sender != "devops" {
		t.Errorf("sender = %q, want devops", evt.Sender)
	}
	if len(evt.ChangedFiles) != 2 {
		t.Errorf("files = %d, want 2", len(evt.ChangedFiles))
	}
}

func TestGitLabMergeRequestEvent(t *testing.T) {
	h := &WebhookHandler{}

	event := gitLabMergeRequestEvent{
		ObjectAttributes: struct {
			IID        int    `json:"iid"`
			Action     string `json:"action"`
			LastCommit struct {
				ID string `json:"id"`
			} `json:"last_commit"`
			SourceBranch string `json:"source_branch"`
		}{
			IID:    99,
			Action: "open",
			LastCommit: struct {
				ID string `json:"id"`
			}{ID: "merge-sha"},
			SourceBranch: "feature/fast",
		},
		UserName: "gitlab-user",
		Project: struct {
			WebURL string `json:"web_url"`
		}{WebURL: "https://gitlab.com/org/repo"},
	}

	body, _ := json.Marshal(event)
	evt := h.parseGitLabMergeRequest(body)

	if evt.CommitSHA != "merge-sha" {
		t.Errorf("commit = %q, want merge-sha", evt.CommitSHA)
	}
	if evt.PRNumber != 99 {
		t.Errorf("pr = %d, want 99", evt.PRNumber)
	}
	if evt.Branch != "feature/fast" {
		t.Errorf("branch = %q, want feature/fast", evt.Branch)
	}
}

func TestGitHubSignatureVerification(t *testing.T) {
	secret := "test-secret"
	h := &WebhookHandler{webhookCfg: WebhookConfig{GitHubSecret: secret}}

	body := []byte(`{"test":"payload"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	r := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", sig)

	if !h.verifyGitHubSignature(sig, r) {
		t.Fatal("valid signature should verify")
	}

	r2 := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
	r2.Header.Set("X-Hub-Signature-256", "sha256=badsignature")

	if h.verifyGitHubSignature("sha256=badsignature", r2) {
		t.Fatal("invalid signature should not verify")
	}

	if h.verifyGitHubSignature("", r2) {
		t.Fatal("empty signature should not verify")
	}
}

func TestDedupeStrings(t *testing.T) {
	tests := []struct {
		name   string
		input  []string
		expect int
	}{
		{"no dups", []string{"a", "b", "c"}, 3},
		{"with dups", []string{"a", "b", "a", "c", "b"}, 3},
		{"empty", []string{}, 0},
		{"single", []string{"x"}, 1},
		{"all same", []string{"x", "x", "x"}, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := dedupeStrings(tt.input)
			if len(result) != tt.expect {
				t.Errorf("dedupe got %d, want %d: %v", len(result), tt.expect, result)
			}
		})
	}
}

func TestWebhookServeHTTPNoEvent(t *testing.T) {
	handler := &WebhookHandler{}

	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown webhook, got %d", w.Code)
	}
}

func TestWebhookServeHTTPGet(t *testing.T) {
	handler := &WebhookHandler{}

	req := httptest.NewRequest("GET", "/webhook", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET, got %d", w.Code)
	}
}

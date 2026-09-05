package pipeline

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

type WebhookConfig struct {
	GitHubSecret string
	GitLabSecret string
}

type WebhookHandler struct {
	orchestrator *Orchestrator
	configs      map[string]*PipelineConfig
	webhookCfg   WebhookConfig
	metaPath     string
}

func NewWebhookHandler(orchestrator *Orchestrator, cfg WebhookConfig, metaPath string) *WebhookHandler {
	return &WebhookHandler{
		orchestrator: orchestrator,
		configs:      make(map[string]*PipelineConfig),
		webhookCfg:   cfg,
		metaPath:     metaPath,
	}
}

func (h *WebhookHandler) RegisterPipeline(cfg *PipelineConfig) {
	h.configs[cfg.Name] = cfg
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	if eventType != "" {
		h.handleGitHub(w, r, eventType)
		return
	}

	eventType = r.Header.Get("X-Gitlab-Event")
	if eventType != "" {
		h.handleGitLab(w, r, eventType)
		return
	}

	http.Error(w, "unknown webhook source", http.StatusBadRequest)
}

func (h *WebhookHandler) handleGitHub(w http.ResponseWriter, r *http.Request, eventType string) {
	if h.webhookCfg.GitHubSecret != "" {
		sig := r.Header.Get("X-Hub-Signature-256")
		if !h.verifyGitHubSignature(sig, r) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}

	var event WebhookEvent

	switch eventType {
	case "push":
		event = h.parseGitHubPush(body)
		event.EventType = TriggerPush
	case "pull_request":
		event = h.parseGitHubPullRequest(body)
		event.EventType = TriggerPullRequest
	case "create":
		event = h.parseGitHubCreate(body)
	default:
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message": "event type not supported"}`))
		return
	}

	if event.CommitSHA == "" {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message": "no commit to process"}`))
		return
	}

	go h.processEvent(event)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"message": "accepted"}`))
}

func (h *WebhookHandler) handleGitLab(w http.ResponseWriter, r *http.Request, eventType string) {
	if h.webhookCfg.GitLabSecret != "" {
		token := r.Header.Get("X-Gitlab-Token")
		if token != h.webhookCfg.GitLabSecret {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}

	var event WebhookEvent

	switch eventType {
	case "Push Hook":
		event = h.parseGitLabPush(body)
		event.EventType = TriggerPush
	case "Merge Request Hook":
		event = h.parseGitLabMergeRequest(body)
		event.EventType = TriggerPullRequest
	case "Tag Push Hook":
		event = h.parseGitLabTagPush(body)
		event.EventType = TriggerTag
	default:
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message": "event type not supported"}`))
		return
	}

	if event.CommitSHA == "" {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message": "no commit to process"}`))
		return
	}

	go h.processEvent(event)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"message": "accepted"}`))
}

func (h *WebhookHandler) processEvent(event WebhookEvent) {
	meta, err := LoadPackageMeta(nil, h.metaPath)
	if err != nil {
		return
	}

	if len(event.ChangedFiles) == 0 {
		event.ChangedFiles, _ = ComputeChangedFilesFromHead(1)
	}
	affected := DetectAffectedPackages(meta, event.ChangedFiles)

	for _, cfg := range h.configs {
		if cfg.MatchEvent(event) {
			h.orchestrator.StartRun(cfg, event, affected)
		}
	}
}

func (h *WebhookHandler) verifyGitHubSignature(sigHeader string, r *http.Request) bool {
	if sigHeader == "" || !strings.HasPrefix(sigHeader, "sha256=") {
		return false
	}
	sigBytes, err := hex.DecodeString(sigHeader[7:])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(h.webhookCfg.GitHubSecret))
	body, _ := io.ReadAll(r.Body)
	mac.Write(body)
	expected := mac.Sum(nil)
	return hmac.Equal(sigBytes, expected)
}

func (h *WebhookHandler) verifyGitLabSignature(token string) bool {
	return token == h.webhookCfg.GitLabSecret
}

type gitHubPushEvent struct {
	Ref    string `json:"ref"`
	After  string `json:"after"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
	Repository struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
	} `json:"repository"`
	Commits []struct {
		ID       string   `json:"id"`
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	} `json:"commits"`
}

func (h *WebhookHandler) parseGitHubPush(body []byte) WebhookEvent {
	var evt gitHubPushEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		return WebhookEvent{}
	}

	branch := strings.TrimPrefix(evt.Ref, "refs/heads/")

	var files []string
	for _, c := range evt.Commits {
		files = append(files, c.Added...)
		files = append(files, c.Removed...)
		files = append(files, c.Modified...)
	}

	return WebhookEvent{
		CommitSHA:    evt.After,
		Branch:       branch,
		RepoURL:      evt.Repository.HTMLURL,
		Sender:       evt.Sender.Login,
		ChangedFiles: dedupeStrings(files),
	}
}

type gitHubPullRequestEvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Head struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
	Repository struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
	} `json:"repository"`
}

func (h *WebhookHandler) parseGitHubPullRequest(body []byte) WebhookEvent {
	var evt gitHubPullRequestEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		return WebhookEvent{}
	}

	if evt.Action != "opened" && evt.Action != "synchronize" && evt.Action != "reopened" {
		return WebhookEvent{}
	}

	files, _ := ComputeChangedFiles("HEAD~1", "HEAD")

	return WebhookEvent{
		CommitSHA:    evt.PullRequest.Head.SHA,
		Branch:       evt.PullRequest.Head.Ref,
		PRNumber:     evt.Number,
		RepoURL:      evt.Repository.HTMLURL,
		Sender:       evt.Sender.Login,
		ChangedFiles: files,
	}
}

type gitHubCreateEvent struct {
	RefType string `json:"ref_type"`
	Ref     string `json:"ref"`
	Sender  struct {
		Login string `json:"login"`
	} `json:"sender"`
	Repository struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
	} `json:"repository"`
}

func (h *WebhookHandler) parseGitHubCreate(body []byte) WebhookEvent {
	var evt gitHubCreateEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		return WebhookEvent{}
	}

	if evt.RefType != "tag" {
		return WebhookEvent{}
	}

	return WebhookEvent{
		Tag:     evt.Ref,
		RepoURL: evt.Repository.HTMLURL,
		Sender:  evt.Sender.Login,
	}
}

type gitLabPushEvent struct {
	Ref         string `json:"ref"`
	CheckoutSHA string `json:"checkout_sha"`
	UserName    string `json:"user_username"`
	Project     struct {
		WebURL string `json:"web_url"`
	} `json:"project"`
	Commits []struct {
		ID       string   `json:"id"`
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	} `json:"commits"`
}

func (h *WebhookHandler) parseGitLabPush(body []byte) WebhookEvent {
	var evt gitLabPushEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		return WebhookEvent{}
	}

	branch := strings.TrimPrefix(evt.Ref, "refs/heads/")

	var files []string
	for _, c := range evt.Commits {
		files = append(files, c.Added...)
		files = append(files, c.Removed...)
		files = append(files, c.Modified...)
	}

	return WebhookEvent{
		CommitSHA:    evt.CheckoutSHA,
		Branch:       branch,
		RepoURL:      evt.Project.WebURL,
		Sender:       evt.UserName,
		ChangedFiles: dedupeStrings(files),
	}
}

type gitLabMergeRequestEvent struct {
	ObjectAttributes struct {
		IID        int    `json:"iid"`
		Action     string `json:"action"`
		LastCommit struct {
			ID string `json:"id"`
		} `json:"last_commit"`
		SourceBranch string `json:"source_branch"`
	} `json:"object_attributes"`
	UserName string `json:"user_username"`
	Project  struct {
		WebURL string `json:"web_url"`
	} `json:"project"`
}

func (h *WebhookHandler) parseGitLabMergeRequest(body []byte) WebhookEvent {
	var evt gitLabMergeRequestEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		return WebhookEvent{}
	}

	if evt.ObjectAttributes.Action != "open" && evt.ObjectAttributes.Action != "update" && evt.ObjectAttributes.Action != "reopen" {
		return WebhookEvent{}
	}

	return WebhookEvent{
		CommitSHA: evt.ObjectAttributes.LastCommit.ID,
		Branch:    evt.ObjectAttributes.SourceBranch,
		PRNumber:  evt.ObjectAttributes.IID,
		RepoURL:   evt.Project.WebURL,
		Sender:    evt.UserName,
	}
}

type gitLabTagPushEvent struct {
	Ref      string `json:"ref"`
	UserName string `json:"user_username"`
	Project  struct {
		WebURL string `json:"web_url"`
	} `json:"project"`
}

func (h *WebhookHandler) parseGitLabTagPush(body []byte) WebhookEvent {
	var evt gitLabTagPushEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		return WebhookEvent{}
	}

	tag := strings.TrimPrefix(evt.Ref, "refs/tags/")

	return WebhookEvent{
		Tag:     tag,
		RepoURL: evt.Project.WebURL,
		Sender:  evt.UserName,
	}
}

func dedupeStrings(s []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, item := range s {
		if !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}

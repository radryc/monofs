package pipeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

type StatusReporter struct {
	githubToken string
	gitlabToken string
}

func NewStatusReporter(githubToken, gitlabToken string) *StatusReporter {
	return &StatusReporter{
		githubToken: githubToken,
		gitlabToken: gitlabToken,
	}
}

type CommitStatus struct {
	State       string
	Description string
	TargetURL   string
	Context     string
}

func (r *StatusReporter) ReportGitHub(repoFullName, sha string, status CommitStatus) error {
	if r.githubToken == "" {
		return nil
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/statuses/%s", repoFullName, sha)

	body := map[string]string{
		"state":       status.State,
		"description": status.Description,
		"target_url":  status.TargetURL,
		"context":     status.Context,
	}
	payload, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+r.githubToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("github status API returned %d", resp.StatusCode)
	}
	return nil
}

func (r *StatusReporter) ReportGitLab(projectID, sha string, status CommitStatus) error {
	if r.gitlabToken == "" {
		return nil
	}

	url := fmt.Sprintf("https://gitlab.com/api/v4/projects/%s/statuses/%s", projectID, sha)

	body := map[string]string{
		"state":       status.State,
		"description": status.Description,
		"target_url":  status.TargetURL,
		"name":        status.Context,
	}
	payload, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", r.gitlabToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("gitlab status API returned %d", resp.StatusCode)
	}
	return nil
}

func JobStateToCommitState(state JobState) string {
	switch state {
	case JobPending, JobClaimed, JobRunning:
		return "pending"
	case JobSucceeded:
		return "success"
	case JobFailed, JobCancelled:
		return "failure"
	default:
		return "error"
	}
}

func RunStateToCommitState(state RunState) string {
	switch state {
	case RunPending, RunRunning:
		return "pending"
	case RunSucceeded:
		return "success"
	case RunFailed, RunCancelled:
		return "failure"
	default:
		return "error"
	}
}

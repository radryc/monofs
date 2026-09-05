package router

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/radryc/monofs/internal/router/pipeline"
)

func (r *Router) handlePipelinesAPI(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.pipelineOrchestrator == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"pipelines": []interface{}{},
		})
		return
	}

	path := strings.TrimPrefix(req.URL.Path, "/api/pipelines")
	path = strings.TrimPrefix(path, "/")
	parts := strings.Split(path, "/")

	switch {
	case path == "" || (len(parts) == 1 && parts[0] == ""):
		if req.Method == http.MethodGet {
			r.handlePipelineList(w, req)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)

	case len(parts) >= 3 && parts[1] == "runs":
		pipelineName := parts[0]
		if len(parts) == 3 && parts[2] == "runs" {
			r.handlePipelineRunsList(w, req, pipelineName)
			return
		}
		runID := parts[2]
		if len(parts) == 4 && parts[3] == "cancel" {
			r.handlePipelineRunCancel(w, req, runID)
			return
		}
		r.handlePipelineRunDetail(w, req, runID)

	case len(parts) == 1 && parts[0] != "" && parts[0] != "stats" && parts[0] != "register":
		if req.Method == http.MethodDelete {
			r.handlePipelineUnregister(w, req, parts[0])
			return
		}

	case len(parts) == 2 && parts[1] == "run":
		if req.Method == http.MethodPost {
			r.handlePipelineRunTrigger(w, req, parts[0])
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)

	case path == "stats":
		r.handlePipelineStats(w, req)

	default:
		http.NotFound(w, req)
	}
}

func (r *Router) handlePipelineList(w http.ResponseWriter, req *http.Request) {
	cfgs := r.pipelineOrchestrator.PipelineConfigs()
	pipelines := make([]PipelineSummary, 0, len(cfgs))

	for name := range cfgs {
		summary := PipelineSummary{
			Name:         name,
			LastRunState: "unknown",
		}
		runs := r.pipelineOrchestrator.ListRuns(name, 1)
		if len(runs) > 0 {
			summary.LastRunState = string(runs[0].State)
			summary.LastRunID = runs[0].RunID
		}
		summary.RunCount = len(r.pipelineOrchestrator.ListRuns(name, 100))
		pipelines = append(pipelines, summary)
	}

	json.NewEncoder(w).Encode(PipelineListData{Pipelines: pipelines})
}

func (r *Router) handlePipelineRunsList(w http.ResponseWriter, req *http.Request, pipelineName string) {
	runs := r.pipelineOrchestrator.ListRuns(pipelineName, 20)
	data := make([]PipelineRunData, 0, len(runs))
	for _, run := range runs {
		data = append(data, pipelineRunToData(run))
	}
	json.NewEncoder(w).Encode(PipelineRunsData{Runs: data})
}

func (r *Router) handlePipelineRunDetail(w http.ResponseWriter, req *http.Request, runID string) {
	run, err := r.pipelineOrchestrator.GetRun(runID)
	if err != nil {
		http.Error(w, "Run not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(pipelineRunToData(run))
}

func (r *Router) handlePipelineRunCancel(w http.ResponseWriter, req *http.Request, runID string) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.pipelineOrchestrator.CancelRun(runID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "cancelled"})
}

func (r *Router) handlePipelineRunTrigger(w http.ResponseWriter, req *http.Request, pipelineName string) {
	cfgs := r.pipelineOrchestrator.PipelineConfigs()
	cfg, ok := cfgs[pipelineName]
	if !ok {
		http.Error(w, "Pipeline not found", http.StatusNotFound)
		return
	}

	var body struct {
		Branch string `json:"branch"`
		Commit string `json:"commit"`
	}
	json.NewDecoder(req.Body).Decode(&body)

	if body.Branch == "" {
		body.Branch = "main"
	}
	if body.Commit == "" {
		body.Commit = "HEAD"
	}

	run, err := r.pipelineOrchestrator.StartRun(cfg, pipeline.WebhookEvent{
		EventType: pipeline.TriggerManual,
		CommitSHA: body.Commit,
		Branch:    body.Branch,
	}, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(pipelineRunToData(run))
}

func (r *Router) handlePipelineStats(w http.ResponseWriter, req *http.Request) {
	stats := r.pipelineOrchestrator.GetStats("")
	json.NewEncoder(w).Encode(PipelineStatsData{
		TotalRuns:     stats.TotalRuns,
		SucceededRuns: stats.SucceededRuns,
		FailedRuns:    stats.FailedRuns,
		SuccessRate:   stats.SuccessRate,
		AvgDurationMs: stats.AvgDurationMs,
		P50DurationMs: stats.P50DurationMs,
		P95DurationMs: stats.P95DurationMs,
	})
}

func (r *Router) handleGitHubWebhook(w http.ResponseWriter, req *http.Request) {
	if r.pipelineWebhookHandler == nil {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message": "webhooks not configured"}`))
		return
	}
	r.pipelineWebhookHandler.ServeHTTP(w, req)
}

func (r *Router) handleGitLabWebhook(w http.ResponseWriter, req *http.Request) {
	if r.pipelineWebhookHandler == nil {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message": "webhooks not configured"}`))
		return
	}
	r.pipelineWebhookHandler.ServeHTTP(w, req)
}

func (r *Router) handlePipelineUnregister(w http.ResponseWriter, req *http.Request, name string) {
	if req.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.pipelineOrchestrator.UnregisterPipeline(name)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message": "pipeline unregistered: " + name,
	})
}

func pipelineRunToData(run *pipeline.PipelineRun) PipelineRunData {
	data := PipelineRunData{
		RunID:        run.RunID,
		PipelineName: run.PipelineName,
		State:        string(run.State),
		Trigger:      string(run.Trigger),
		CommitSHA:    run.CommitSHA,
		Branch:       run.Branch,
		Tag:          run.Tag,
		PRNumber:     run.PRNumber,
		CreatedAt:    run.CreatedAt.Format("2006-01-02T15:04:05Z"),
		Affected:     run.Affected,
	}
	if run.StartedAt != nil {
		data.StartedAt = run.StartedAt.Format("2006-01-02T15:04:05Z")
	}
	if run.FinishedAt != nil {
		data.FinishedAt = run.FinishedAt.Format("2006-01-02T15:04:05Z")
	}
	for _, job := range run.Jobs {
		jd := JobStatusData{
			JobName:    job.JobName,
			State:      string(job.State),
			Needs:      job.Needs,
			WorkerID:   job.WorkerID,
			Retries:    job.Retries,
			MaxRetries: job.MaxRetries,
			Error:      job.Error,
			ExitCode:   job.ExitCode,
		}
		if job.StartedAt != nil {
			jd.StartedAt = job.StartedAt.Format("2006-01-02T15:04:05Z")
		}
		if job.FinishedAt != nil {
			jd.FinishedAt = job.FinishedAt.Format("2006-01-02T15:04:05Z")
		}
		if job.StartedAt != nil && job.FinishedAt != nil {
			jd.DurationMs = job.FinishedAt.Sub(*job.StartedAt).Milliseconds()
		}
		data.Jobs = append(data.Jobs, jd)
	}
	return data
}

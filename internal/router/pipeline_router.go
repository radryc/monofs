package router

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/router/pipeline"
)

const pipelinePrincipalID = "monofs-pipeline"
const pipelinePrincipalToken = "monofs-pipeline-internal-token"
const pipelineConfigsPrefix = "/.pipelines"
const pipelineQueuesPrefix = "/.queues/pipeline"
const pipelineResultsMarker = "/.results/"
const pipelinePackagesMetaPath = "/monofs-packages.yaml"

func (r *Router) initPipeline(logger *slog.Logger) {
	r.guardianPrincipals.upsertConnectedClient(
		pipelinePrincipalID,
		pipelinePrincipalToken,
		"pipeline",
		"MonoFS Pipeline Orchestrator",
		"",
	)

	kvsClient := &routerKVSClient{router: r}
	queue := pipeline.NewTaskQueue(kvsClient)
	orch := pipeline.NewOrchestrator(queue, logger.With("component", "pipeline"))
	orch.SetOnRunFinished(func(run *pipeline.PipelineRun) {
		r.onPipelineRunFinished(run, logger)
	})
	r.pipelineOrchestrator = orch
	r.pipelineTaskQueue = queue

	wh := pipeline.NewWebhookHandler(
		orch,
		pipeline.WebhookConfig{
			GitHubSecret: os.Getenv("MONOFS_GITHUB_WEBHOOK_SECRET"),
			GitLabSecret: os.Getenv("MONOFS_GITLAB_WEBHOOK_SECRET"),
		},
		"monofs-packages.yaml",
	)
	wh.SetMetaLoader(func() ([]byte, error) {
		content, _, err := r.readPipelinePath(pipelinePackagesMetaPath)
		return content, err
	})
	// Webhook pushes also signal upstream repository changes for the
	// auto-refresh re-ingest path.
	wh.SetRepoChangedHandler(func(repoURL, branch, sha string) {
		r.enqueueUpstreamRefresh(repoURL)
	})
	r.pipelineWebhookHandler = wh

	r.pipelineStatusReporter = pipeline.NewStatusReporter(
		os.Getenv("MONOFS_GITHUB_TOKEN"),
		os.Getenv("MONOFS_GITLAB_TOKEN"),
	)

	if err := r.loadPipelinesFromKVS(logger); err != nil {
		logger.Warn("initial pipeline load failed", "error", err)
	}

	go r.watchPipelinesFromKVS(logger)
	go r.watchPipelineResults(logger)

	logger.Info("pipeline orchestrator initialized — configs read from /.pipelines/")
}

// watchPipelineResults advances the pipeline DAG as workers publish task
// results. Workers write results into /.queues/pipeline/<run>/.results/,
// which fans out as guardian change events with inline content.
func (r *Router) watchPipelineResults(logger *slog.Logger) {
	logger.Info("watching for pipeline task results", "prefix", pipelineQueuesPrefix)

	sub, id := r.subscribeGuardianLogicalChanges([]string{pipelineQueuesPrefix}, true)
	defer r.unsubscribeGuardianLogicalChanges(id)

	for {
		select {
		case event, ok := <-sub:
			if !ok {
				return
			}
			r.handlePipelineResultEvent(event, logger)
		case <-r.stopUI:
			return
		}
	}
}

func (r *Router) handlePipelineResultEvent(event *pb.GuardianChangeEvent, logger *slog.Logger) {
	if event == nil {
		return
	}
	switch event.GetType() {
	case pb.ChangeType_ADDED, pb.ChangeType_MODIFIED:
	default:
		return
	}

	logicalPath := event.GetLogicalPath()
	if !strings.Contains(logicalPath, pipelineResultsMarker) {
		return
	}

	content := event.GetInlineContent()
	if len(content) == 0 {
		var err error
		content, _, err = r.readPipelinePath(logicalPath)
		if err != nil {
			logger.Warn("pipeline result event has no readable content",
				"path", logicalPath, "error", err)
			return
		}
	}

	var result pipeline.TaskResult
	if err := json.Unmarshal(content, &result); err != nil {
		logger.Warn("unmarshal pipeline task result failed",
			"path", logicalPath, "error", err)
		return
	}
	if result.RunID == "" || result.JobName == "" {
		return
	}

	logger.Debug("pipeline task result received",
		"run_id", result.RunID, "job", result.JobName, "state", result.State)

	r.pipelineOrchestrator.OnTaskResult(context.Background(), &result)
}

// onPipelineRunFinished cleans up the finished run's queue state and
// reports the outcome as a commit status to the originating forge.
func (r *Router) onPipelineRunFinished(run *pipeline.PipelineRun, logger *slog.Logger) {
	if run == nil {
		return
	}

	if r.pipelineTaskQueue != nil {
		if err := r.pipelineTaskQueue.CleanupRun(run.RunID); err != nil {
			logger.Warn("pipeline queue cleanup failed", "run_id", run.RunID, "error", err)
		}
	}

	r.reportPipelineStatus(run, logger)
}

func (r *Router) reportPipelineStatus(run *pipeline.PipelineRun, logger *slog.Logger) {
	if r.pipelineStatusReporter == nil {
		return
	}
	if run.RepoFullName == "" || run.CommitSHA == "" {
		return
	}

	status := pipeline.CommitStatus{
		State:       pipeline.RunStateToCommitState(run.State),
		Description: "MonoFS pipeline " + run.PipelineName + ": " + string(run.State),
		Context:     "monofs/" + run.PipelineName,
	}

	var err error
	if strings.Contains(strings.ToLower(run.RepoURL), "gitlab") {
		err = r.pipelineStatusReporter.ReportGitLab(url.PathEscape(run.RepoFullName), run.CommitSHA, status)
	} else {
		err = r.pipelineStatusReporter.ReportGitHub(run.RepoFullName, run.CommitSHA, status)
	}
	if err != nil {
		logger.Warn("pipeline commit status report failed",
			"run_id", run.RunID, "repo", run.RepoFullName, "error", err)
	}
}

func (r *Router) loadPipelinesFromKVS(logger *slog.Logger) error {
	versions, _, err := r.guardianVersions.list(pipelineConfigsPrefix, 1000, "")
	if err != nil {
		return err
	}

	if len(versions) == 0 {
		return nil
	}

	for _, version := range versions {
		if version == nil || version.GetTombstone() {
			continue
		}

		logicalPath := version.GetLogicalPath()
		stored, exists := r.guardianVersions.currentVersion(logicalPath)
		if !exists || stored.Tombstone || len(stored.Content) == 0 {
			continue
		}

		cfg, err := pipeline.ParseConfig(stored.Content)
		if err != nil {
			logger.Warn("skip invalid pipeline config",
				"path", logicalPath,
				"error", err,
			)
			continue
		}
		r.pipelineOrchestrator.RegisterPipeline(cfg)
		if r.pipelineWebhookHandler != nil {
			r.pipelineWebhookHandler.RegisterPipeline(cfg)
		}
		logger.Debug("loaded pipeline", "name", cfg.Name)
	}

	return nil
}

func (r *Router) watchPipelinesFromKVS(logger *slog.Logger) {
	logger.Info("watching for pipeline config changes", "prefix", pipelineConfigsPrefix)

	sub, id := r.subscribeGuardianLogicalChanges([]string{pipelineConfigsPrefix}, true)
	defer r.unsubscribeGuardianLogicalChanges(id)

	for {
		select {
		case event, ok := <-sub:
			if !ok {
				return
			}
			logicalPath := event.GetLogicalPath()
			if logicalPath == "" {
				continue
			}

			switch event.GetType() {
			case pb.ChangeType_ADDED, pb.ChangeType_MODIFIED:
				content := event.GetInlineContent()
				if len(content) == 0 {
					logger.Warn("pipeline change event has no inline content", "path", logicalPath)
					continue
				}
				cfg, err := pipeline.ParseConfig(content)
				if err != nil {
					logger.Warn("invalid pipeline config in change event",
						"path", logicalPath, "error", err,
					)
					continue
				}
				r.pipelineOrchestrator.RegisterPipeline(cfg)
				if r.pipelineWebhookHandler != nil {
					r.pipelineWebhookHandler.RegisterPipeline(cfg)
				}
				logger.Info("pipeline registered from change event", "name", cfg.Name)

			case pb.ChangeType_DELETED:
				current, exists := r.guardianVersions.currentVersion(logicalPath)
				if !exists || current.Tombstone {
					r.pipelineOrchestrator.UnregisterPipeline(logicalPath)
					logger.Info("pipeline unregistered", "path", logicalPath)
				}
			}
		case <-r.stopUI:
			return
		}
	}
}

type routerKVSClient struct {
	router *Router
}

func (c *routerKVSClient) Write(logicalPath string, content []byte, expectedVersionID string) (string, error) {
	return c.router.writePipelinePath(logicalPath, content, expectedVersionID)
}

func (c *routerKVSClient) Read(logicalPath string) ([]byte, string, error) {
	return c.router.readPipelinePath(logicalPath)
}

func (c *routerKVSClient) Delete(logicalPath string) error {
	return c.router.deletePipelinePath(logicalPath)
}

func (c *routerKVSClient) List(logicalDir string) ([]string, error) {
	return c.router.listPipelinePath(logicalDir)
}

func (r *Router) subscribeGuardianLogicalChanges(prefixes []string, includeInline bool) (<-chan *pb.GuardianChangeEvent, uint64) {
	r.guardianLogicalChangeSubsMu.Lock()
	defer r.guardianLogicalChangeSubsMu.Unlock()

	id := r.guardianLogicalChangeSeq.Add(1)
	ch := make(chan *pb.GuardianChangeEvent, 128)

	r.guardianLogicalChangeSubs[id] = &guardianLogicalChangeSubscriber{
		id:                 id,
		logicalPrefixes:    prefixes,
		events:             ch,
		includeInlineBytes: includeInline,
	}

	return ch, id
}

func (r *Router) unsubscribeGuardianLogicalChanges(id uint64) {
	r.guardianLogicalChangeSubsMu.Lock()
	defer r.guardianLogicalChangeSubsMu.Unlock()
	if sub, ok := r.guardianLogicalChangeSubs[id]; ok {
		close(sub.events)
		delete(r.guardianLogicalChangeSubs, id)
	}
}

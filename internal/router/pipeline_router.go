package router

import (
	"log/slog"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/router/pipeline"
)

const pipelinePrincipalID = "monofs-pipeline"
const pipelinePrincipalToken = "monofs-pipeline-internal-token"
const pipelineConfigsPrefix = "/.pipelines"

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
	r.pipelineOrchestrator = orch

	wh := pipeline.NewWebhookHandler(
		orch,
		pipeline.WebhookConfig{},
		"monofs-packages.yaml",
	)
	r.pipelineWebhookHandler = wh

	if err := r.loadPipelinesFromKVS(logger); err != nil {
		logger.Warn("initial pipeline load failed", "error", err)
	}

	go r.watchPipelinesFromKVS(logger)

	logger.Info("pipeline orchestrator initialized — configs read from /.pipelines/")
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

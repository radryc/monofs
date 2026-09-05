package pipeline

import (
	"context"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

func LoadConfig(path string) (*PipelineConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pipeline config %s: %w", path, err)
	}
	return ParseConfig(data)
}

func ParseConfig(data []byte) (*PipelineConfig, error) {
	var cfg PipelineConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse pipeline config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate pipeline config: %w", err)
	}
	return &cfg, nil
}

func (c *PipelineConfig) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("pipeline name is required")
	}
	if len(c.Jobs) == 0 {
		return fmt.Errorf("at least one job is required")
	}
	for name, job := range c.Jobs {
		if err := job.Validate(); err != nil {
			return fmt.Errorf("job %q: %w", name, err)
		}
		if !job.HasRunner() {
			_ = name
		}
	}
	if err := c.ValidateDAG(); err != nil {
		return err
	}
	return nil
}

func (j *JobConfig) Validate() error {
	if len(j.Steps) == 0 {
		return fmt.Errorf("at least one step is required")
	}
	if j.RunsOn != "" && !IsValidRunnerType(j.RunsOn) {
		return fmt.Errorf("unknown runner type: %q (valid: %v)", j.RunsOn, AllRunnerTypes())
	}
	return nil
}

func (j *JobConfig) HasRunner() bool {
	return j.RunsOn != ""
}

func (c *PipelineConfig) ValidateDAG() error {
	for name, job := range c.Jobs {
		for _, need := range job.Needs {
			if _, ok := c.Jobs[need]; !ok {
				return fmt.Errorf("job %q depends on unknown job %q", name, need)
			}
		}
	}
	if err := c.detectCycle(); err != nil {
		return err
	}
	return nil
}

func (c *PipelineConfig) detectCycle() error {
	visited := make(map[string]int)
	const (
		white = 0
		gray  = 1
		black = 2
	)
	var dfs func(name string) error
	dfs = func(name string) error {
		visited[name] = gray
		job := c.Jobs[name]
		for _, need := range job.Needs {
			switch visited[need] {
			case gray:
				return fmt.Errorf("cycle detected: %s -> %s", name, need)
			case white:
				if err := dfs(need); err != nil {
					return err
				}
			}
		}
		visited[name] = black
		return nil
	}
	for name := range c.Jobs {
		visited[name] = white
	}
	for name := range c.Jobs {
		if visited[name] == white {
			if err := dfs(name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *PipelineConfig) MatchEvent(event WebhookEvent) bool {
	eventMatched := false
	switch event.EventType {
	case TriggerPush:
		if c.On.Push == nil {
			return false
		}
		if !c.On.Push.matchesBranch(event.Branch) {
			return false
		}
		eventMatched = true
	case TriggerPullRequest:
		if c.On.PullRequest == nil {
			return false
		}
		if !c.On.PullRequest.matchesBranch(event.Branch) {
			return false
		}
		eventMatched = true
	case TriggerTag:
		if len(c.On.Tags) == 0 {
			return false
		}
		for _, pattern := range c.On.Tags {
			if matchTag(pattern, event.Tag) {
				eventMatched = true
				break
			}
		}
		if !eventMatched {
			return false
		}
	case TriggerManual:
		return true
	default:
		return false
	}

	if c.SourceDir != "" && c.SourceDir != "." && len(event.ChangedFiles) > 0 {
		hasMatch := false
		for _, file := range event.ChangedFiles {
			if isPathAffected(file, c.SourceDir) {
				hasMatch = true
				break
			}
		}
		if !hasMatch {
			return false
		}
	}

	return true
}

func (f *BranchFilter) matchesBranch(branch string) bool {
	if len(f.Branches) == 0 {
		return true
	}
	for _, pattern := range f.Branches {
		if matchGlob(pattern, branch) {
			return true
		}
	}
	return false
}

func matchGlob(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	rest := value
	for i, part := range parts {
		if part == "" {
			if i == len(parts)-1 {
				return true
			}
			continue
		}
		idx := strings.Index(rest, part)
		if idx < 0 {
			return false
		}
		rest = rest[idx+len(part):]
	}
	return true
}

func matchTag(pattern, tag string) bool {
	return matchGlob(pattern, tag)
}

func (c *PipelineConfig) EntrypointJobs() []string {
	var entrypoints []string
	for name, job := range c.Jobs {
		if len(job.Needs) == 0 {
			entrypoints = append(entrypoints, name)
		}
	}
	return entrypoints
}

func (c *PipelineConfig) DownstreamJobs(jobName string) []string {
	var downstream []string
	for name, job := range c.Jobs {
		for _, need := range job.Needs {
			if need == jobName {
				downstream = append(downstream, name)
			}
		}
	}
	return downstream
}

func (c *PipelineConfig) AllNeedsSatisfied(jobName string, completed map[string]bool) bool {
	job, ok := c.Jobs[jobName]
	if !ok {
		return false
	}
	for _, need := range job.Needs {
		if !completed[need] {
			return false
		}
	}
	return true
}

func LoadPackageMeta(ctx context.Context, path string) (*PackageMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read package meta %s: %w", path, err)
	}
	var meta PackageMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse package meta: %w", err)
	}
	return &meta, nil
}

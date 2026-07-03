package workspacepolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	EffectAllow = "allow"
	EffectDeny  = "deny"

	ActionRefresh    = "REFRESH"
	ActionPublish    = "PUBLISH"
	ActionSourcePush = "SOURCE_PUSH"

	PushModeSquash   = "squash"
	PushModePreserve = "preserve"

	BranchStrategyDirect        = "direct"
	BranchStrategyWorkspace     = "workspace_branch"
	BranchStrategyPerRepoBranch = "per_repo_branch"

	ReasonPolicyDenied              = "POLICY_DENIED"
	ReasonPolicyProtectedBranch     = "POLICY_PROTECTED_BRANCH"
	ReasonPolicyProtectedWorkspace  = "POLICY_PROTECTED_WORKSPACE"
	ReasonPolicyModeRestricted      = "POLICY_MODE_RESTRICTED"
	ReasonPolicyStrategyRestricted  = "POLICY_STRATEGY_RESTRICTED"
	ReasonPolicyRepoRestricted      = "POLICY_REPO_RESTRICTED"
	ReasonPolicyPrincipalNotAuth    = "POLICY_PRINCIPAL_NOT_AUTHORIZED"
	ReasonPolicyDefaultDeny         = "POLICY_DEFAULT_DENY"
	ReasonPolicyAllowed             = "POLICY_ALLOWED"
	ReasonPolicyDefaultAllow        = "POLICY_DEFAULT_ALLOW"
)

type PolicyConfig struct {
	Version int    `yaml:"version"`
	Default string `yaml:"default"`
	Rules   []Rule `yaml:"rules"`
}

type Rule struct {
	Name     string     `yaml:"name"`
	Match    MatchSet   `yaml:"match"`
	MatchNot MatchSet   `yaml:"match_not"`
	Effect   string     `yaml:"effect"`
	Reason   string     `yaml:"reason"`
}

type MatchSet struct {
	PrincipalIDs     []string `yaml:"principal_ids"`
	WorkspaceIDs     []string `yaml:"workspace_ids"`
	LogicalBranches  []string `yaml:"logical_branches"`
	RepositoryIDs    []string `yaml:"repository_ids"`
	Actions          []string `yaml:"actions"`
	PushModes        []string `yaml:"push_modes"`
	BranchStrategies  []string `yaml:"branch_strategies"`
}

type EvaluationRequest struct {
	PrincipalID     string
	WorkspaceID     string
	LogicalBranch   string
	RepositoryIDs   []string
	Action          string
	PushMode        string
	BranchStrategy  string
}

type EvaluationResult struct {
	Effect     string
	ReasonCode  string
	Reason     string
	RuleName   string
}

var validActions = map[string]bool{
	ActionRefresh: true, ActionPublish: true, ActionSourcePush: true,
}
var validPushModes = map[string]bool{PushModeSquash: true, PushModePreserve: true}
var validBranchStrategies = map[string]bool{
	BranchStrategyDirect: true, BranchStrategyWorkspace: true, BranchStrategyPerRepoBranch: true,
}
var validEffects = map[string]bool{EffectAllow: true, EffectDeny: true}

func Load(path string) (*PolicyConfig, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("policy config path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy config %s: %w", path, err)
	}
	return Parse(data)
}

func Parse(data []byte) (*PolicyConfig, error) {
	var cfg PolicyConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse policy YAML: %w", err)
	}
	if cfg.Version != 1 {
		return nil, fmt.Errorf("unsupported policy config version %d (expected 1)", cfg.Version)
	}
	if !validEffects[cfg.Default] {
		return nil, fmt.Errorf("policy default must be 'allow' or 'deny', got %q", cfg.Default)
	}
	for i, rule := range cfg.Rules {
		if strings.TrimSpace(rule.Name) == "" {
			return nil, fmt.Errorf("rule %d has empty name", i)
		}
		if !validEffects[rule.Effect] {
			return nil, fmt.Errorf("rule %q effect must be 'allow' or 'deny', got %q", rule.Name, rule.Effect)
		}
		for _, a := range rule.Match.Actions {
			if !validActions[a] {
				return nil, fmt.Errorf("rule %q has unknown action %q", rule.Name, a)
			}
		}
		for _, m := range rule.Match.PushModes {
			if !validPushModes[m] {
				return nil, fmt.Errorf("rule %q has unknown push_mode %q", rule.Name, m)
			}
		}
		for _, s := range rule.Match.BranchStrategies {
			if !validBranchStrategies[s] {
				return nil, fmt.Errorf("rule %q has unknown branch_strategy %q", rule.Name, s)
			}
		}
		for _, a := range rule.MatchNot.Actions {
			if !validActions[a] {
				return nil, fmt.Errorf("rule %q match_not has unknown action %q", rule.Name, a)
			}
		}
		for _, m := range rule.MatchNot.PushModes {
			if !validPushModes[m] {
				return nil, fmt.Errorf("rule %q match_not has unknown push_mode %q", rule.Name, m)
			}
		}
		for _, s := range rule.MatchNot.BranchStrategies {
			if !validBranchStrategies[s] {
				return nil, fmt.Errorf("rule %q match_not has unknown branch_strategy %q", rule.Name, s)
			}
		}
	}
	return &cfg, nil
}

func Evaluate(cfg *PolicyConfig, req *EvaluationRequest) *EvaluationResult {
	if cfg == nil || req == nil {
		return &EvaluationResult{Effect: EffectDeny, ReasonCode: ReasonPolicyDenied, Reason: "invalid config or request"}
	}

	for _, rule := range cfg.Rules {
		matched := matchesRule(&rule.Match, req)
		if !matched {
			continue
		}
		if !rule.MatchNot.isEmpty() && matchesRule(&rule.MatchNot, req) {
			continue
		}
		reasonCode := rule.Reason
		if reasonCode == "" {
			reasonCode = strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(rule.Name, " ", "_"), "-", "_"))
		}
		return &EvaluationResult{
			Effect:    rule.Effect,
			ReasonCode: reasonCode,
			Reason:    rule.Reason,
			RuleName:  rule.Name,
		}
	}

	if cfg.Default == EffectDeny {
		return &EvaluationResult{
			Effect:    EffectDeny,
			ReasonCode: ReasonPolicyDefaultDeny,
			Reason:    "no matching rule and default=deny",
		}
	}
	return &EvaluationResult{
		Effect:    EffectAllow,
		ReasonCode: ReasonPolicyDefaultAllow,
		Reason:    "no matching rule and default=allow",
	}
}

func matchesRule(ms *MatchSet, req *EvaluationRequest) bool {
	if ms == nil {
		return false
	}
	if ms.isEmpty() {
		return true
	}
	if !matchStringGlob(ms.PrincipalIDs, req.PrincipalID) { return false }
	if !matchStringGlob(ms.WorkspaceIDs, req.WorkspaceID) { return false }
	if !matchStringGlob(ms.LogicalBranches, req.LogicalBranch) { return false }
	if !matchAnyStringGlob(ms.RepositoryIDs, req.RepositoryIDs) { return false }
	if !matchStringExact(ms.Actions, req.Action) { return false }
	if !matchStringExact(ms.PushModes, req.PushMode) { return false }
	if !matchStringExact(ms.BranchStrategies, req.BranchStrategy) { return false }
	return true
}

func (ms *MatchSet) isEmpty() bool {
	return len(ms.PrincipalIDs) == 0 &&
		len(ms.WorkspaceIDs) == 0 &&
		len(ms.LogicalBranches) == 0 &&
		len(ms.RepositoryIDs) == 0 &&
		len(ms.Actions) == 0 &&
		len(ms.PushModes) == 0 &&
		len(ms.BranchStrategies) == 0
}

func matchStringGlob(patterns []string, value string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		matched, err := filepath.Match(p, value)
		if err == nil && matched {
			return true
		}
	}
	return false
}

func matchAnyStringGlob(patterns []string, values []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, v := range values {
		for _, p := range patterns {
			if matched, _ := filepath.Match(p, v); matched {
				return true
			}
		}
	}
	return false
}

func matchStringExact(patterns []string, value string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if p == value {
			return true
		}
	}
	return false
}

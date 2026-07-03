package workspacepolicy

import (
	"testing"
)

func TestDefaultDenyNoRules(t *testing.T) {
	cfg := &PolicyConfig{Version: 1, Default: EffectDeny}
	result := Evaluate(cfg, &EvaluationRequest{
		PrincipalID: "user-1", WorkspaceID: "ws-1", Action: ActionSourcePush,
	})
	if result.Effect != EffectDeny {
		t.Errorf("expected deny, got %s", result.Effect)
	}
	if result.ReasonCode != ReasonPolicyDefaultDeny {
		t.Errorf("expected %s, got %s", ReasonPolicyDefaultDeny, result.ReasonCode)
	}
}

func TestDefaultAllowNoRules(t *testing.T) {
	cfg := &PolicyConfig{Version: 1, Default: EffectAllow}
	result := Evaluate(cfg, &EvaluationRequest{
		PrincipalID: "user-1", WorkspaceID: "ws-1", Action: ActionSourcePush,
	})
	if result.Effect != EffectAllow {
		t.Errorf("expected allow, got %s", result.Effect)
	}
}

func TestFirstMatchWins(t *testing.T) {
	cfg := &PolicyConfig{
		Version: 1,
		Default: EffectDeny,
		Rules: []Rule{
			{Name: "block alice", Match: MatchSet{PrincipalIDs: []string{"alice"}}, Effect: EffectDeny, Reason: "no alice allowed"},
			{Name: "allow all", Match: MatchSet{}, Effect: EffectAllow, Reason: "catch-all"},
		},
	}
	result := Evaluate(cfg, &EvaluationRequest{PrincipalID: "alice", Action: ActionSourcePush})
	if result.Effect != EffectDeny {
		t.Errorf("alice should be denied by first rule, got %s", result.Effect)
	}
	if result.Reason != "no alice allowed" {
		t.Errorf("expected 'no alice allowed', got %s", result.Reason)
	}
}

func TestCatchAllAllows(t *testing.T) {
	cfg := &PolicyConfig{
		Version: 1,
		Default: EffectDeny,
		Rules: []Rule{
			{Name: "catch-all", Match: MatchSet{}, Effect: EffectAllow, Reason: "allow everything"},
		},
	}
	result := Evaluate(cfg, &EvaluationRequest{PrincipalID: "bob", Action: ActionPublish})
	if result.Effect != EffectAllow {
		t.Errorf("expected allow, got %s", result.Effect)
	}
}

func TestWorkspaceGlobMatch(t *testing.T) {
	cfg := &PolicyConfig{
		Version: 1,
		Default: EffectDeny,
		Rules: []Rule{
			{Name: "prod block", Match: MatchSet{WorkspaceIDs: []string{"prod-*"}}, Effect: EffectDeny, Reason: "no prod"},
		},
	}
	result := Evaluate(cfg, &EvaluationRequest{WorkspaceID: "prod-us-east", Action: ActionSourcePush})
	if result.Effect != EffectDeny {
		t.Errorf("prod-* should match prod-us-east, got %s", result.Effect)
	}

	result2 := Evaluate(cfg, &EvaluationRequest{WorkspaceID: "dev-sandbox", Action: ActionSourcePush})
	if result2.Effect != EffectDeny {
		t.Errorf("dev sandbox unmatched → default deny, got %s", result2.Effect)
	}
}

func TestActionMatch(t *testing.T) {
	cfg := &PolicyConfig{
		Version: 1,
		Default: EffectAllow,
		Rules: []Rule{
			{Name: "block push", Match: MatchSet{Actions: []string{ActionSourcePush, ActionPublish}}, Effect: EffectDeny, Reason: "no writes"},
		},
	}
	result := Evaluate(cfg, &EvaluationRequest{Action: ActionRefresh})
	if result.Effect != EffectAllow {
		t.Errorf("refresh should be allowed, got %s", result.Effect)
	}
	result2 := Evaluate(cfg, &EvaluationRequest{Action: ActionSourcePush})
	if result2.Effect != EffectDeny {
		t.Errorf("source push should be denied, got %s", result2.Effect)
	}
}

func TestMatchNotNegation(t *testing.T) {
	cfg := &PolicyConfig{
		Version: 1,
		Default: EffectAllow,
		Rules: []Rule{
			{
				Name:  "deny non-staging per-repo-branch",
				Match: MatchSet{BranchStrategies: []string{BranchStrategyPerRepoBranch}},
				MatchNot: MatchSet{WorkspaceIDs: []string{"staging-*"}},
				Effect: EffectDeny,
				Reason: "per-repo-branch restricted to staging",
			},
		},
	}
	result := Evaluate(cfg, &EvaluationRequest{
		WorkspaceID: "dev-test", BranchStrategy: BranchStrategyPerRepoBranch, Action: ActionSourcePush,
	})
	if result.Effect != EffectDeny {
		t.Errorf("dev workspace with per-repo-branch should be denied, got %s", result.Effect)
	}

	result2 := Evaluate(cfg, &EvaluationRequest{
		WorkspaceID: "staging-us", BranchStrategy: BranchStrategyPerRepoBranch, Action: ActionSourcePush,
	})
	if result2.Effect != EffectAllow {
		t.Errorf("staging workspace with per-repo-branch should pass match_not, got %s", result2.Effect)
	}
}

func TestRepositoryMatchAny(t *testing.T) {
	cfg := &PolicyConfig{
		Version: 1,
		Default: EffectAllow,
		Rules: []Rule{
			{
				Name:  "block sensitive repos",
				Match: MatchSet{RepositoryIDs: []string{"repo-secret", "repo-internal"}},
				Effect: EffectDeny,
				Reason: "sensitive repo blocked",
			},
		},
	}
	result := Evaluate(cfg, &EvaluationRequest{
		Action: ActionSourcePush, RepositoryIDs: []string{"repo-normal", "repo-secret"},
	})
	if result.Effect != EffectDeny {
		t.Errorf("job touching secret repo should be denied, got %s", result.Effect)
	}
	result2 := Evaluate(cfg, &EvaluationRequest{
		Action: ActionSourcePush, RepositoryIDs: []string{"repo-normal", "repo-docs"},
	})
	if result2.Effect != EffectAllow {
		t.Errorf("job with no secret repos should be allowed, got %s", result2.Effect)
	}
}

func TestParseValid(t *testing.T) {
	data := []byte(`
version: 1
default: deny
rules:
  - name: "allow ops team"
    match:
      principal_ids: ["ops-*"]
    effect: allow
    reason: "ops team authorized"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(cfg.Rules))
	}
}

func TestParseInvalidDefault(t *testing.T) {
	cfg, err := Parse([]byte(`version: 1
default: maybe
`))
	if err == nil {
		t.Fatalf("expected error for invalid default, got config: %+v", cfg)
	}
}

func TestParseInvalidAction(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
default: allow
rules:
  - name: test
    match:
      actions: ["DELETE"]
    effect: deny
`))
	if err == nil {
		t.Fatal("expected error for invalid action")
	}
}

func TestParseInvalidEffect(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
default: allow
rules:
  - name: test
    match: {}
    effect: maybe
`))
	if err == nil {
		t.Fatal("expected error for invalid effect")
	}
}

func TestPushModeMatch(t *testing.T) {
	cfg := &PolicyConfig{
		Version: 1,
		Default: EffectAllow,
		Rules: []Rule{
			{Name: "ci squash only", Match: MatchSet{WorkspaceIDs: []string{"ci-*"}, PushModes: []string{PushModePreserve}}, Effect: EffectDeny, Reason: "CI must use squash"},
		},
	}
	result := Evaluate(cfg, &EvaluationRequest{WorkspaceID: "ci-build", PushMode: PushModePreserve, Action: ActionSourcePush})
	if result.Effect != EffectDeny {
		t.Errorf("ci preserve should be denied, got %s", result.Effect)
	}
	result2 := Evaluate(cfg, &EvaluationRequest{WorkspaceID: "ci-build", PushMode: PushModeSquash, Action: ActionSourcePush})
	if result2.Effect != EffectAllow {
		t.Errorf("ci squash should be allowed, got %s", result2.Effect)
	}
}

func TestNilConfigDenies(t *testing.T) {
	result := Evaluate(nil, &EvaluationRequest{Action: ActionSourcePush})
	if result.Effect != EffectDeny {
		t.Errorf("nil config should deny, got %s", result.Effect)
	}
}

package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBEPTargetCompleted(t *testing.T) {
	// Simulate a BEP JSON file with target completed events.
	bepContent := `{"id":{"targetCompleted":{"label":"//pkg/auth:auth"}},"completed":{"success":true}}
{"id":{"targetCompleted":{"label":"//cmd/server:server"}},"completed":{"success":false,"exit_code":1}}
{"id":{"targetCompleted":{"label":"//pkg/utils:utils"}},"completed":{"success":true}}`

	dir := t.TempDir()
	path := filepath.Join(dir, "bep.json")
	os.WriteFile(path, []byte(bepContent), 0644)

	result, err := ParseBEP(path)
	if err != nil {
		t.Fatalf("ParseBEP: %v", err)
	}
	if result.TargetsBuilt != 3 {
		t.Errorf("TargetsBuilt: got %d, want 3", result.TargetsBuilt)
	}
	if len(result.TargetResults) != 3 {
		t.Fatalf("TargetResults: got %d, want 3", len(result.TargetResults))
	}

	// First target: success.
	if result.TargetResults[0].Status != "BUILT" {
		t.Errorf("target 0 status: got %s, want BUILT", result.TargetResults[0].Status)
	}

	// Second target: failure.
	if result.TargetResults[1].Status != "FAILED" {
		t.Errorf("target 1 status: got %s, want FAILED", result.TargetResults[1].Status)
	}
	if result.TargetResults[1].ExitCode != 1 {
		t.Errorf("target 1 exit_code: got %d, want 1", result.TargetResults[1].ExitCode)
	}
}

func TestParseBEPTestResult(t *testing.T) {
	bepContent := `{"id":{"testResult":{"label":"//pkg/auth:auth_test","status":"PASSED"}}}
{"id":{"testResult":{"label":"//pkg/auth:edge_test","status":"FAILED"}}}`

	dir := t.TempDir()
	path := filepath.Join(dir, "bep.json")
	os.WriteFile(path, []byte(bepContent), 0644)

	result, err := ParseBEP(path)
	if err != nil {
		t.Fatalf("ParseBEP: %v", err)
	}
	if result.TargetsTested != 2 {
		t.Errorf("TargetsTested: got %d, want 2", result.TargetsTested)
	}
	if result.TestsPassed != 1 {
		t.Errorf("TestsPassed: got %d, want 1", result.TestsPassed)
	}
	if result.TestsFailed != 1 {
		t.Errorf("TestsFailed: got %d, want 1", result.TestsFailed)
	}
}

func TestParseBEPEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	os.WriteFile(path, []byte(""), 0644)

	result, err := ParseBEP(path)
	if err != nil {
		t.Fatalf("ParseBEP empty: %v", err)
	}
	if result.TargetsBuilt != 0 {
		t.Errorf("empty BEP: got %d targets", result.TargetsBuilt)
	}
}

func TestParseBEPMissing(t *testing.T) {
	_, err := ParseBEP("/nonexistent/path.json")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestBazelCommand(t *testing.T) {
	ctx := &BazelJobContext{
		CacheAddr:    "monofs-cache:9092",
		ExecutorAddr: "monofs-executor:9093",
		MountPath:    "/mnt/monofs",
		Targets:      []string{"//sre/api/..."},
	}

	cmd := ctx.BazelCommand("build")
	if !strings.Contains(cmd, "bazel build") {
		t.Errorf("missing bazel build: %s", cmd)
	}
	if !strings.Contains(cmd, "--config=ci") {
		t.Errorf("missing --config=ci: %s", cmd)
	}
	if !strings.Contains(cmd, "--remote_cache=http://monofs-cache:9092") {
		t.Errorf("missing remote_cache: %s", cmd)
	}
	if !strings.Contains(cmd, "--remote_executor=grpc://monofs-executor:9093") {
		t.Errorf("missing remote_executor: %s", cmd)
	}
	if !strings.Contains(cmd, "//sre/api/...") {
		t.Errorf("missing targets: %s", cmd)
	}
	if !strings.Contains(cmd, "--build_event_json_file=") {
		t.Errorf("missing BEP file: %s", cmd)
	}
}

func TestBazelCommandDefaults(t *testing.T) {
	ctx := &BazelJobContext{}
	cmd := ctx.BazelCommand("test")
	if !strings.Contains(cmd, "bazel test") {
		t.Errorf("missing bazel test: %s", cmd)
	}
	if !strings.Contains(cmd, "//...") {
		t.Errorf("missing default targets: %s", cmd)
	}
	if !strings.Contains(cmd, "--test_output=errors") {
		t.Errorf("missing test_output=errors: %s", cmd)
	}
}

func TestBazelCommandNoRemote(t *testing.T) {
	ctx := &BazelJobContext{} // No cache/executor.
	cmd := ctx.BazelCommand("build")
	if strings.Contains(cmd, "remote_cache") {
		t.Error("should not contain remote_cache when not configured")
	}
	if strings.Contains(cmd, "remote_executor") {
		t.Error("should not contain remote_executor when not configured")
	}
}

func TestMarshalStepResult(t *testing.T) {
	bep := &BazelBuildResult{
		TargetsBuilt:  10,
		TargetsTested: 25,
		TestsPassed:   24,
		TestsFailed:   1,
		CacheHits:     8,
	}

	data := MarshalStepResult(0, "", bep)
	s := string(data)

	if !strings.Contains(s, `"targets_built":10`) {
		t.Errorf("missing targets_built: %s", s)
	}
	if !strings.Contains(s, `"tests_passed":24`) {
		t.Errorf("missing tests_passed: %s", s)
	}
}

func TestMarshalStepResultError(t *testing.T) {
	data := MarshalStepResult(1, "build failed", nil)
	s := string(data)
	if !strings.Contains(s, `"exit_code":1`) {
		t.Errorf("missing exit_code: %s", s)
	}
	if !strings.Contains(s, "build failed") {
		t.Errorf("missing error: %s", s)
	}
}

func TestAllRunnerTypes(t *testing.T) {
	types := AllRunnerTypes()
	if len(types) != 5 {
		t.Errorf("expected 5 runner types, got %d", len(types))
	}
	if !IsValidRunnerType(RunnerBazel) {
		t.Error("RunnerBazel should be valid")
	}
	if !IsValidRunnerType(RunnerBuilder) {
		t.Error("RunnerBuilder should be valid")
	}
	if IsValidRunnerType("invalid") {
		t.Error("invalid runner type should not be valid")
	}
}

func TestValidateBazelRunner(t *testing.T) {
	if err := ValidateBazelRunner(RunnerBazel); err != nil {
		t.Errorf("bazel runner should be valid: %v", err)
	}
	if err := ValidateBazelRunner(RunnerBuilder); err == nil {
		t.Error("builder runner should not pass bazel validation")
	}
}

func TestComputeAffectedTargets(t *testing.T) {
	targets, err := ComputeAffectedTargets("", "", "")
	if err != nil {
		t.Fatalf("ComputeAffectedTargets: %v", err)
	}
	if len(targets) != 1 || targets[0] != "//..." {
		t.Errorf("got %v, want [//...]", targets)
	}
}

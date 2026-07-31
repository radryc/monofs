package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BazelBuildResult collects structured information from a Bazel invocation.
// It is populated by parsing the Build Event Protocol JSON output.
type BazelBuildResult struct {
	TargetsBuilt  int
	TargetsTested int
	TestsPassed   int
	TestsFailed   int
	CacheHits     int
	CacheMisses   int
	TotalDuration time.Duration
	TargetResults []BazelTargetResult
	RawEvents     []json.RawMessage
}

// BazelTargetResult describes a single Bazel target build/test result.
type BazelTargetResult struct {
	Label    string
	Status   string // "BUILT", "FAILED", "PASSED", "TIMEOUT"
	Duration time.Duration
	ExitCode int32
	Error    string
}

// ParseBEP reads a Bazel Build Event Protocol JSON file and extracts
// the structured build/test results.
//
// Bazel writes BEP events to a file specified by --build_event_json_file.
// Each line is a JSON object. We parse the relevant event types:
//   - "completed" events with "targetCompleted" for per-target results
//   - "progress" events with stdout/stderr for log output
//   - "finished" event for overall build result
func ParseBEP(bepPath string) (*BazelBuildResult, error) {
	data, err := os.ReadFile(bepPath)
	if err != nil {
		return nil, fmt.Errorf("read BEP file: %w", err)
	}

	result := &BazelBuildResult{}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var event map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		result.RawEvents = append(result.RawEvents, json.RawMessage(line))

		// Extract target completed events.
		if id, ok := event["id"]; ok {
			var idMap map[string]json.RawMessage
			if json.Unmarshal(id, &idMap) == nil {
				if tc, ok := idMap["targetCompleted"]; ok {
					var target struct {
						Label string `json:"label"`
					}
					if json.Unmarshal(tc, &target) == nil && target.Label != "" {
						targetResult := BazelTargetResult{Label: target.Label, Status: "BUILT"}
						if completed, ok := event["completed"]; ok {
							var comp struct {
								Success  bool  `json:"success"`
								ExitCode int32 `json:"exit_code"`
							}
							if json.Unmarshal(completed, &comp) == nil {
								if !comp.Success {
									targetResult.Status = "FAILED"
								}
								targetResult.ExitCode = comp.ExitCode
							}
						}
						result.TargetResults = append(result.TargetResults, targetResult)
						result.TargetsBuilt++
					}
				}

				if tr, ok := idMap["testResult"]; ok {
					var testRes struct {
						Label  string `json:"label"`
						Status string `json:"status"`
					}
					if json.Unmarshal(tr, &testRes) == nil {
						result.TargetsTested++
						targetResult := BazelTargetResult{
							Label:  testRes.Label,
							Status: strings.ToUpper(testRes.Status),
						}
						if testRes.Status == "FAILED" {
							result.TestsFailed++
							targetResult.ExitCode = 1
						} else {
							result.TestsPassed++
						}
						result.TargetResults = append(result.TargetResults, targetResult)
					}
				}
			}
		}

		// Count cache hits from action summary.
		if _, ok := event["cacheHits"]; ok {
			var hits struct {
				CacheHits int `json:"cacheHits"`
			}
			if json.Unmarshal([]byte(line), &hits) == nil {
				result.CacheHits += hits.CacheHits
			}
		}
	}

	return result, nil
}

// BazelRunner wraps information about a Bazel pipeline job.
type BazelRunner struct {
	// CacheAddr is the monofs-cache address for remote caching.
	CacheAddr string

	// ExecutorAddr is the monofs-executor address for remote execution.
	ExecutorAddr string

	// MountPath is the path to the MonoFS FUSE mount.
	MountPath string
}

// BazelJobContext compiles the environment variables and flags to pass
// to a bazel invocation in a pipeline job.
type BazelJobContext struct {
	CurrentCommit  string
	PreviousCommit string
	BuildTag       string
	CacheAddr      string
	ExecutorAddr   string
	MountPath      string
	Targets        []string // Bazel target patterns (e.g. "//...")
}

// BazelCommand returns the bazel command line for a given action.
func (ctx *BazelJobContext) BazelCommand(action string) string {
	args := []string{"bazel", action}

	// Always use CI config.
	args = append(args, "--config=ci")

	// Remote cache.
	if ctx.CacheAddr != "" {
		args = append(args, "--remote_cache=http://"+ctx.CacheAddr)
	}

	// Remote executor.
	if ctx.ExecutorAddr != "" {
		args = append(args, "--remote_executor=grpc://"+ctx.ExecutorAddr)
	}

	// Targets.
	if len(ctx.Targets) > 0 {
		args = append(args, ctx.Targets...)
	} else {
		args = append(args, "//...")
	}

	// BEP output.
	bepFile := filepath.Join(os.TempDir(), fmt.Sprintf("bazel-bep-%s.json", action))
	args = append(args, "--build_event_json_file="+bepFile)

	// Test-specific flags.
	if action == "test" {
		args = append(args, "--test_output=errors")
	}

	return strings.Join(args, " ")
}

// BEPFilePath returns the path to the BEP file for the given action
// within the given workspace directory.
func BEPFilePath(workDir, action string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("bazel-bep-%s.json", action))
}

// BazelStepConfig configures a pipeline step that runs Bazel.
type BazelStepConfig struct {
	// Action is "build", "test", or "run".
	Action string `yaml:"action" json:"action"`

	// Targets are the Bazel target patterns.
	Targets []string `yaml:"targets,omitempty" json:"targets,omitempty"`

	// AffectedOnly limits to targets affected by the current change.
	AffectedOnly bool `yaml:"affected_only,omitempty" json:"affected_only,omitempty"`
}

// ValidateBazelRunner checks if the runner type "bazel" is valid for a job.
func ValidateBazelRunner(runner RunnerType) error {
	if runner == RunnerBazel {
		return nil
	}
	return fmt.Errorf("expected bazel runner, got %s", runner)
}

// AllRunnerTypes returns all valid runner types.
func AllRunnerTypes() []RunnerType {
	return []RunnerType{RunnerBuilder, RunnerDocker, RunnerDeploy, RunnerLambda, RunnerBazel}
}

// IsValidRunnerType checks if a runner type string is recognized.
func IsValidRunnerType(runner RunnerType) bool {
	for _, r := range AllRunnerTypes() {
		if r == runner {
			return true
		}
	}
	return false
}

// stepResultJSON is a compact serialization of a step's execution result.
type stepResultJSON struct {
	ExitCode      int    `json:"exit_code"`
	Error         string `json:"error,omitempty"`
	TargetsBuilt  int    `json:"targets_built,omitempty"`
	TargetsTested int    `json:"targets_tested,omitempty"`
	TestsPassed   int    `json:"tests_passed,omitempty"`
	TestsFailed   int    `json:"tests_failed,omitempty"`
	CacheHits     int    `json:"cache_hits,omitempty"`
	CacheMisses   int    `json:"cache_misses,omitempty"`
	DurationMs    int64  `json:"duration_ms,omitempty"`
}

// MarshalStepResult serializes a step result as JSON for log output.
func MarshalStepResult(exitCode int, errStr string, bep *BazelBuildResult) []byte {
	sr := stepResultJSON{
		ExitCode: exitCode,
		Error:    errStr,
	}
	if bep != nil {
		sr.TargetsBuilt = bep.TargetsBuilt
		sr.TargetsTested = bep.TargetsTested
		sr.TestsPassed = bep.TestsPassed
		sr.TestsFailed = bep.TestsFailed
		sr.CacheHits = bep.CacheHits
		sr.DurationMs = bep.TotalDuration.Milliseconds()
	}
	data, _ := json.Marshal(sr)
	return data
}

// ComputeAffectedTargets uses git diff to determine which Bazel targets changed.
// This is a simpler alternative to bazel diff when the full Bazel is not available.
func ComputeAffectedTargets(workDir, oldCommit, newCommit string) ([]string, error) {
	// For now, return all targets — this would be enhanced with bazel query.
	_ = workDir
	_ = oldCommit
	_ = newCommit
	return []string{"//..."}, nil
}

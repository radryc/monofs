# Phase 3 — Policy-Gated and Automatic Upstream Push

## Design

### Objective
Enforce server-side push governance and add safe automated push orchestration.

### Current baseline

- Router/fetcher accept push requests directly once bundle validation passes (`internal/router/workspace_source_push.go`, `internal/fetcher/workspace_source_push.go`).
- No branch/actor/repository policy gate before upstream mutation.
- No autonomous scheduler for pending local commit pushes.

### Policy engine model

- New policy package: `internal/router/workspacepolicy`
- Policy inputs:
  - `principal_id` / `client_id`
  - `workspace_id`
  - `logical_branch`
  - `repository_refs` (list of storage IDs)
  - `action` (`REFRESH`, `PUBLISH`, `SOURCE_PUSH`)
  - `push_mode` (`squash`, `preserve`)
  - `branch_strategy` (`direct`, `workspace_branch`, `per_repo_branch`)
- Policy outputs:
  - allow/deny
  - machine-readable reason code (see reason codes below)
  - human-readable explanation
- Rule model: ordered list, **first-match-wins** (like firewall rules). Default action when no rule matches is configurable (`allow` or `deny`).

### Policy configuration schema

Policy lives in a standalone YAML file, loaded at startup and hot-reloadable via SIGHUP (or admin RPC).

**Config flag:** `--policy-config /etc/monofs/policy.yaml`

If the flag is unset or the file is absent, and `PolicyGateEnabled=true`, the default action applies to all requests (no rules to match). If `PolicyGateEnabled=false`, policy is bypassed entirely (Phase 0 gate).

```yaml
# /etc/monofs/policy.yaml
#
# Ordered rule list. First match wins. If no rule matches, `default` applies.
# All match fields within a rule are ANDed. Multiple values within a field are ORed.
# Pattern fields support glob syntax (* matches any sequence, ? matches one character).

version: 1
default: deny    # "allow" or "deny" — default action when no rule matches

rules:
  # --- Example 1: block writes to protected branches ---
  - name: "block push to main on production workspaces"
    match:
      workspace_ids: ["prod-*"]
      logical_branches: ["main", "master"]
      actions: [SOURCE_PUSH, PUBLISH]
    effect: deny
    reason: "direct push to protected branch prohibited"

  # --- Example 2: allow ops team unrestricted access ---
  - name: "ops team full access"
    match:
      principal_ids: ["ops-bot-01", "ops-bot-02"]
    effect: allow
    reason: "ops team authorized"

  # --- Example 3: require squash mode on CI workspaces ---
  - name: "ci workspaces must use squash"
    match:
      workspace_ids: ["ci-*"]
      push_modes: [preserve]
    effect: deny
    reason: "CI workspaces require squash push mode"

  # --- Example 4: per-repo-branch strategy restricted to staging ---
  - name: "per-repo-branch only for staging workspaces"
    match:
      branch_strategies: [per_repo_branch]
    match_not:
      workspace_ids: ["staging-*"]
    effect: deny
    reason: "per-repo-branch strategy restricted to staging workspaces"

  # --- Example 5: catch-all allow ---
  - name: "default allow for development workspaces"
    match:
      workspace_ids: ["dev-*", "sandbox-*"]
    effect: allow
    reason: "development workspace — push permitted"
```

### Match field reference

All match fields are optional. An absent field matches everything (no filter).

| Field | Type | Description |
|-------|------|-------------|
| `principal_ids` | `[]string` (glob) | Match requesting client ID. Maps to `WorkspaceSyncJob.requested_by_client_id`. |
| `workspace_ids` | `[]string` (glob) | Match workspace ID. |
| `logical_branches` | `[]string` (glob) | Match logical branch name (e.g. `"main"`, `"feature/*"`). |
| `repository_ids` | `[]string` (glob) | Match repository storage IDs. True if **any** repo in the job matches. |
| `actions` | `[]string` (enum) | Match `WorkspaceSyncAction`: `REFRESH`, `PUBLISH`, `SOURCE_PUSH`. |
| `push_modes` | `[]string` (enum) | Match push mode: `squash`, `preserve`. Only meaningful for `SOURCE_PUSH` actions; ignored for others. |
| `branch_strategies` | `[]string` (enum) | Match branch strategy: `direct`, `workspace_branch`, `per_repo_branch`. Only meaningful for `PUBLISH`/`SOURCE_PUSH`; ignored for `REFRESH`. |

Additionally, `match_not` is the negated form of `match`: the rule fires only if **none** of the `match_not` conditions are satisfied. Fields in `match` and `match_not` are combined: `match AND NOT match_not`. If a field appears in both, `match_not` takes precedence for that field.

### Effect and reason codes

| Field | Type | Description |
|-------|------|-------------|
| `effect` | `allow` or `deny` | Decision. |
| `reason` | `string` | Human-readable explanation. Emitted in audit events and CLI/UI responses. |
| `reason_code` | `string` (optional) | Machine-readable reason code. If unset, derived from the rule name (snake_cased). |

Predefined reason codes (the `workspacepolicy` package exports these as constants):

| Code | Meaning |
|------|---------|
| `POLICY_DENIED` | Generic denial by rule match |
| `POLICY_PROTECTED_BRANCH` | Branch is protected |
| `POLICY_PROTECTED_WORKSPACE` | Workspace is protected |
| `POLICY_MODE_RESTRICTED` | Push mode not allowed |
| `POLICY_STRATEGY_RESTRICTED` | Branch strategy not allowed |
| `POLICY_REPO_RESTRICTED` | Repository not allowed for this operation |
| `POLICY_PRINCIPAL_NOT_AUTHORIZED` | Caller not authorized |
| `POLICY_DEFAULT_DENY` | No matching rule and default=deny |
| `POLICY_ALLOWED` | Allowed by rule match |
| `POLICY_DEFAULT_ALLOW` | No matching rule and default=allow |

### Rule evaluation semantics

```
evaluate(request):
  for rule in rules (in order):
    if rule.match matches request AND NOT (rule.match_not matches request):
      emit audit event: policy.{allowed,denied}, reason_code, rule name
      return rule.effect, rule.reason_code, rule.reason
  
  emit audit event: policy.{allowed,denied}, POLICY_DEFAULT_{ALLOW,DENY}
  return default, POLICY_DEFAULT_{ALLOW,DENY}, "no matching rule"
```

A `match` or `match_not` field is satisfied when the request value matches **any** value in the list. For pattern strings, `*` matches any character sequence and `?` matches exactly one character. For enum fields, exact string comparison is used.

### Validation at load time

- Unknown field names in `match` → reject config, refuse to start.
- Unknown values in enum fields (`actions`, `push_modes`, `branch_strategies`) → reject config.
- `effect` not `allow` or `deny` → reject config.
- Empty rule name → reject config.
- No rules and `default=deny` → warn but accept (everything denied; operator intent).

### Auto-push orchestrator model

- Router-owned background worker with:
  - periodic scan (`AutoPushInterval`)
  - per-workspace+branch exclusion lock
  - global concurrency cap
  - exponential backoff and pause/dead-letter state
- Trigger options:
  - periodic only (default)
  - optional explicit trigger API for operators.

### Scale targets (from Phase 1 assumptions: 2,000 engineers, ~8,000 push jobs/day)

| Metric | Target |
|--------|--------|
| Auto-push scan interval | 60 seconds (configurable) |
| Concurrent auto-push cap | 20 (leaves headroom for manual pushes) |
| Policy evaluation latency (p99) | ≤ 1 ms (in-memory rule match, ≤ 50 rules) |
| Backoff ceiling | 30 minutes (exponential: 1m → 2m → 4m → ... → 30m) |
| Dead-letter after | 10 consecutive failures (persisted in Phase 1 job store) |

### Audit integration

- Every deny and auto-trigger emits immutable audit events (Phase 1 ledger).

### Auto-PR provider integration

When a source push uses a non-direct branch strategy (`workspace_branch` or `per_repo_branch`), the pushed commits land on a `monofs/<workspace_id>/...` branch. To merge them into the target branch, a pull request must be created. The router can do this automatically if provider credentials are configured, or emit a clickable compare URL otherwise.

**Package:** `internal/router/workspacepr` — separate from the policy engine so it can be tested and configured independently.

**Provider interface:**

```go
type PullRequestProvider interface {
    Create(ctx context.Context, req CreatePRRequest) (*CreatePRResult, error)
    ProviderName() string
}

type CreatePRRequest struct {
    RepoCloneURL    string // e.g. "https://github.com/org/repo.git"
    SourceBranch    string // e.g. "monofs/ws-abc/f3a1b2"
    TargetBranch    string // e.g. "main"
    Title           string
    Body            string
}

type CreatePRResult struct {
    WebURL   string // e.g. "https://github.com/org/repo/pull/42"
    ID       string // provider-specific PR ID (GitHub: number as string, GitLab: iid)
    Created  bool   // false if PR already exists for this source→target
}
```

**Supported providers (Phase 1 — two implementations):**

| Provider | Detection | API | Auth |
|----------|-----------|-----|------|
| GitHub | `github.com` in `RepoCloneURL` host | REST API v3 (`POST /repos/{owner}/{repo}/pulls`) | `Authorization: Bearer <token>` (PAT or GitHub App installation token) |
| GitLab | `gitlab.com` or self-hosted domain configured via `GitLabBaseURL` | REST API v4 (`POST /projects/{id}/merge_requests`) | `PRIVATE-TOKEN: <token>` (PAT or project access token) |

**Auto-detection via `PRProviderType`:**

Router config field `PRProviderType`:

- `"auto"` (default) — parse `RepoCloneURL` hostname: `github.com` → GitHub, `gitlab.com` or host matching `GitLabBaseURL` → GitLab
- `"github"` — force GitHub even if URL doesn't match
- `"gitlab"` — force GitLab even if URL doesn't match

Provider selection happens per-repository (a single push job may touch repos from different Git hosts). The router instantiates the appropriate client lazily, using the same token for all repos on the same host.

**Deterministic compare URL fallback:**

When no token is configured for a detected provider, or the provider is unrecognized, the router **does not fail the push job**. It emits a `pr.compare_url` audit event and returns the URL to the CLI/UI so the engineer can manually create the PR.

| Provider | Compare URL template |
|----------|----------------------|
| GitHub | `https://github.com/{owner}/{repo}/compare/{target}...{source}` |
| GitLab | `https://gitlab.com/{owner}/{repo}/-/merge_requests/new?merge_request[source_branch]={source}&merge_request[target_branch]={target}` |
| Unknown | Plain text: `"Create PR: {source} → {target} on {RepoCloneURL}"` |

Owner and repo are parsed from `RepoCloneURL` (support both `https://` and `git@` SSH URLs).

**PR title and body templates:**

Configured in router config, with Go template variables:

```
PRTitleTemplate  = "MonoFS: {{.WorkspaceID}} → {{.TargetBranch}} ({{.CommitCount}} commits)"
PRBodyTemplate   = ""
```

If `PRBodyTemplate` is empty, a default body is generated:

```markdown
## MonoFS Workspace Push

- **Workspace:** `{{.WorkspaceID}}`
- **Job:** `{{.JobID}}`
- **Source branch:** `{{.SourceBranch}}`
- **Target branch:** `{{.TargetBranch}}`
- **Commits:** {{.CommitCount}}
- **Pushed by:** {{.PrincipalName}}

### Local commits

{{range .CommitMessages}}
- {{.}}
{{end}}
```

Template variables:

| Variable | Source |
|----------|--------|
| `{{.WorkspaceID}}` | `WorkspaceSyncJob.workspace_id` |
| `{{.JobID}}` | `WorkspaceSyncJob.job_id` |
| `{{.SourceBranch}}` | The `monofs/...` branch that was pushed |
| `{{.TargetBranch}}` | The target branch (repo's default, or `logical_branch` from the job) |
| `{{.CommitCount}}` | Number of local commits in the push |
| `{{.CommitMessages}}` | First lines of each local commit message |
| `{{.PrincipalName}}` | `requested_by_client_id` |
| `{{.RepoName}}` | Repository name parsed from clone URL |
| `{{.PushMode}}` | `"squash"` or `"preserve"` |

**Idempotency:** Before creating a PR, the provider queries open PRs matching `head={sourceBranch}` and `base={targetBranch}`. If an existing PR is found, `CreatePRResult.Created` is `false` and the existing PR URL is returned. This makes auto-push retries safe.

**Config additions (in Phase 0 flags):**

```
PRProviderEnabled        bool   // enable auto-PR creation; if false, fallback to compare URL
PRProviderType           string // "auto" (default), "github", "gitlab"
PRProviderToken          string // OAuth token or PAT (redacted in logs)
PRProviderTokenFile      string // path to file containing token (alternative to inline)
PRProviderGitLabBaseURL  string // self-hosted GitLab URL (e.g. "https://git.internal.example.com")
PRTitleTemplate          string // Go template for PR title
PRBodyTemplate           string // Go template for PR body (empty = default)
```

All fields gated behind `PRProviderEnabled`. When disabled, every push with a non-direct branch strategy emits a compare URL instead.

**Lifecycle integration:**

1. Source push job finishes successfully (fetcher reports all repos pushed).
2. For each repo where `branch_strategy` is `workspace_branch` or `per_repo_branch`:
   - Router detects provider from repo clone URL.
   - If `PRProviderEnabled=true` and token is configured for that provider:
     - Call `PullRequestProvider.Create()`. On success, emit `pr.created` audit event with PR URL. On failure, emit `pr.create_failed` audit event with error, **and** fall through to compare URL as a safety net.
   - Else (disabled or no token):
     - Emit `pr.compare_url` audit event with the deterministic compare URL.
3. PR URL (either created or compare URL) is included in the job's repository result and surfaced in CLI/UI output.

## Implementation plan

1. **Policy config schema and loader** (`internal/router/workspacepolicy/`):
   - `schema.go` — `PolicyConfig`, `Rule`, `Match` struct definitions with YAML tags
   - `loader.go` — parse YAML from `--policy-config` path, validate (unknown fields → reject, bad enum values → reject), return parsed config
   - `evaluator.go` — `Evaluate(ctx, config, request) → (Decision, error)` with first-match-wins, glob matching via `filepath.Match`, audit event emission
   - `watcher.go` — fsnotify or SIGHUP handler for hot-reload; atomically swap active config under `sync.RWMutex`
   - Wire into `RouterConfig`: `PolicyConfigPath string`
2. Insert policy evaluation before any call to fetcher stage/push RPCs.
3. Return structured denial to CLI/UI and persist `workspace.policy.denied` event.
4. Implement autopush worker in router:
   - reads pending local-commit state from durable stores
   - checks policy and running-job lock
   - enqueues push jobs via same orchestration path as manual push.
5. Add retry state tracking and dead-letter transition after max failures.
6. **Auto-PR provider integration** (`internal/router/workspacepr/`):
   - `provider.go` — `PullRequestProvider` interface and provider detection from repo clone URL
   - `github.go` — GitHub REST API v3 implementation
   - `gitlab.go` — GitLab REST API v4 implementation
   - `fallback.go` — deterministic compare URL generator for all three cases (GitHub, GitLab, unknown)
   - `template.go` — PR title/body rendering from Go templates
   - Hook into source-push completion path: after successful push, for each non-direct-branch-strategy repo → create PR or emit compare URL
   - Emit audit events (`pr.created`, `pr.create_failed`, `pr.compare_url`)
7. Add tests:
   - deny-before-write guarantee
   - no duplicate concurrent push per workspace+branch
   - backoff and pause behavior.


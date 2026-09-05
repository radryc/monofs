# MonoFS — Worker & Authz Package Documentation

This document covers every exported and unexported function in the listed files.

---

## 1. `internal/worker/client.go`

Package `worker` — a task worker that listens for pipeline tasks via the MonoFS Router's
Guardian change-event stream, claims and executes them, and writes results back.

### Types

```
Client struct {
    router   pb.MonoFSRouterClient
    token    string
    workerID string
    logger   *slog.Logger
}
```

```
TaskData struct {
    TaskID     string            `json:"task_id"`
    RunID      string            `json:"run_id"`
    JobName    string            `json:"job_name"`
    RunnerType string            `json:"runner_type"`
    Steps      []StepData        `json:"steps"`
    TimeoutSec int               `json:"timeout_sec"`
    MaxRetries int               `json:"max_retries"`
}
```

```
StepData struct {
    Name string            `json:"name"`
    Run  string            `json:"run"`
    Uses string            `json:"uses"`
    With map[string]string `json:"with"`
    ID   string            `json:"id"`
}
```

```
ResultData struct {
    TaskID    string `json:"task_id"`
    RunID     string `json:"run_id"`
    JobName   string `json:"job_name"`
    State     string `json:"state"`
    ExitCode  int    `json:"exit_code"`
    Error     string `json:"error,omitempty"`
    StartedAt string `json:"started_at"`
    EndedAt   string `json:"ended_at"`
    WorkerID  string `json:"worker_id"`
}
```

```
ClaimData struct {
    TaskID    string `json:"task_id"`
    RunID     string `json:"run_id"`
    JobName   string `json:"job_name"`
    WorkerID  string `json:"worker_id"`
    ClaimedAt string `json:"claimed_at"`
}
```

```
Handler interface {
    Execute(ctx context.Context, task *TaskData, logWriter io.Writer) (int, error)
}
```

```
Worker struct {
    client      *Client
    handler     Handler
    concurrency int
    logger      *slog.Logger
    sem         chan struct{}
}
```

```
discardWriter struct{}
```

### Constants

```
initialBackoff = time.Second
maxBackoff     = time.Minute
```

---

### `NewClient(conn grpc.ClientConnInterface, token, workerID string, logger *slog.Logger) (*Client, error)`

**What it does:** Creates a worker `Client` that communicates with the MonoFS Router
over gRPC.

**Parameters:**
- `conn` — a gRPC client connection interface.
- `token` — guardian authentication token.
- `workerID` — unique worker identifier; auto-generates one from the current nanosecond
  timestamp if empty.
- `logger` — structured logger.

**Returns:** A new `*Client` and nil error (always succeeds).

**Called from:** `worker.New()` (constructor), downstream tests.

**Details:** Wraps `conn` with `pb.NewMonoFSRouterClient`.

---

### `(c *Client) SubscribeChanges(ctx context.Context, prefixes []string) (<-chan *pb.GuardianChangeEvent, error)`

**What it does:** Opens a streaming subscription to Guardian change events for the
given logical-path prefixes, returning a buffered channel of events.

**Parameters:**
- `ctx` — controls subscription lifetime.
- `prefixes` — logical-path prefixes to watch (e.g. `[]string{"/.queues/pipeline"}`).

**Returns:** A receive-only channel of `*pb.GuardianChangeEvent` (buffer 256) and an
error.

**Called from:** `Worker.runOnce()`.

**Details:** Starts a goroutine that reads from the gRPC stream and forwards to the
channel. Includes inline content in the subscription request. The goroutine closes
the channel on exit. Context-cancelled and deadline-exceeded errors are logged
quietly (not as errors); `io.EOF` is also silent.

---

### `(c *Client) WritePath(ctx context.Context, logicalPath string, content []byte, expectedVersion string) error`

**What it does:** Writes content at a logical path in the MonoFS Guardian store via
the `UpsertGuardianPaths` RPC.

**Parameters:**
- `ctx` — request context.
- `logicalPath` — target path (e.g. a claim or result path).
- `content` — raw bytes to write.
- `expectedVersion` — version precondition (empty string = unconditional write).

**Returns:** The RPC error, or nil.

**Called from:** `Worker.executeTask()` (for claim writes), `Worker.writeResult()`
(for result writes).

---

### `(c *Client) WorkerID() string`

**What it does:** Returns the worker's unique identifier.

---

### `queuePrefix(runID string) string`

**Signature (unexported):** `func queuePrefix(runID string) string`

**What it does:** Builds the logical path prefix for a pipeline run's queue directory:
`"/.queues/pipeline/" + runID`.

**Called from:** `taskPath()`, `claimPath()`, `resultPath()`.

---

### `taskPath(runID, taskID string) string`

**Signature (unexported):** `func taskPath(runID, taskID string) string`

**What it does:** Constructs the logical path for a task JSON file:
`/.queues/pipeline/<runID>/tasks/<taskID>.json`.

**Called from:** (declared for future use; complement to claimPath/resultPath).

---

### `claimPath(runID, taskID string) string`

**Signature (unexported):** `func claimPath(runID, taskID string) string`

**What it does:** Constructs the logical path for a task claim file:
`/.queues/pipeline/<runID>/.claims/<taskID>.json`.

**Called from:** `Worker.executeTask()`.

---

### `resultPath(runID, taskID string) string`

**Signature (unexported):** `func resultPath(runID, taskID string) string`

**What it does:** Constructs the logical path for a task result file:
`/.queues/pipeline/<runID>/.results/<taskID>.json`.

**Called from:** `Worker.processTaskEvent()`, `Worker.executeTask()`.

---

### `isContextError(err error) bool`

**Signature (unexported):** `func isContextError(err error) bool`

**What it does:** Checks whether the error string contains `"context canceled"` or
`"context deadline exceeded"`.

**Returns:** `true` if the error indicates context cancellation or deadline.

**Called from:** `Client.SubscribeChanges()` (goroutine), `Worker.Run()`.

---

### `New(client *Client, handler Handler, concurrency int, logger *slog.Logger) *Worker`

**What it does:** Constructs a `Worker` with the given client, task handler,
concurrency limit, and logger.

**Parameters:**
- `client` — router client for subscriptions and writes.
- `handler` — implements task execution.
- `concurrency` — max concurrent task goroutines; controls the semaphore channel
  capacity.
- `logger` — structured logger.

**Returns:** A new `*Worker`.

---

### `(w *Worker) Run(ctx context.Context) error`

**What it does:** Runs the worker's event loop with exponential backoff on
disconnect. Calls `runOnce` in a retry loop; on transient stream errors, backs off
from 1s to 1min and retries. Returns immediately on context errors.

**Parameters:**
- `ctx` — controls the lifetime of the event loop.

**Returns:** The context error if cancelled/deadline exceeded; otherwise nil on
clean shutdown.

**Called from:** CLI/main entry points.

---

### `(w *Worker) runOnce(ctx context.Context) error`

**Signature (unexported):** `func (w *Worker) runOnce(ctx context.Context) error`

**What it does:** Subscribes to pipeline change events and processes incoming ADDED
events whose logical path contains `"/tasks/"`. Blocks until the event stream ends
or context is cancelled.

**Parameters:** `ctx` — subscription context.

**Returns:** Error from subscription failure, stream closure, or context
cancellation.

**Called from:** `Worker.Run()`.

**Details:** Filters events to `ChangeType_ADDED` only. Skips events whose path does
not contain `"/tasks/"`.

---

### `(w *Worker) processTaskEvent(ctx context.Context, event *pb.GuardianChangeEvent)`

**Signature (unexported):** `func (w *Worker) processTaskEvent(ctx context.Context, event *pb.GuardianChangeEvent)`

**What it does:** Parses a task from a change event, acquires a concurrency slot
via the semaphore, and spawns a goroutine to execute it.

**Parameters:**
- `ctx` — parent context.
- `event` — the Guardian change event (ADDED type, with inline content).

**Details:**
- Extracts `runID` and `taskID` from the logical path:
  strips prefix `"/.queues/pipeline/"`, splits on `"/"`, expects at least 3 parts.
- Unmarshals inline content into `TaskData`.
- Acquires a semaphore slot (blocks if at concurrency limit).
- Recovers from panics inside the goroutine and writes a failure result.
- Calls `executeTask`.

**Called from:** `Worker.runOnce()`.

---

### `(w *Worker) executeTask(ctx context.Context, runID, taskID string, task *TaskData)`

**Signature (unexported):** `func (w *Worker) executeTask(ctx context.Context, runID, taskID string, task *TaskData)`

**What it does:** Claims a task by writing a claim file (optimistic lock via
Guardian), then executes it with a timeout derived from `task.TimeoutSec`, and
writes the result.

**Parameters:**
- `ctx` — parent context.
- `runID` — pipeline run identifier.
- `taskID` — task identifier.
- `task` — parsed task data.

**Details:**
- Writes a `ClaimData` JSON file at the claim path; if the write fails (already
  claimed), the task is silently skipped.
- Creates a child context with timeout.
- Calls `handler.Execute()` with a `discardWriter` (output is discarded; production
  handlers write their own logs).
- Sets state to `"succeeded"` or `"failed"` based on the exit code and error.
- Writes `ResultData` via `writeResult`.

**Called from:** `Worker.processTaskEvent()` goroutine.

---

### `(w *Worker) writeResult(ctx context.Context, path string, result ResultData)`

**Signature (unexported):** `func (w *Worker) writeResult(ctx context.Context, path string, result ResultData)`

**What it does:** Marshals a `ResultData` struct to JSON and writes it to the given
logical path via the Guardian client.

**Called from:** `Worker.executeTask()` and the panic recovery in
`Worker.processTaskEvent()`.

---

### `(d discardWriter) Write(p []byte) (int, error)`

**Signature (unexported):** `func (d discardWriter) Write(p []byte) (int, error)`

**What it does:** Discards all written bytes; satisfies `io.Writer`. Always returns
`len(p), nil`.

**Details:** Implements `io.Writer`. Used as the log writer for task execution in
the worker loop where step output is not collected.

---

## 2. `internal/worker/handler.go`

Package `worker` — task execution handlers implementing the `Handler` interface.

### Types

```
BuilderHandler struct {
    mountPath string
    logger    *slog.Logger
}
```

```
DockerHandler struct {
    logger *slog.Logger
}
```

```
DeployerHandler struct {
    mountPath string
    logger    *slog.Logger
}
```

```
stepLogBuffer struct {
    buf bytes.Buffer
}
```

---

### `NewBuilderHandler(mountPath string, logger *slog.Logger) *BuilderHandler`

**What it does:** Creates a `BuilderHandler` that executes CI pipeline steps with
the FUSE mount path as the working directory.

**Parameters:**
- `mountPath` — filesystem mount point for workspace access.
- `logger` — structured logger.

---

### `(h *BuilderHandler) Execute(ctx context.Context, task *TaskData, logWriter io.Writer) (int, error)`

**What it does:** Implements `Handler.Execute`. Iterates over `task.Steps` and
executes each step sequentially via `executeStep`.

**Returns:** The exit code and error from the first failing step, or `0, nil` on
success.

---

### `(h *BuilderHandler) executeStep(ctx context.Context, step StepData, logWriter io.Writer) (int, error)`

**Signature (unexported):** `func (h *BuilderHandler) executeStep(ctx context.Context, step StepData, logWriter io.Writer) (int, error)`

**What it does:** Dispatches a step to either a builtin action (if `step.Uses` is
set) or a shell command (if `step.Run` is set).

**Returns:** Error if neither is set (`"step has no run or uses directive"`).

**Called from:** `BuilderHandler.Execute()`.

---

### `(h *BuilderHandler) executeShell(ctx context.Context, cmdStr string, logWriter io.Writer) (int, error)`

**Signature (unexported):** `func (h *BuilderHandler) executeShell(ctx context.Context, cmdStr string, logWriter io.Writer) (int, error)`

**What it does:** Runs a shell command in the mount path directory. Respects the
`SHELL` environment variable for the shell binary (defaults to `/bin/sh`).

**Parameters:**
- `ctx` — context for command cancellation.
- `cmdStr` — command string passed to `sh -c`.
- `logWriter` — receives stderr/stdout output via `io.MultiWriter`.

**Returns:** Exit code and error. Extracts `ExitCode()` from `exec.ExitError`.

**Called from:** `BuilderHandler.executeStep()`.

---

### `(h *BuilderHandler) executeBuiltin(ctx context.Context, step StepData, logWriter io.Writer) (int, error)`

**Signature (unexported):** `func (h *BuilderHandler) executeBuiltin(ctx context.Context, step StepData, logWriter io.Writer) (int, error)`

**What it does:** Dispatches to a builtin action based on the `step.Uses` field.
Strips the `"monofs/"` prefix and matches against known builtins.

**Returns:** Error for unknown builtin actions.

**Called from:** `BuilderHandler.executeStep()`.

**Known builtins:**
- `affected@v1` / `affected` → `builtinAffected`
- `checkout*` (prefix match) → `builtinCheckout`

---

### `(h *BuilderHandler) builtinAffected(ctx context.Context, step StepData, logWriter io.Writer) (int, error)`

**Signature (unexported):** `func (h *BuilderHandler) builtinAffected(ctx context.Context, step StepData, logWriter io.Writer) (int, error)`

**What it does:** Reads and outputs the `monofs-packages.yaml` file content,
showing which packages are affected by the change. The packages file path is read
from `step.With["packages"]` (relative to `mountPath`), defaulting to
`<mountPath>/monofs-packages.yaml`.

**Output:** Emits GitHub Actions-style group annotations (`::group::`).

**Called from:** `BuilderHandler.executeBuiltin()`.

---

### `(h *BuilderHandler) builtinCheckout(ctx context.Context, step StepData, logWriter io.Writer) (int, error)`

**Signature (unexported):** `func (h *BuilderHandler) builtinCheckout(ctx context.Context, step StepData, logWriter io.Writer) (int, error)`

**What it does:** Emits a notice with the mount path (source location). Always
succeeds.

**Called from:** `BuilderHandler.executeBuiltin()`.

---

### `(b *stepLogBuffer) Write(p []byte) (int, error)`

**Signature (unexported):** `func (b *stepLogBuffer) Write(p []byte) (int, error)`

**What it does:** Appends data to an internal bytes buffer. Implements `io.Writer`.

**Called from:** `BuilderHandler.executeShell()` as a multi-writer target.

---

### `(b *stepLogBuffer) String() string`

**Signature (unexported):** `func (b *stepLogBuffer) String() string`

**What it does:** Returns the captured buffer content as a string.

---

### `NewDockerHandler(logger *slog.Logger) *DockerHandler`

**What it does:** Creates a `DockerHandler` that runs shell steps. Intended for
containerized execution environments.

---

### `(h *DockerHandler) Execute(ctx context.Context, task *TaskData, logWriter io.Writer) (int, error)`

**What it does:** Implements `Handler.Execute`. Runs each step's `Run` command via
`/bin/sh -c`. Skips steps without a `Run` directive. Does not set a working
directory (runs in the current directory).

**Returns:** Exit code and error from the first failing command.

---

### `NewDeployerHandler(mountPath string, logger *slog.Logger) *DeployerHandler`

**What it does:** Creates a `DeployerHandler` that runs deployment shell steps in
the mount path.

---

### `(h *DeployerHandler) Execute(ctx context.Context, task *TaskData, logWriter io.Writer) (int, error)`

**What it does:** Implements `Handler.Execute`. Runs each step's `Run` command via
`/bin/sh -c` with the mount path as the working directory. Skips steps without a
`Run` directive.

**Returns:** Exit code and error from the first failing command.

---

## 3. `pkg/authz/authz.go`

Package `authz` — shared identity and authorization primitives (actions, roles,
identity model).

### Types

```
Action string        // coarse capability: "view", "ingest", "modify"
Role   string        // named bundle of actions: "viewer", "ingester", "maintainer", "admin"

Identity struct {
    Subject  string   // OIDC "sub"
    Email    string
    Groups   []string // IdP group memberships
    ClientID string   // machine principal (OAuth client)
}
```

### Constants

```
ActionView   Action = "view"
ActionIngest Action = "ingest"
ActionModify Action = "modify"

RoleViewer     Role = "viewer"
RoleIngester   Role = "ingester"
RoleMaintainer Role = "maintainer"
RoleAdmin      Role = "admin"
```

### Package-level variable

```
roleActions = map[Role]map[Action]bool{
    RoleViewer:     {ActionView: true},
    RoleIngester:   {ActionView: true, ActionIngest: true},
    RoleMaintainer: {ActionView: true, ActionIngest: true, ActionModify: true},
    RoleAdmin:      {ActionView: true, ActionIngest: true, ActionModify: true},
}
```

---

### `(r Role) Allows(action Action) bool`

**What it does:** Reports whether the role permits the given action by looking up
`roleActions`.

**Called from:** `GrantStore.Can()` during grant evaluation.

---

### `(r Role) Valid() bool`

**What it does:** Reports whether the role is a recognized value (exists in
`roleActions`).

**Called from:** `Grant.Validate()`.

---

### `(id Identity) IsAnonymous() bool`

**What it does:** Reports whether the identity carries no verified principal
(both `Subject` and `ClientID` are empty).

**Called from:** Multiple places: `GrantStore.Can()`, `ownerRefMatchesIdentity()`,
`WebAuthenticator.Handler()`.

---

### `(id Identity) HasGroup(group string) bool`

**What it does:** Case-insensitively checks if the identity belongs to the given
IdP group. Trims whitespace from `group` and each entry in `id.Groups`.

**Called from:** `Grant.matchesIdentity()`, `ownerRefMatchesIdentity()`.

---

### `(id Identity) PrincipalID() string`

**What it does:** Returns a stable principal identifier, preferring `ClientID`
(machine) over `Subject` (human). Returns `""` when anonymous.

**Called from:** `Authenticator.authenticate()` (debug logging).

---

## 4. `pkg/authz/compiler.go`

Compiles OWNERS files into `Grant` objects, resolving team handles to IdP groups.

### Types

```
TeamMapping map[string]string      // team handle → IdP group name

teamMappingYAML struct {            // YAML wire format
    Version int               `yaml:"version"`
    Teams   map[string]string `yaml:"teams"`
}
```

---

### `ParseTeamMapping(data []byte) (TeamMapping, error)`

**What it does:** Parses a YAML team-to-group mapping document (version 1 required).

**Parameters:** `data` — raw YAML bytes.

**Returns:** A `TeamMapping` map, or error for parse failures or unsupported
versions.

---

### `(m TeamMapping) GroupFor(team string) (group string, ok bool)`

**What it does:** Resolves a team handle to its IdP group name. If no mapping
exists, returns the handle itself with `ok=false` so callers can surface a warning.

**Called from:** `CompileGrants()`, `ownerRefMatchesIdentity()`.

---

### `CompileGrants(partition string, owners *OwnersFile, mapping TeamMapping) (grants []Grant, warnings []string)`

**What it does:** Converts a partition's OWNERS file into a slice of `Grant`
objects. Team references are resolved to IdP groups via `mapping`; subject
references become grants keyed by `Subject`. Grants are emitted in deterministic
role order: viewer, ingester, maintainer.

**Parameters:**
- `partition` — partition name.
- `owners` — parsed OWNERS file (nil returns nil, nil).
- `mapping` — team-to-group mapping.

**Returns:** A slice of `Grant` and any warnings for unmapped teams.

---

## 5. `pkg/authz/device.go`

OAuth 2.0 Device Authorization Grant (RFC 8628) for CLI login.

### Types

```
OAuthEndpoints struct {
    DeviceAuthURL string
    TokenURL      string
    AuthURL       string
}

TokenResponse struct {
    AccessToken  string `json:"access_token"`
    TokenType    string `json:"token_type"`
    RefreshToken string `json:"refresh_token,omitempty"`
    IDToken      string `json:"id_token,omitempty"`
    ExpiresIn    int    `json:"expires_in,omitempty"`
    Scope        string `json:"scope,omitempty"`
    ObtainedAt   int64  `json:"obtained_at,omitempty"`
}

DeviceAuthResponse struct {
    DeviceCode              string `json:"device_code"`
    UserCode                string `json:"user_code"`
    VerificationURI         string `json:"verification_uri"`
    VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
    ExpiresIn               int    `json:"expires_in"`
    Interval                int    `json:"interval"`
}

DeviceFlowClient struct {
    ClientID     string
    ClientSecret string
    Scopes       []string
    Endpoints    OAuthEndpoints
    HTTPClient   *http.Client
    sleep        func(time.Duration)
    now          func() time.Time
}
```

---

### `NewDeviceFlowClient(clientID string, scopes []string, endpoints OAuthEndpoints) *DeviceFlowClient`

**What it does:** Builds a device-flow client with an HTTP client (15s timeout),
real `time.Sleep`, and real `time.Now` (injectable for tests).

---

### `(c *DeviceFlowClient) Authorize(ctx context.Context) (*DeviceAuthResponse, error)`

**What it does:** Sends a POST to the device authorization endpoint with
`client_id`, optional `client_secret`, and scopes. Returns the device code, user
code, and polling interval. Defaults polling interval to 5s if the IdP returns
zero.

**Called from:** CLI login commands (`guardianctl`, `monofs-admin`).

---

### `(c *DeviceFlowClient) Poll(ctx context.Context, dev *DeviceAuthResponse) (*TokenResponse, error)`

**What it does:** Polls the token endpoint at the device-code interval until the
user approves, the code expires, or access is denied. Handles standard error
responses: `authorization_pending` (keep waiting), `slow_down` (increase interval
by 5s), `access_denied`, `expired_token`.

**Parameters:**
- `ctx` — polling context.
- `dev` — device authorization response from `Authorize`.

**Returns:** The token response or an error.

**Called from:** CLI login commands after `Authorize`.

---

### `(c *DeviceFlowClient) postForm(ctx context.Context, endpoint string, form url.Values) (*http.Response, error)`

**Signature (unexported):** `func (c *DeviceFlowClient) postForm(ctx context.Context, endpoint string, form url.Values) (*http.Response, error)`

**What it does:** Sends an HTTP POST with form-urlencoded body and JSON accept
header.

**Called from:** `Authorize()` and `Poll()`.

---

### `SaveTokenFile(path string, tok *TokenResponse) error`

**What it does:** Persists a `TokenResponse` to disk as JSON with `0600`
permissions. Creates parent directories with `0700`. Uses atomic write via temp
file + rename.

**Called from:** CLI login commands after obtaining a token.

---

### `LoadTokenFile(path string) (*TokenResponse, error)`

**What it does:** Reads and decodes a previously saved `TokenResponse` from disk.

**Called from:** CLI bootstrap to check for existing credentials.

---

## 6. `pkg/authz/grants.go`

Grant evaluation engine — stores, validates, and checks grants.

### Types

```
Grant struct {
    Subject   string `json:"subject,omitempty"`   // individual principal
    Group     string `json:"group,omitempty"`     // IdP group
    Partition string `json:"partition"`           // partition or "*"
    Role      Role   `json:"role"`
}

GrantStore struct {
    mu          sync.RWMutex
    grants      []Grant
    persistPath string
}
```

### Constants

```
WildcardPartition = "*"
```

---

### `(g Grant) Validate() error`

**What it does:** Validates that exactly one of `Subject` or `Group` is set,
`Partition` is non-empty, and `Role` is valid.

**Called from:** `GrantStore.Add()`, `GrantStore.Replace()`.

---

### `(g Grant) matchesPartition(partition string) bool`

**Signature (unexported):** `func (g Grant) matchesPartition(partition string) bool`

**What it does:** Returns true if the grant's partition equals the given partition
or is the wildcard `"*"`.

**Called from:** `GrantStore.Can()`.

---

### `(g Grant) matchesIdentity(id Identity) bool`

**Signature (unexported):** `func (g Grant) matchesIdentity(id Identity) bool`

**What it does:** Checks if `id` satisfies the grant:
- For subject grants: matches `Subject`, `ClientID`, or `Email`.
- For group grants: uses `id.HasGroup()`.

**Called from:** `GrantStore.Can()`.

---

### `NewGrantStore(persistPath string) (*GrantStore, error)`

**What it does:** Creates a `GrantStore`, optionally backed by a JSON file. When
`persistPath` is non-empty, loads existing grants and creates parent directories if
needed. Returns nil error if the file doesn't exist (treated as empty store).

---

### `(s *GrantStore) load() error`

**Signature (unexported):** `func (s *GrantStore) load() error`

**What it does:** Reads and JSON-decodes grants from `persistPath`. Returns nil if
the file does not exist.

**Called from:** `NewGrantStore()`.

---

### `(s *GrantStore) saveLocked() error`

**Signature (unexported):** `func (s *GrantStore) saveLocked() error`

**What it does:** JSON-encodes and writes grants to `persistPath` using atomic
temp-file + rename. No-op when `persistPath` is empty. Caller must hold the write
lock.

**Called from:** `Add()` and `Replace()`.

---

### `(s *GrantStore) Add(g Grant) error`

**What it does:** Validates and appends a grant, then persists. Thread-safe.

---

### `(s *GrantStore) Replace(grants []Grant) error`

**What it does:** Atomically replaces all grants after validating each one, then
persists. Used by the OWNERS policy compiler during policy refresh.

---

### `(s *GrantStore) Grants() []Grant`

**What it does:** Returns a copy of the current grants. Thread-safe.

---

### `(s *GrantStore) Can(_ context.Context, id Identity, partition string, action Action) bool`

**What it does:** Implements `GrantEvaluator`. Iterates over all grants and returns
true if any grant matches both the identity and partition and its role permits the
action. Returns false for anonymous identities.

**Called from:** gRPC/HTTP authorization interceptors, control-plane components.

---

## 7. `pkg/authz/interceptor.go`

gRPC and HTTP authentication interceptors that extract and verify bearer tokens.

### Types

```
identityContextKey struct{}           // private context key

Authenticator struct {
    Verifier     TokenVerifier
    Logger       *slog.Logger
    RequireToken bool
    Observe      func(outcome string)
}

identityServerStream struct {
    grpc.ServerStream
    ctx context.Context
}
```

---

### `ContextWithIdentity(ctx context.Context, id Identity) context.Context`

**What it does:** Returns a child context annotated with the given `Identity`.

**Called from:** `Authenticator.authenticate()`, `WebAuthenticator.Handler()`,
`WebAuthenticator.Middleware()`.

---

### `IdentityFromContext(ctx context.Context) (Identity, bool)`

**What it does:** Extracts the authenticated `Identity` from the context. Returns
`false` if none is present.

**Called from:** Authorization check sites throughout the codebase.

---

### `NewAuthenticator(verifier TokenVerifier, logger *slog.Logger, requireToken bool) *Authenticator`

**What it does:** Builds an `Authenticator`. Falls back to `NoopVerifier` if
`verifier` is nil, and to a discard logger if `logger` is nil.

**Parameters:**
- `verifier` — token verifier.
- `logger` — structured logger.
- `requireToken` — if true, missing/invalid tokens are rejected (ENFORCE mode);
  otherwise they yield anonymous identities (OBSERVE mode).

---

### `(a *Authenticator) observe(outcome string)`

**Signature (unexported):** `func (a *Authenticator) observe(outcome string)`

**What it does:** Calls the optional `Observe` hook with the outcome label
(`"authenticated"`, `"anonymous"`, `"rejected"`) for metrics.

---

### `(a *Authenticator) authenticate(ctx context.Context, rawToken, source string) (context.Context, error)`

**Signature (unexported):** `func (a *Authenticator) authenticate(ctx context.Context, rawToken, source string) (context.Context, error)`

**What it does:** Verifies a raw bearer token. In OBSERVE mode (`RequireToken =
false`) it never returns an error — invalid/missing tokens yield an anonymous
`Identity`. In ENFORCE mode, missing or invalid tokens return gRPC
`Unauthenticated` errors. On success, attaches the `Identity` to the returned
context.

**Parameters:**
- `ctx` — parent context.
- `rawToken` — bearer token string.
- `source` — descriptive label for logging (e.g. `"grpc:/monofs.Service/Method"`).

**Called from:** `UnaryServerInterceptor()`, `StreamServerInterceptor()`,
`HTTPMiddleware()`.

---

### `(a *Authenticator) UnaryServerInterceptor() grpc.UnaryServerInterceptor`

**What it does:** Returns a gRPC unary interceptor that authenticates each call
before forwarding to the handler.

---

### `(a *Authenticator) StreamServerInterceptor() grpc.StreamServerInterceptor`

**What it does:** Returns a gRPC stream interceptor that wraps the server stream
with an `identityServerStream` so the authenticated context is visible to
downstream handlers.

---

### `(a *Authenticator) HTTPMiddleware(next http.Handler) http.Handler`

**What it does:** Returns an HTTP middleware that extracts the bearer token from
the `Authorization` header, verifies it, and attaches the `Identity` to the
request context. In ENFORCE mode, invalid/missing tokens return HTTP 401.

---

### `(s *identityServerStream) Context() context.Context`

**What it does:** Overrides the gRPC `ServerStream.Context()` to return the
authenticated context.

---

### `bearerFromMetadata(ctx context.Context) string`

**Signature (unexported):** `func bearerFromMetadata(ctx context.Context) string`

**What it does:** Reads a bearer token from the gRPC `"authorization"` metadata.

**Called from:** `UnaryServerInterceptor()`, `StreamServerInterceptor()`.

---

### `bearerFromHeader(header string) string`

**Signature (unexported):** `func bearerFromHeader(header string) string`

**What it does:** Parses a bearer token from an HTTP `Authorization` header value.

**Called from:** `HTTPMiddleware()`, `WebAuthenticator.identityFromRequest()`.

---

### `parseBearer(header string) string`

**Signature (unexported):** `func parseBearer(header string) string`

**What it does:** Extracts the token from a `"Bearer <token>"` header value
(case-insensitive prefix match). Returns `""` if the header is empty or doesn't
start with `"bearer "`.

**Called from:** `bearerFromMetadata()`, `bearerFromHeader()`.

---

## 8. `pkg/authz/machine.go`

Machine/service token support — client credentials grant and static token
verification.

### Types

```
ClientCredentialsClient struct {
    ClientID      string
    ClientSecret  string
    Scopes        []string
    TokenURL      string
    HTTPClient    *http.Client
    RefreshBuffer time.Duration
    now           func() time.Time
    mu            sync.Mutex
    cached        *TokenResponse
    expiresAt     time.Time
}

StaticTokenVerifier struct {
    mu     sync.RWMutex
    tokens map[string]Identity
}

ChainVerifier struct {
    verifiers []TokenVerifier
}
```

### Constants

```
defaultRefreshBuffer = 30 * time.Second
```

---

### `(c *ClientCredentialsClient) Token(ctx context.Context) (*TokenResponse, error)`

**What it does:** Obtains an access token via the OAuth 2.0 client-credentials
grant. POSTs `grant_type=client_credentials` with client ID/secret to the token URL.

**Returns:** A `TokenResponse` with `ObtainedAt` set.

**Called from:** `CachedToken()` and directly for one-off token requests.

---

### `(c *ClientCredentialsClient) CachedToken(ctx context.Context) (*TokenResponse, error)`

**What it does:** Returns a cached access token, refreshing it when expired or
nearing expiry (within the refresh buffer). Concurrent calls are serialized;
only one refresh happens at a time.

**Returns:** Cached or freshly obtained token, or an error.

---

### `(c *ClientCredentialsClient) nowFunc() time.Time`

**Signature (unexported):** `func (c *ClientCredentialsClient) nowFunc() time.Time`

**What it does:** Returns the current time via the injectable `now` function,
falling back to `time.Now`.

---

### `(c *ClientCredentialsClient) refreshBuffer() time.Duration`

**Signature (unexported):** `func (c *ClientCredentialsClient) refreshBuffer() time.Duration`

**What it does:** Returns the configured refresh buffer, defaulting to 30s.

---

### `(c *ClientCredentialsClient) isExpired() bool`

**Signature (unexported):** `func (c *ClientCredentialsClient) isExpired() bool`

**What it does:** Returns true if the current time is past `expiresAt`.

---

### `(c *ClientCredentialsClient) computeExpiry(tok *TokenResponse) time.Time`

**Signature (unexported):** `func (c *ClientCredentialsClient) computeExpiry(tok *TokenResponse) time.Time`

**What it does:** Computes the effective expiry time by subtracting the refresh
buffer from the token's `ExpiresIn`. If `ExpiresIn` is zero or negative, returns
now (treats token as single-use, always refreshes). If the buffered expiry is
already in the past, returns the unbuffered expiry.

---

### `NewStaticTokenVerifier() *StaticTokenVerifier`

**What it does:** Creates an empty static token verifier. Tokens are stored as
SHA-256 hashes, not in plaintext.

---

### `(v *StaticTokenVerifier) Add(token string, id Identity)`

**What it does:** Registers an opaque bearer token → Identity mapping. The token
is hashed with SHA-256 before storage. Empty tokens are silently ignored.

---

### `(v *StaticTokenVerifier) Verify(_ context.Context, rawToken string) (Identity, error)`

**What it does:** Implements `TokenVerifier`. Hashes the raw token and looks up the
corresponding identity. Returns error for empty or unknown tokens.

---

### `hashToken(token string) string`

**Signature (unexported):** `func hashToken(token string) string`

**What it does:** Returns the hex-encoded SHA-256 hash of the token string.

**Called from:** `StaticTokenVerifier.Add()`, `StaticTokenVerifier.Verify()`.

---

### `NewBreakGlassVerifier(adminToken, clientID string) *StaticTokenVerifier`

**What it does:** Creates a `StaticTokenVerifier` preloaded with a single
break-glass admin token that resolves to the given `clientID`. Intended for the
`MONOFS_TOKEN` emergency access path. Every use should be audit-logged via the
interceptor's `Observe` hook.

---

### `NewChainVerifier(verifiers ...TokenVerifier) *ChainVerifier`

**What it does:** Builds a verifier chain that tries each provided verifier in
order. Nil entries are skipped.

---

### `(c *ChainVerifier) Verify(ctx context.Context, rawToken string) (Identity, error)`

**What it does:** Implements `TokenVerifier`. Tries verifiers sequentially; the
first that returns a non-anonymous identity without error wins. If none succeed,
returns the last error (or a generic error if no verifier produced an error).

---

## 9. `pkg/authz/oidc.go`

OIDC JWT access token verification with JWKS caching.

### Types

```
OIDCConfig struct {
    Issuer            string
    Audience          string
    JWKSURL           string
    GroupsClaim       string
    EmailClaim        string
    ClientIDClaim     string
    RefreshInterval   time.Duration
    AllowedAlgorithms []jose.SignatureAlgorithm
    HTTPClient        *http.Client
    TokenCacheTTL     time.Duration
    TokenCacheSize    int
    now               func() time.Time
}

OIDCVerifier struct {
    cfg        OIDCConfig
    jwksURL    string
    mu         sync.RWMutex
    keys       *jose.JSONWebKeySet
    fetchedAt  time.Time
    tokenMu    sync.RWMutex
    tokenCache map[string]cachedIdentity
}

cachedIdentity struct {
    identity  Identity
    expiresAt time.Time
}
```

### Package-level defaults (constants)

```
defaultGroupsClaim     = "groups"
defaultEmailClaim      = "email"
defaultClientIDClaim   = "client_id"
defaultRefreshInterval = 15 * time.Minute
defaultTokenCacheTTL   = 5 * time.Minute
defaultTokenCacheSize  = 256
```

### Package-level variable

```
defaultAlgorithms = []jose.SignatureAlgorithm{
    jose.RS256, jose.RS384, jose.RS512,
    jose.ES256, jose.ES384, jose.ES512,
    jose.PS256,
}
```

---

### `NewOIDCVerifier(cfg OIDCConfig) (*OIDCVerifier, error)`

**What it does:** Validates and fills defaults for an OIDC config, returning a
verifier. Requires `Issuer` and `Audience`. JWKS is fetched lazily on the first
`Verify` call. Sets defaults for all optional fields including algorithm whitelist,
HTTP client (10s timeout), token cache TTL (5min) and size (256).

---

### `(v *OIDCVerifier) Verify(ctx context.Context, rawToken string) (Identity, error)`

**What it does:** Implements `TokenVerifier`. Verifies an OIDC JWT access token:

1. Checks the token cache (avoids expensive crypto on cache hit).
2. Parses the JWT, extracting the KID from the first header.
3. Looks up the matching JWKS key (refreshing the JWKS cache if needed).
4. Validates the signature and standard claims (issuer, audience, expiry).
5. Extracts `Identity` from claims: Subject, Email, Groups, ClientID (with `azp`
   fallback).
6. Caches the verified identity.

---

### `(v *OIDCVerifier) keyForKID(ctx context.Context, kid string) (jose.JSONWebKey, error)`

**Signature (unexported):** `func (v *OIDCVerifier) keyForKID(ctx context.Context, kid string) (jose.JSONWebKey, error)`

**What it does:** Looks up a JWKS key by KID. On cache miss, refreshes the JWKS
and retries the lookup.

**Called from:** `Verify()`.

---

### `(v *OIDCVerifier) lookup(kid string) (jose.JSONWebKey, bool)`

**Signature (unexported):** `func (v *OIDCVerifier) lookup(kid string) (jose.JSONWebKey, bool)`

**What it does:** Searches the cached JWKS for a key matching `kid`. If the cache
is stale (older than `RefreshInterval`), returns false to force a refresh. Also
handles the case of a single unnamed key (no KID) as a fallback match.

**Called from:** `keyForKID()`.

---

### `(v *OIDCVerifier) refresh(ctx context.Context) error`

**Signature (unexported):** `func (v *OIDCVerifier) refresh(ctx context.Context) error`

**What it does:** Resolves the JWKS URL (via discovery if needed), fetches the key
set, and updates the cache.

**Called from:** `keyForKID()`.

---

### `(v *OIDCVerifier) resolveJWKSURL(ctx context.Context) (string, error)`

**Signature (unexported):** `func (v *OIDCVerifier) resolveJWKSURL(ctx context.Context) (string, error)`

**What it does:** Returns the configured JWKS URL, or discovers it from the
issuer's `/.well-known/openid-configuration` document.

**Called from:** `refresh()`.

---

### `(v *OIDCVerifier) fetchJWKS(ctx context.Context, jwksURL string) (*jose.JSONWebKeySet, error)`

**Signature (unexported):** `func (v *OIDCVerifier) fetchJWKS(ctx context.Context, jwksURL string) (*jose.JSONWebKeySet, error)`

**What it does:** Fetches and JSON-decodes the JWKS from the given URL. Returns
error if the response is not 200 or contains zero keys.

**Called from:** `refresh()`.

---

### `stringClaim(claims map[string]any, name string) string`

**Signature (unexported):** `func stringClaim(claims map[string]any, name string) string`

**What it does:** Extracts a string value from a JWT custom claims map. Returns ""
if not present or not a string.

**Called from:** `Verify()`, `clientIDClaim()`.

---

### `clientIDClaim(claims map[string]any, name string) string`

**Signature (unexported):** `func clientIDClaim(claims map[string]any, name string) string`

**What it does:** Extracts a client ID from custom claims. First tries the named
claim, then falls back to `"azp"` (authorized party).

**Called from:** `Verify()`.

---

### `stringSliceClaim(claims map[string]any, name string) []string`

**Signature (unexported):** `func stringSliceClaim(claims map[string]any, name string) []string`

**What it does:** Extracts a string slice from JWT claims. Handles three formats:
`[]string`, `[]any` (converts stringable items), and `string` (space-delimited).

**Called from:** `Verify()`.

---

### `(v *OIDCVerifier) tokenCacheLookup(rawToken string) (Identity, bool)`

**Signature (unexported):** `func (v *OIDCVerifier) tokenCacheLookup(rawToken string) (Identity, bool)`

**What it does:** Looks up a cached `Identity` for the raw token. If the cached
entry is expired, evicts it and returns false.

**Called from:** `Verify()`.

---

### `(v *OIDCVerifier) tokenCacheStore(rawToken string, id Identity, tokenExpiry *jwt.NumericDate)`

**Signature (unexported):** `func (v *OIDCVerifier) tokenCacheStore(rawToken string, id Identity, tokenExpiry *jwt.NumericDate)`

**What it does:** Caches a verified `Identity`. The effective TTL is the minimum of
`TokenCacheTTL` and the token's remaining lifetime. When the cache is full, expired
entries are evicted; if still full, the write is dropped (re-verification on next
request).

**Called from:** `Verify()`.

---

### `(v *OIDCVerifier) evictExpired()`

**Signature (unexported):** `func (v *OIDCVerifier) evictExpired()`

**What it does:** Removes all expired entries from the token cache. Caller must
hold the `tokenMu` write lock.

**Called from:** `tokenCacheStore()`.

---

## 10. `pkg/authz/owners.go`

Parsing and representation of OWNERS files.

### Types

```
OwnerRef struct {
    Team    string   // "@team" — without the "@"
    Subject string   // bare principal id or email
}

OwnersFile struct {
    Version     int
    Viewers     []OwnerRef
    Ingesters   []OwnerRef
    Maintainers []OwnerRef
}

ownersYAML struct {                  // YAML wire format
    Version     int      `yaml:"version"`
    Viewers     []string `yaml:"viewers"`
    Ingesters   []string `yaml:"ingesters"`
    Maintainers []string `yaml:"maintainers"`
}
```

### Constants

```
OwnersFileName = "OWNERS"
OwnersDir      = ".guardian"
```

---

### `(r OwnerRef) IsTeam() bool`

**What it does:** Returns true if the reference is a team handle
(`r.Team != ""`).

---

### `(r OwnerRef) String() string`

**What it does:** Renders the reference in OWNERS syntax: `"@team"` for teams,
bare string for subjects.

**Called from:** `OwnershipResolver.OwnersOf()` for deduplication keys.

---

### `parseOwnerRef(raw string) (OwnerRef, error)`

**Signature (unexported):** `func parseOwnerRef(raw string) (OwnerRef, error)`

**What it does:** Parses a single OWNERS entry string. Entries starting with `"@"`
become `Team` refs; everything else becomes `Subject` refs. Rejects empty entries
and team handles with no name after `"@"`.

**Called from:** `ParseOwners()`.

---

### `(o *OwnersFile) ByRole() map[Role][]OwnerRef`

**What it does:** Returns owner references grouped by role in a map with keys
`RoleViewer`, `RoleIngester`, `RoleMaintainer`.

**Called from:** `CompileGrants()`.

---

### `ParseOwners(data []byte) (*OwnersFile, error)`

**What it does:** Parses a YAML OWNERS document. Requires version 1 and at least
one owner entry across all roles. Rejects malformed entries.

**Returns:** A parsed `*OwnersFile` or error.

---

## 11. `pkg/authz/pkce.go`

PKCE (Proof Key for Code Exchange, RFC 7636) for the browser login flow, plus
web session management.

### Types

```
PKCE struct {
    Verifier  string
    Challenge string
    Method    string   // always "S256"
}

PKCEExchangeConfig struct {
    ClientID     string
    ClientSecret string
    RedirectURI  string
    TokenURL     string
    HTTPClient   *http.Client
}

Session struct {
    ID        string
    Identity  Identity
    CreatedAt time.Time
    ExpiresAt time.Time
}

SessionStore struct {
    mu       sync.RWMutex
    sessions map[string]Session
    ttl      time.Duration
    now      func() time.Time
}

PersistentSessionStore struct {
    store *SessionStore
    dir   string
    mu    sync.Mutex
}
```

---

### `GeneratePKCE() (PKCE, error)`

**What it does:** Generates a 32-byte cryptographically random code verifier and
its S256 challenge (base64url-encoded).

**Called from:** `WebAuthenticator.LoginHandler()`.

---

### `AuthCodeURL(authURL, clientID, redirectURI, state string, scopes []string, pkce PKCE) string`

**What it does:** Builds the authorization endpoint URL with PKCE parameters
(`code_challenge`, `code_challenge_method`, `response_type=code`) and optional
scopes. Appends query parameters with correct separator (`?` or `&`).

**Called from:** `WebAuthenticator.LoginHandler()`.

---

### `ExchangeCode(ctx context.Context, cfg PKCEExchangeConfig, code, verifier string) (*TokenResponse, error)`

**What it does:** Exchanges an authorization code + PKCE verifier for tokens at the
token endpoint. Sets `ObtainedAt` on the response.

**Parameters:**
- `ctx` — request context.
- `cfg` — exchange configuration.
- `code` — authorization code from the IdP callback.
- `verifier` — the PKCE code verifier that was generated at the start of the flow.

**Returns:** A `TokenResponse` containing access/id/refresh tokens.

**Called from:** `WebAuthenticator.CallbackHandler()`.

---

### `NewSessionStore(ttl time.Duration) *SessionStore`

**What it does:** Creates an in-memory session store with the given TTL (defaults
to 12 hours if zero or negative).

---

### `(s *SessionStore) Create(id Identity) (string, error)`

**What it does:** Generates a 24-byte random session ID (base64url-encoded) and
stores a `Session` entry with the given identity and TTL-based expiry.

**Called from:** `WebAuthenticator.CallbackHandler()`, `PersistentSessionStore.Create()`.

---

### `(s *SessionStore) Get(sid string) (Identity, bool)`

**What it does:** Looks up a session by ID. Deletes the session if expired and
returns `false`.

**Called from:** `WebAuthenticator.identityFromRequest()`,
`PersistentSessionStore.Get()`.

---

### `(s *SessionStore) Delete(sid string)`

**What it does:** Removes a session by ID (logout).

**Called from:** `Get()` (when expired), `WebAuthenticator.LogoutHandler()`,
`PersistentSessionStore.Delete()`.

---

### `(s *SessionStore) Snapshot() map[string]Session`

**What it does:** Returns a copy of all active (non-expired) sessions.

**Called from:** `PersistentSessionStore.persistToDisk()`.

---

### `(s *SessionStore) Restore(sessions map[string]Session)`

**What it does:** Imports sessions, keeping only those that have not yet expired.

**Called from:** `PersistentSessionStore.restoreFromDisk()`.

---

### `NewPersistentSessionStore(dir string, ttl time.Duration) (*PersistentSessionStore, error)`

**What it does:** Creates a file-backed session store. Creates the directory (0700)
and restores sessions from `sessions.json` on startup.

---

### `(ps *PersistentSessionStore) SessionStore() *SessionStore`

**What it does:** Returns the underlying in-memory `SessionStore`.

---

### `(ps *PersistentSessionStore) sessionsPath() string`

**Signature (unexported):** `func (ps *PersistentSessionStore) sessionsPath() string`

**What it does:** Returns the path `<dir>/sessions.json`.

---

### `(ps *PersistentSessionStore) persistToDisk()`

**Signature (unexported):** `func (ps *PersistentSessionStore) persistToDisk()`

**What it does:** Snapshots active sessions and writes them to `sessions.json` via
an atomic temp-file rename. Errors are silently ignored.

**Called from:** `Create()` and `Delete()`.

---

### `(ps *PersistentSessionStore) restoreFromDisk()`

**Signature (unexported):** `func (ps *PersistentSessionStore) restoreFromDisk()`

**What it does:** Reads and restores sessions from `sessions.json`. Errors and
missing files are silently ignored.

**Called from:** `NewPersistentSessionStore()`.

---

### `(ps *PersistentSessionStore) Create(id Identity) (string, error)`

**What it does:** Creates a session and persists to disk.

---

### `(ps *PersistentSessionStore) Delete(sid string)`

**What it does:** Deletes a session and persists to disk.

---

### `(ps *PersistentSessionStore) Get(sid string) (Identity, bool)`

**What it does:** Looks up a session by ID.

---

## 12. `pkg/authz/resolver.go`

Hierarchical ownership resolution via nested OWNERS files.

### Types

```
OwnersSource interface {
    LoadOwners(ctx context.Context, dir string) (*OwnersFile, error)
}

OwnershipResolver struct {
    source  OwnersSource
    mapping TeamMapping
}
```

---

### `OwnersPath(dir string) string`

**What it does:** Returns the conventional OWNERS file path for a directory:
`"<dir>/.guardian/OWNERS"` (or `".guardian/OWNERS"` for the root).

---

### `ancestorDirs(path string) []string`

**Signature (unexported):** `func ancestorDirs(path string) []string`

**What it does:** Returns the path itself and each ancestor directory, deepest
first, ending with `""` (root). E.g. `"a/b/c"` → `["a/b/c", "a/b", "a", ""]`.

**Called from:** `IsOwner()`, `OwnsAll()` (via `IsOwner`), `OwnersOf()`.

---

### `ownerRefMatchesIdentity(ref OwnerRef, id Identity, mapping TeamMapping) bool`

**Signature (unexported):** `func ownerRefMatchesIdentity(ref OwnerRef, id Identity, mapping TeamMapping) bool`

**What it does:** Checks whether `id` satisfies an `OwnerRef`. For team refs,
resolves the team to an IdP group via `mapping` and checks group membership. For
subject refs, matches against `Subject`, `ClientID`, or `Email`. Returns false for
anonymous identities.

**Called from:** `IsOwner()`.

---

### `NewOwnershipResolver(source OwnersSource, mapping TeamMapping) *OwnershipResolver`

**What it does:** Builds an ownership resolver.

---

### `(r *OwnershipResolver) IsOwner(ctx context.Context, path string, id Identity) (owned bool, ownerDir string, err error)`

**What it does:** Walks up the directory tree from `path` through ancestors,
loading OWNERS files at each level. Returns true if `id` is listed as a maintainer
in the nearest governing OWNERS file (first match wins). `ownerDir` is the
directory whose OWNERS file granted ownership.

**Called from:** `OwnsAll()`.

---

### `(r *OwnershipResolver) OwnsAll(ctx context.Context, paths []string, id Identity) (ownsAll bool, unowned []string, err error)`

**What it does:** Checks whether `id` owns all given paths. Returns the subset of
paths that `id` does not own, which callers can route through a merge request.

---

### `(r *OwnershipResolver) OwnersOf(ctx context.Context, paths []string) ([]OwnerRef, error)`

**What it does:** Returns the distinct maintainer references governing any of the
given paths, collected from nearest and ancestor OWNERS files. Order is:
path order, then deepest-to-root, first occurrence wins (deduplication by
`OwnerRef.String()`).

**Called from:** Merge request reviewer assignment.

---

## 13. `pkg/authz/verifier.go`

Interface definitions and passthrough/no-op implementations for the authorization
framework.

### Interfaces

```
TokenVerifier interface {
    Verify(ctx context.Context, rawToken string) (Identity, error)
}

GrantEvaluator interface {
    Can(ctx context.Context, id Identity, partition string, action Action) bool
}
```

### Types

```
NoopVerifier      struct{}   // treats every token as anonymous
AllowAllEvaluator struct{}   // permits every action
DenyAllEvaluator  struct{}   // rejects every action
```

---

### `(NoopVerifier) Verify(context.Context, string) (Identity, error)`

**What it does:** Always returns an anonymous `Identity{}` and nil error. Used
during OBSERVE-mode rollout.

**Called from:** `NewAuthenticator` as the default when no verifier is provided.

---

### `(AllowAllEvaluator) Can(context.Context, Identity, string, Action) bool`

**What it does:** Always returns `true`. OBSERVE-mode default so enforcement can
be layered in without changing behavior.

---

### `(DenyAllEvaluator) Can(context.Context, Identity, string, Action) bool`

**What it does:** Always returns `false`. Safe default for tests.

---

## 14. `pkg/authz/webauth.go`

Browser-based OIDC login flow with PKCE, session cookies, and request middleware.

### Types

```
sessionStore interface {              // abstracts session storage
    Create(Identity) (string, error)
    Get(string) (Identity, bool)
    Delete(string)
}

WebAuthConfig struct {
    Issuer            string
    ClientID          string
    ClientSecret      string
    RedirectURL       string
    Scopes            []string
    Endpoints         OAuthEndpoints
    Verifier          TokenVerifier
    Sessions          sessionStore
    RoutePrefix       string
    CookieName        string
    Secure            bool
    PostLoginRedirect string
    RequireLogin      bool
    ExemptPaths       []string
    HTTPClient        *http.Client
    now               func() time.Time
}

WebAuthenticator struct {
    cfg     WebAuthConfig
    mu      sync.Mutex
    pending map[string]pendingLogin
}

pendingLogin struct {
    verifier string
    created  time.Time
}
```

### Constants

```
stateCookieName     = "monofs_oauth_state"
defaultSessionCK    = "monofs_session"
pendingLoginMaxAge  = 10 * time.Minute
```

### Package-level variable

```
defaultExemptUIPaths = []string{
    "/healthz", "/livez", "/readyz", "/-/health", "/metrics",
    "/favicon.ico", "/assets", "/static", "/api", "/debug",
}
```

---

### `DiscoverEndpoints(ctx context.Context, issuer string, client *http.Client) (OAuthEndpoints, error)`

**What it does:** Fetches the `.well-known/openid-configuration` document from the
given issuer and returns the authorization, token, and device authorization
endpoints. Uses a default 10s timeout HTTP client if none provided.

**Called from:** `NewWebAuthenticator()` when endpoints are not explicitly
configured.

---

### `NewWebAuthenticator(ctx context.Context, cfg WebAuthConfig) (*WebAuthenticator, error)`

**What it does:** Validates the web auth configuration (requires `ClientID`,
`RedirectURL`, and `Verifier`). Discovers OAuth endpoints if not provided. Falls
back to persistent or in-memory session store. Sets defaults for scopes
(`["openid", "email", "groups", "profile"]`), route prefix (`"/auth"`), cookie
name (`"monofs_session"`), and post-login redirect (`"/"`).

**Session store selection:** If `MONOFS_SESSION_DIR` env var is set, tries a
persistent session store; falls back to in-memory on error.

---

### `randToken() string`

**Signature (unexported):** `func randToken() string`

**What it does:** Generates a 24-byte random token (base64url-encoded). Used for
OAuth state values.

**Called from:** `LoginHandler()`.

---

### `(w *WebAuthenticator) LoginHandler(rw http.ResponseWriter, r *http.Request)`

**What it does:** Initiates the PKCE login flow:
1. Generates PKCE verifier/challenge.
2. Generates random OAuth state.
3. Stores the state → verifier mapping in the `pending` map (pruning stale entries
   older than 10 minutes).
4. Sets a state cookie (`monofs_oauth_state`).
5. Redirects the browser to the IdP authorization endpoint.

**Route:** `<RoutePrefix>/login`

---

### `(w *WebAuthenticator) CallbackHandler(rw http.ResponseWriter, r *http.Request)`

**What it does:** Completes the login flow:
1. Validates the OAuth state against the cookie and pending map.
2. Exchanges the authorization code + PKCE verifier for tokens.
3. Verifies the `id_token` via the configured `Verifier`.
4. Creates a session and sets the session cookie.
5. Clears the state cookie.
6. Redirects to `PostLoginRedirect`.

**Route:** `<RoutePrefix>/callback`

---

### `(w *WebAuthenticator) LogoutHandler(rw http.ResponseWriter, r *http.Request)`

**What it does:** Deletes the session and clears the session cookie. Redirects to
`PostLoginRedirect`.

**Route:** `<RoutePrefix>/logout`

---

### `(w *WebAuthenticator) identityFromRequest(r *http.Request) Identity`

**Signature (unexported):** `func (w *WebAuthenticator) identityFromRequest(r *http.Request) Identity`

**What it does:** Resolves the caller identity:
1. Checks for a valid session cookie.
2. Falls back to a bearer token in the `Authorization` header (verified via
   `Verifier`).
3. Returns anonymous `Identity{}` if neither is present/valid.

**Called from:** `Middleware()`, `Handler()`.

---

### `(w *WebAuthenticator) Middleware(next http.Handler) http.Handler`

**What it does:** Returns middleware that attaches the resolved identity to the
request context. Never rejects requests (observe mode).

---

### `(w *WebAuthenticator) isExempt(path string) bool`

**Signature (unexported):** `func (w *WebAuthenticator) isExempt(path string) bool`

**What it does:** Returns true if the path matches any of the default exempt paths
(health, metrics, static assets, API, debug) or any user-configured `ExemptPaths`.
Matches exact paths and path prefixes.

**Called from:** `Handler()`.

---

### `wantsHTML(r *http.Request) bool`

**Signature (unexported):** `func wantsHTML(r *http.Request) bool`

**What it does:** Returns true if the request likely comes from a browser:
`Accept: text/html` header or `Sec-Fetch-Mode: navigate`.

**Called from:** `Handler()` to decide between redirecting (browser) vs returning
401 (API/machine).

---

### `(w *WebAuthenticator) Handler(next http.Handler) http.Handler`

**What it does:** Mounts the auth routes (`/login`, `/callback`, `/logout`) and
wraps all other requests with identity resolution. When `RequireLogin` is true,
anonymous callers are challenged: browsers are redirected to `/login`, other
requests get HTTP 401. Bearer-token access and exempt paths are always allowed.
Session-cookie-authenticated users always pass through.

**Routes:**
- `<RoutePrefix>/login` → `LoginHandler`
- `<RoutePrefix>/callback` → `CallbackHandler`
- `<RoutePrefix>/logout` → `LogoutHandler`

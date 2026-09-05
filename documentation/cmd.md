# MonoFS Command Documentation

This document describes every function in the `cmd/` directory Go source files (excluding `_test.go`).

---

## cmd/k8s-fuse-device-plugin/main.go

Package: `main`

A Kubernetes device plugin that advertises `/dev/fuse` as a schedulable resource (`sretoolhub.com/fuse`). This enables pods to request FUSE access via Kubernetes resource limits.

### Constants

- `resourceName = "sretoolhub.com/fuse"` — The Kubernetes resource name used for scheduling.
- `serverSock = pluginapi.DevicePluginPath + "fuse.sock"` — Unix socket path for the device plugin gRPC server.

### Struct: `FuseDevicePlugin`

| Field | Type | Description |
|-------|------|-------------|
| `devs` | `[]*pluginapi.Device` | Slice of virtual devices (one per allowed FUSE mount). |
| `socket` | `string` | Unix socket path for the gRPC server. |
| `stop` | `chan struct{}` | Closed on shutdown; signals `ListAndWatch` to return. |
| `health` | `chan *pluginapi.Device` | Channel to signal a device should be marked unhealthy. |
| `server` | `*grpc.Server` | The gRPC server instance. |

---

### `NewFuseDevicePlugin(number int) *FuseDevicePlugin`

**Signature:** `func NewFuseDevicePlugin(number int) *FuseDevicePlugin`

Creates a new `FuseDevicePlugin` with `number` virtual devices. Each device gets an ID like `fuse-<hostname>-<index>`. Called from `main()` to set up the plugin.

**Parameters:**
- `number` — Maximum number of concurrent FUSE mounts to advertise.

**Returns:** Initialized `*FuseDevicePlugin`.

---

### `(m *FuseDevicePlugin) GetDevicePluginOptions(_ context.Context, _ *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error)`

**Signature:** `func (m *FuseDevicePlugin) GetDevicePluginOptions(_ context.Context, _ *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error)`

Implements the `DevicePluginServer` interface. Returns empty options (no pre-start container required). Called by the kubelet during plugin registration.

**Returns:** Empty `*pluginapi.DevicePluginOptions`, nil error.

---

### `(m *FuseDevicePlugin) PreStartContainer(_ context.Context, _ *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error)`

**Signature:** `func (m *FuseDevicePlugin) PreStartContainer(_ context.Context, _ *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error)`

Implements the `DevicePluginServer` interface. Called by kubelet before starting containers that requested this resource. Returns empty response (no-op).

---

### `(m *FuseDevicePlugin) GetPreferredAllocation(_ context.Context, _ *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error)`

**Signature:** `func (m *FuseDevicePlugin) GetPreferredAllocation(_ context.Context, _ *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error)`

Implements the `DevicePluginServer` interface. Returns empty response (no device preference). Called by kubelet to ask for preferred allocation.

---

### `(m *FuseDevicePlugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error`

**Signature:** `func (m *FuseDevicePlugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error`

Implements the `DevicePluginServer` interface. Sends the initial list of devices and then watches for health changes. Loop exits when `m.stop` is closed.

**Parameters:**
- `s` — Server stream used to send `ListAndWatchResponse` messages back to kubelet.

**Implementation:** First sends all devices. Then waits on `m.stop` or `m.health`. When a device arrives on `m.health`, its health is set to `Unhealthy` and the full device list is re-sent. When `m.stop` closes, returns nil.

---

### `(m *FuseDevicePlugin) Allocate(_ context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error)`

**Signature:** `func (m *FuseDevicePlugin) Allocate(_ context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error)`

Implements the `DevicePluginServer` interface. Called by kubelet to allocate FUSE devices to containers. Validates each requested device ID exists, then maps `/dev/fuse` on the host to `/dev/fuse` in the container with `rwm` permissions.

**Parameters:**
- `reqs` — Allocation request from kubelet containing container requests and device IDs.

**Returns:** Response with `DeviceSpec` mapping `/dev/fuse` for each request, or an error if an unknown device ID is requested.

---

### `(m *FuseDevicePlugin) Start() error`

**Signature:** `func (m *FuseDevicePlugin) Start() error`

Starts the gRPC device plugin server. Cleans up any existing socket, listens on the Unix socket, creates a gRPC server, registers the `DevicePluginServer`, and starts serving. Also performs a health-check dial to verify the socket is ready.

**Returns:** Error if cleanup, listen, or health check fails.

---

### `(m *FuseDevicePlugin) Stop() error`

**Signature:** `func (m *FuseDevicePlugin) Stop() error`

Gracefully stops the gRPC server and cleans up the socket. Closes `m.stop` channel. No-op if `m.server` is nil.

**Returns:** Error from cleanup only.

---

### `(m *FuseDevicePlugin) Register(kubeletEndpoint string) error`

**Signature:** `func (m *FuseDevicePlugin) Register(kubeletEndpoint string) error`

Registers this device plugin with the kubelet via the registration API. Dials the kubelet's Unix socket, creates a `RegistrationClient`, and sends a `RegisterRequest` with the plugin version, socket endpoint name, and resource name.

**Parameters:**
- `kubeletEndpoint` — Path to kubelet's registration socket.

**Returns:** Error if dial or registration RPC fails.

---

### `(m *FuseDevicePlugin) Serve() error`

**Signature:** `func (m *FuseDevicePlugin) Serve() error`

Convenience method that calls `Start()` then `Register(kubeletSocket)`. If registration fails, calls `Stop()` to clean up. Called from `main()`.

**Returns:** Error if start or registration fails.

---

### `(m *FuseDevicePlugin) cleanup() error`

**Signature:** `func (m *FuseDevicePlugin) cleanup() error`

Removes the Unix socket file. Ignored if the file doesn't exist.

**Returns:** Error only if removal fails for reasons other than file-not-exist.

---

### `dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error)`

**Signature:** `func dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error)`

Connects to a Unix socket gRPC server with the given timeout. Uses insecure transport credentials and blocking dial. Called by `Start()` (health check) and `Register()`.

**Parameters:**
- `unixSocketPath` — Path to Unix domain socket.
- `timeout` — Connection timeout.

**Returns:** gRPC client connection or error.

---

### `createDevices(number int) []*pluginapi.Device`

**Signature:** `func createDevices(number int) []*pluginapi.Device`

Creates `number` virtual FUSE devices, each with an ID based on hostname and index, all marked `Healthy`. Called by `NewFuseDevicePlugin()`.

**Parameters:**
- `number` — How many devices to create.

**Returns:** Slice of `*pluginapi.Device`.

---

### `deviceExists(devs []*pluginapi.Device, id string) bool`

**Signature:** `func deviceExists(devs []*pluginapi.Device, id string) bool`

Checks whether a device with the given ID exists in the slice. Called by `Allocate()` for validation.

**Parameters:**
- `devs` — Device list to search.
- `id` — Device ID to look for.

**Returns:** `true` if found.

---

### `main()`

**Signature:** `func main()`

Parses the `--mounts-allowed` flag (default 100), creates a `FuseDevicePlugin`, calls `Serve()`, then blocks forever on `select {}`. If `Serve()` fails, calls `log.Fatalf`.

---

## cmd/monofs-admin/main.go

Package: `main`

The `monofs-admin` CLI tool provides cluster administration operations: ingest, delete, status, failover, rebalance, stats, drain, node-files, fetchers, whitelist, dogfood, authz, and login.

### `main()`

**Signature:** `func main()`

Entry point. Creates `flag.FlagSet` for each subcommand, parses arguments, and dispatches to the appropriate handler function. Supports subcommands: `ingest`, `delete`, `status`, `failover`, `rebuild-index`, `repos`, `rebalance`, `stats`, `drain`, `undrain`, `trigger-failover`, `clear-failover`, `node-files`, `fetchers`, `whitelist`, `dogfood`, `authz`, `login`. Prints usage and exits if no subcommand given.

---

### `printUsage()`

**Signature:** `func printUsage()`

Prints the CLI usage help text to stderr, listing all subcommands, options, and examples.

---

### `parseGitHubURL(rawURL string) (repoURL, branch string, err error)`

**Signature:** `func parseGitHubURL(rawURL string) (repoURL, branch string, err error)`

Extracts the repository `.git` URL and branch name from a GitHub URL. Supports formats: `https://github.com/owner/repo`, `https://github.com/owner/repo/tree/branch`, `https://github.com/owner/repo.git`. Strips `.git` suffix and `/tree/branch` prefix. Defaults branch to `"main"` if none specified.

**Parameters:**
- `rawURL` — Raw GitHub URL from user input.

**Returns:** Clean repo URL (`.git` form), branch name, error.

---

### `validateIngestionParams(source, ref, ingestionType string) error`

**Signature:** `func validateIngestionParams(source, ref, ingestionType string) error`

Validates source and ref parameters based on the ingestion type.

- For `"git"`: source must be a valid HTTP(S) or git@ URL.
- For `"go"`: source must be a module path with optional `@version`, version must start with `v`.
- For `"s3"`: source (bucket name) is required.

**Parameters:**
- `source` — Source identifier.
- `ref` — Reference (branch/version).
- `ingestionType` — One of `"git"`, `"go"`, `"s3"`.

**Returns:** Error if validation fails.

---

### `ingestRepository(routerAddr, rawSource, ref, customSourceID, ingestionType, fetchType string, replicateData bool, clientID, token string) error`

**Signature:** `func ingestRepository(routerAddr, rawSource, ref, customSourceID, ingestionType, fetchType string, replicateData bool, clientID, token string) error`

Ingests a source repository into the MonoFS cluster via the router's gRPC `IngestRepository` streaming RPC. Validates params, parses GitHub URLs for git type, connects to router with 30-minute timeout, and streams progress updates. Attaches `x-client-id` and `authorization` metadata for whitelist/SSO auth.

**Parameters:**
- `routerAddr` — Router gRPC address.
- `rawSource` — Source identifier.
- `ref` — Branch/version reference.
- `customSourceID` — Optional custom source ID.
- `ingestionType` — Backend type: `git`, `s3`, `file`.
- `fetchType` — Fetch backend: `blob`, `git`.
- `replicateData` — Whether to replicate blob data to fetch backend.
- `clientID` — Optional whitelist client ID.
- `token` — Bearer token for SSO auth.

**Returns:** Error if ingestion fails.

---

### `parseIngestionType(s string) pb.IngestionType`

**Signature:** `func parseIngestionType(s string) pb.IngestionType`

Converts a string to the corresponding protobuf `IngestionType` enum. Maps `"git"` → `INGESTION_GIT`, `"s3"` → `INGESTION_S3`, `"file"` → `INGESTION_FILE`. Defaults to `INGESTION_GIT`.

**Parameters:**
- `s` — String ingestion type (case-insensitive).

**Returns:** Protobuf ingestion type enum.

---

### `parseFetchType(s string) pb.SourceType`

**Signature:** `func parseFetchType(s string) pb.SourceType`

Converts a string to the corresponding protobuf `SourceType` enum. Maps `"git"` → `SOURCE_TYPE_GIT`, `"blob"` → `SOURCE_TYPE_BLOB`. Defaults to `SOURCE_TYPE_BLOB`.

**Parameters:**
- `s` — String fetch type (case-insensitive).

**Returns:** Protobuf source type enum.

---

### `deleteRepository(routerAddr, storageID string) error`

**Signature:** `func deleteRepository(routerAddr, storageID string) error`

Deletes a repository from all nodes via the router's gRPC `DeleteRepository` RPC. Connects with 10-second dial timeout, 5-minute operation timeout. Prints deletion result (files deleted, message).

**Parameters:**
- `routerAddr` — Router gRPC address.
- `storageID` — Storage ID to delete.

**Returns:** Error if connection or RPC fails.

---

### `showClusterStatus(routerAddr string) error`

**Signature:** `func showClusterStatus(routerAddr string) error`

Displays cluster health, node status, fetcher summary, search/indexer summary, and predictor stats. Calls `GetClusterInfo` via gRPC, then also fetches fetcher, search, and predictor stats via HTTP API.

**Parameters:**
- `routerAddr` — Router gRPC address (port 9090). Converts to port 8080 for HTTP APIs.

**Returns:** Error if gRPC call fails.

---

### `fetchFetcherStatsSummary(routerAddr string) *fetcherStatsSummary`

**Signature:** `func fetchFetcherStatsSummary(routerAddr string) *fetcherStatsSummary`

Fetches fetcher cluster stats via the HTTP API at `/api/fetchers` (port 8080). Converts gRPC address to HTTP address by replacing `:9090` with `:8080`.

**Parameters:**
- `routerAddr` — Router gRPC address.

**Returns:** `*fetcherStatsSummary` or nil on error.

---

### `fetchSearchStatsSummary(routerAddr string) *searchStatsSummary`

**Signature:** `func fetchSearchStatsSummary(routerAddr string) *searchStatsSummary`

Fetches search/indexer stats via the HTTP API at `/api/search/stats` (port 8080).

**Parameters:**
- `routerAddr` — Router gRPC address.

**Returns:** `*searchStatsSummary` or empty summary on error.

---

### `fetchPredictorStatsSummary(routerAddr string) *predictorStatsSummary`

**Signature:** `func fetchPredictorStatsSummary(routerAddr string) *predictorStatsSummary`

Fetches predictor (prefetch intelligence) stats via the HTTP API at `/api/predictor` (port 8080).

**Parameters:**
- `routerAddr` — Router gRPC address.

**Returns:** `*predictorStatsSummary` or nil on error.

---

### `showFailoverStatus(routerAddr string) error`

**Signature:** `func showFailoverStatus(routerAddr string) error`

Displays replication/failover status. Calls `GetClusterInfo` via gRPC, counts active/failed nodes, builds a failover mapping (first healthy node backs up each failed node), and prints a formatted table.

**Parameters:**
- `routerAddr` — Router gRPC address.

**Returns:** Error if gRPC call fails.

---

### `rebuildDirectoryIndex(routerAddr, storageID string, useExternal, debug bool) error`

**Signature:** `func rebuildDirectoryIndex(routerAddr, storageID string, useExternal, debug bool) error`

Rebuilds directory indexes for a repository across all nodes. Gets cluster info from router, then calls `BuildDirectoryIndexes` on each node's gRPC server. Uses an external address map for outside-Docker usage (node1:9000 → localhost:9001, etc.).

**Parameters:**
- `routerAddr` — Router gRPC address.
- `storageID` — Storage ID of the repository.
- `useExternal` — Use external (localhost) addresses instead of internal Docker addresses.
- `debug` — Enable debug logging.

**Returns:** Error if all required nodes fail, or a summary error with count of failed nodes.

---

### `showRepositories(routerAddr string) error`

**Signature:** `func showRepositories(routerAddr string) error`

Displays all repositories in the cluster via the HTTP API at `/api/repositories`. Sorts by `repo_id` and prints formatted boxes with status icons for each repo.

**Parameters:**
- `routerAddr` — Router HTTP address (port 8080).

**Returns:** Error if HTTP call fails.

---

### `triggerRebalance(routerAddr, storageID string) error`

**Signature:** `func triggerRebalance(routerAddr, storageID string) error`

Triggers rebalancing for a specific repository via the HTTP API at `/api/rebalance`. Sends a POST with `storage_id` form data.

**Parameters:**
- `routerAddr` — Router HTTP address (port 8080).
- `storageID` — Storage ID to rebalance.

**Returns:** Error if rebalance fails.

---

### `showStatistics(routerAddr, statsType, format string) error`

**Signature:** `func showStatistics(routerAddr, statsType, format string) error`

Displays cluster or node statistics. Creates a gRPC connection and dispatches based on `statsType`:
- `"cluster"` → `displayClusterStats`
- `"nodes"` → `displayNodeStats`
- `"all"` → both.

**Parameters:**
- `routerAddr` — Router gRPC address (port 9090).
- `statsType` — One of `"cluster"`, `"nodes"`, `"all"`.
- `format` — `"table"` or `"json"`.

**Returns:** Error if connection or RPC fails.

---

### `displayClusterStats(ctx context.Context, client pb.MonoFSRouterClient, format string) error`

**Signature:** `func displayClusterStats(ctx context.Context, client pb.MonoFSRouterClient, format string) error`

Displays cluster-wide statistics from `GetClusterStats` RPC. Shows total/healthy/unhealthy/syncing nodes, repositories, files, size, version, and active failovers.

**Parameters:**
- `ctx` — Context.
- `client` — Router gRPC client.
- `format` — `"table"` or `"json"`.

**Returns:** Error if RPC fails.

---

### `displayNodeStats(ctx context.Context, client pb.MonoFSRouterClient, format string) error`

**Signature:** `func displayNodeStats(ctx context.Context, client pb.MonoFSRouterClient, format string) error`

Displays per-node statistics from `GetNodeStats` RPC. Shows node ID, address, status, health, file count, sync progress, and backing-up nodes.

**Parameters:**
- `ctx` — Context.
- `client` — Router gRPC client.
- `format` — `"table"` or `"json"`.

**Returns:** Error if RPC fails.

---

### `formatBytes(bytes int64) string`

**Signature:** `func formatBytes(bytes int64) string`

Converts bytes to human-readable format (KB, MB, GB, TB, PB, EB). Uses 1024-based units.

**Parameters:**
- `bytes` — Number of bytes.

**Returns:** Formatted string like `"1.5 GB"`.

---

### `formatNumber(n int64) string`

**Signature:** `func formatNumber(n int64) string`

Formats a large number with comma separators (e.g., `1234567` → `"1,234,567"`).

**Parameters:**
- `n` — Number to format.

**Returns:** Comma-separated string.

---

### `truncate(s string, maxLen int) string`

**Signature:** `func truncate(s string, maxLen int) string`

Shortens a string to `maxLen` characters, appending `"..."` if truncated.

**Parameters:**
- `s` — String to truncate.
- `maxLen` — Maximum length.

**Returns:** Truncated string.

---

### `triggerFailover(routerAddr, nodeID string) error`

**Signature:** `func triggerFailover(routerAddr, nodeID string) error`

Manually triggers graceful failover for a node. Connects to router gRPC, calls `RequestFailover` RPC. Prints the result (target node, message).

**Parameters:**
- `routerAddr` — Router gRPC address.
- `nodeID` — Node ID to trigger failover for.

**Returns:** Error if connection or RPC fails.

---

### `clearFailover(routerAddr, nodeID string) error`

**Signature:** `func clearFailover(routerAddr, nodeID string) error`

Clears failover state for a recovered node. First gets cluster info to find the node's address, then connects directly to the node and calls `ClearFailoverCache` RPC.

**Parameters:**
- `routerAddr` — Router gRPC address.
- `nodeID` — Node ID to clear failover for.

**Returns:** Error if node not found or RPC fails.

---

### `showNodeFiles(routerAddr, nodeID, storageID, format string) error`

**Signature:** `func showNodeFiles(routerAddr, nodeID, storageID, format string) error`

Lists files owned by a specific node. If `nodeID` is `"all"`, queries all nodes via `showAllNodesFiles`. Otherwise, connects directly to the node. If `storageID` is empty, shows a node overview via `showNodeOverview`. Otherwise, calls `GetRepositoryFiles` RPC.

**Parameters:**
- `routerAddr` — Router gRPC address.
- `nodeID` — Node ID or `"all"`.
- `storageID` — Optional storage ID filter.
- `format` — `"table"` or `"json"`.

**Returns:** Error if node not found or RPC fails.

---

### `showNodeOverview(ctx context.Context, nodeClient pb.MonoFSClient, routerAddr, nodeID, nodeAddr, format string) error`

**Signature:** `func showNodeOverview(ctx context.Context, nodeClient pb.MonoFSClient, routerAddr, nodeID, nodeAddr, format string) error`

Shows a summary of all repositories and file counts on a single node. Calls `GetNodeInfo` and `ListRepositories`, then for each repo calls `GetRepositoryInfo` and `GetRepositoryFiles`.

**Parameters:**
- `ctx` — Context.
- `nodeClient` — Direct node gRPC client.
- `routerAddr` — Router address for display.
- `nodeID` — Node ID.
- `nodeAddr` — Node address.
- `format` — `"table"` or `"json"`.

**Returns:** Error if RPC calls fail.

---

### `showAllNodesFiles(ctx context.Context, routerAddr string, clusterInfo *pb.ClusterInfoResponse, storageID, format string) error`

**Signature:** `func showAllNodesFiles(ctx context.Context, routerAddr string, clusterInfo *pb.ClusterInfoResponse, storageID, format string) error`

Lists files for a storage ID across all nodes in parallel. Queries each node concurrently, collects results, deduplicates files across nodes, and displays which nodes hold each file.

**Parameters:**
- `ctx` — Context.
- `routerAddr` — Router address for display.
- `clusterInfo` — Cluster info from `GetClusterInfo`.
- `storageID` — Storage ID to query.
- `format` — `"table"` or `"json"`.

**Returns:** Error if no nodes or all queries fail.

---

### `showFetchers(routerHTTP, format string, detailed bool) error`

**Signature:** `func showFetchers(routerHTTP, format string, detailed bool) error`

Displays fetcher cluster status and statistics via the HTTP API at `/api/fetchers`. Shows cluster overview (health, cache hit rate, cache size, requests) and per-instance tables. Supports `--detailed` for per-source statistics.

**Parameters:**
- `routerHTTP` — Router HTTP address (port 8080).
- `format` — `"table"` or `"json"`.
- `detailed` — Show per-source statistics.

**Returns:** Error if HTTP call fails.

---

### `formatBytesShort(bytes int64) string`

**Signature:** `func formatBytesShort(bytes int64) string`

Converts bytes to a compact human-readable format (e.g., `"1GB"`, `"500MB"`, `"10KB"`). Uses 1024-based units with no space.

**Parameters:**
- `bytes` — Number of bytes.

**Returns:** Compact formatted string.

---

### `dogfoodRepositories(routerAddr, reposFilter, excludeFilter string) error`

**Signature:** `func dogfoodRepositories(routerAddr, reposFilter, excludeFilter string) error`

Discovers sibling repositories (guardian, doctor, monofs, kvs, k8s-top, agent, packager, cfg, stratatools) and ingests them into MonoFS. Uses git to get remote URL and current ref for each repo. Normalizes SSH URLs to HTTPS.

**Parameters:**
- `routerAddr` — Router gRPC address.
- `reposFilter` — Comma-separated repo names (default: all).
- `excludeFilter` — Comma-separated repo names to exclude.

**Returns:** Error listing failed repos if any ingestion fails.

---

### `findStratatoolsRoot() string`

**Signature:** `func findStratatoolsRoot() string`

Walks up from the current working directory looking for `pyproject.toml` with `src/stratatools`. Returns the found directory or a fallback path.

**Returns:** Path to stratatools root.

---

### `gitOutput(repoDir string, args []string, defaultVal string) string`

**Signature:** `func gitOutput(repoDir string, args []string, defaultVal string) string`

Executes a `git` command in the given repo directory and returns the trimmed output. Returns `defaultVal` if the command fails.

**Parameters:**
- `repoDir` — Repository directory (`-C` flag).
- `args` — Git subcommand and args.
- `defaultVal` — Value to return on error.

**Returns:** Command output or default.

---

### `normalizeGitSource(source string) string`

**Signature:** `func normalizeGitSource(source string) string`

Converts SSH-style git URLs to HTTPS:
- `git@github.com:owner/repo.git` → `https://github.com/owner/repo.git`
- `ssh://git@github.com/owner/repo.git` → `https://github.com/owner/repo.git`

**Parameters:**
- `source` — Raw git remote URL.

**Returns:** Normalized HTTPS URL.

---

### `manageAuthz(routerHTTP, action string) error`

**Signature:** `func manageAuthz(routerHTTP, action string) error`

Manages partition ingest authorization. Dispatches based on action: `"status"`, `"enable-ingest"`, `"disable-ingest"`.

**Parameters:**
- `routerHTTP` — Router HTTP address (port 8080).
- `action` — One of `"status"`, `"enable-ingest"`, `"disable-ingest"`.

**Returns:** Error if unknown action or HTTP call fails.

---

### `authzStatus(routerHTTP string) error`

**Signature:** `func authzStatus(routerHTTP string) error`

Fetches authz ingest enforcement status from `/api/authz/ingest`.

**Parameters:**
- `routerHTTP` — Router HTTP address.

**Returns:** Error if HTTP call fails.

---

### `authzToggleIngest(routerHTTP string, enforce bool) error`

**Signature:** `func authzToggleIngest(routerHTTP string, enforce bool) error`

Toggles ingest authorization enforcement via POST to `/api/authz/ingest/toggle`.

**Parameters:**
- `routerHTTP` — Router HTTP address.
- `enforce` — Whether to enable enforcement.

**Returns:** Error if HTTP call fails.

---

### `loginDeviceFlow(issuer, clientID, clientSecret, scopes string) error`

**Signature:** `func loginDeviceFlow(issuer, clientID, clientSecret, scopes string) error`

Authenticates via OIDC device authorization flow. Discovers OIDC endpoints, initiates device authorization, displays the verification URL and user code, polls for completion, and saves the token to `~/.monofs/token`.

**Parameters:**
- `issuer` — OIDC issuer URL.
- `clientID` — OIDC client ID.
- `clientSecret` — OIDC client secret.
- `scopes` — Space-separated scopes.

**Returns:** Error if discovery, authorization, polling, or saving fails.

---

### `loadSavedToken() string`

**Signature:** `func loadSavedToken() string`

Loads a saved OIDC token from `~/.monofs/token`. Returns the access token if available, otherwise the ID token.

**Returns:** Token string or empty string.

---

### Structs

**`fetcherStatsSummary`** — Holds aggregated fetcher stats (total, healthy, cache size, hit rate, requests).

**`searchStatsSummary`** — Holds aggregated search/indexer stats (indexes, files, failed jobs, searches, uptime).

**`predictorStatsSummary`** — Holds aggregated predictor stats (nodes, predictions, prefetches, hits, misses, hit rate, chains, dir maps).

**`FetcherClusterStats`** — JSON response for `/api/fetchers` (total, healthy, requests, cache stats, per-instance stats).

**`FetcherStats`** — Per-instance fetcher statistics.

**`SourceStatsInfo`** — Per-source statistics for a fetcher instance.

---

## cmd/monofs-admin/drain.go

Package: `main`

Handles cluster drain/undrain operations for safe maintenance.

### `drainCluster(routerAddr, reason string) error`

**Signature:** `func drainCluster(routerAddr, reason string) error`

Puts the cluster into drain/maintenance mode via the router's `DrainCluster` gRPC RPC. Connects with 10-second timeout. Prints formatted output with drain confirmation, reason, time, and instructions.

**Parameters:**
- `routerAddr` — Router gRPC address.
- `reason` — Reason for draining.

**Returns:** Error if connection or RPC fails.

---

### `undrainCluster(routerAddr string) error`

**Signature:** `func undrainCluster(routerAddr string) error`

Exits drain/maintenance mode via the router's `UndrainCluster` gRPC RPC. Prints confirmation that normal operations have resumed.

**Parameters:**
- `routerAddr` — Router gRPC address.

**Returns:** Error if connection or RPC fails.

---

## cmd/monofs-admin/whitelist.go

Package: `main`

Manages the ingestion whitelist for controlling which clients can ingest data.

### `manageWhitelist(routerAddr, action, clientID, label string) error`

**Signature:** `func manageWhitelist(routerAddr, action, clientID, label string) error`

Dispatches whitelist operations based on `action`:
- `"list"` → `whitelistList`
- `"add"` → `whitelistAdd` (requires `clientID`)
- `"remove"` → `whitelistRemove` (requires `clientID`)
- `"enable"` → `whitelistSetEnabled(ctx, client, true)`
- `"disable"` → `whitelistSetEnabled(ctx, client, false)`

Connects to router gRPC with 10-second timeout.

**Parameters:**
- `routerAddr` — Router gRPC address.
- `action` — One of `"add"`, `"remove"`, `"list"`, `"enable"`, `"disable"`.
- `clientID` — Client ID for add/remove.
- `label` — Human-friendly label (used with add).

**Returns:** Error if unknown action, connection, or RPC fails.

---

### `whitelistList(ctx context.Context, client pb.MonoFSRouterClient) error`

**Signature:** `func whitelistList(ctx context.Context, client pb.MonoFSRouterClient) error`

Calls `GetWhitelistStatus` RPC and displays the whitelist status (enabled/disabled), count, and a table of whitelisted clients with their IDs, labels, and added-at timestamps.

**Parameters:**
- `ctx` — Context.
- `client` — Router gRPC client.

**Returns:** Error if RPC fails.

---

### `whitelistAdd(ctx context.Context, client pb.MonoFSRouterClient, clientID, label string) error`

**Signature:** `func whitelistAdd(ctx context.Context, client pb.MonoFSRouterClient, clientID, label string) error`

Adds a client to the whitelist via `AddWhitelistedClient` RPC. Prints success or warning message.

**Parameters:**
- `ctx` — Context.
- `client` — Router gRPC client.
- `clientID` — Client ID to add.
- `label` — Optional label.

**Returns:** Error if RPC fails.

---

### `whitelistRemove(ctx context.Context, client pb.MonoFSRouterClient, clientID string) error`

**Signature:** `func whitelistRemove(ctx context.Context, client pb.MonoFSRouterClient, clientID string) error`

Removes a client from the whitelist via `RemoveWhitelistedClient` RPC.

**Parameters:**
- `ctx` — Context.
- `client` — Router gRPC client.
- `clientID` — Client ID to remove.

**Returns:** Error if RPC fails.

---

### `whitelistSetEnabled(ctx context.Context, client pb.MonoFSRouterClient, enabled bool) error`

**Signature:** `func whitelistSetEnabled(ctx context.Context, client pb.MonoFSRouterClient, enabled bool) error`

Enables or disables whitelist enforcement via `SetWhitelistEnabled` RPC. Shows lock/unlock icon with message.

**Parameters:**
- `ctx` — Context.
- `client` — Router gRPC client.
- `enabled` — Whether to enable enforcement.

**Returns:** Error if RPC fails.

---

## cmd/monofs-client/main.go

Package: `main`

The FUSE filesystem client for MonoFS. Mounts a virtual filesystem backed by the MonoFS cluster.

### `main()`

**Signature:** `func main()`

Entry point. Parses CLI flags (`--router`, `--mount`, `--cache`, `--overlay`, `--writable`, `--virtual-monorepo`, `--debug`, `--fuse-debug`, `--log-file`, `--keep-cache`, `--rpc-timeout`, `--client-id`, `--version`, `--use-external-addrs`). Sets up structured logging with `buildLogger`. Creates a sharded client connection, optional cache layer, optional write support (session manager, commit manager, socket handler), and mounts the FUSE filesystem. Handles SIGINT/SIGTERM for clean unmount.

---

### `newShardedClientConfig(routerAddr, clientID, mountpoint string, writable bool, rpcTimeout time.Duration, logger *slog.Logger, useExternalAddresses bool) client.ShardedClientConfig`

**Signature:** `func newShardedClientConfig(routerAddr, clientID, mountpoint string, writable bool, rpcTimeout time.Duration, logger *slog.Logger, useExternalAddresses bool) client.ShardedClientConfig`

Creates a `ShardedClientConfig` with the given parameters and a 30-second refresh interval.

**Parameters:**
- `routerAddr` — Router address.
- `clientID` — Client identifier.
- `mountpoint` — FUSE mount point.
- `writable` — Enable write support.
- `rpcTimeout` — RPC timeout.
- `logger` — Structured logger for the "sharded-client" component.
- `useExternalAddresses` — Use external node addresses.

**Returns:** Configured `ShardedClientConfig`.

---

### `workspaceGitStateDir(overlayDir string) string`

**Signature:** `func workspaceGitStateDir(overlayDir string) string`

Returns the workspace git state directory path. If `overlayDir` is set, uses `<overlayDir>/workspace-git`. Otherwise uses `~/.monofs/workspace-git`.

**Parameters:**
- `overlayDir` — Overlay directory path.

**Returns:** Workspace git state directory path.

---

### `validateClientPaths(mountpoint, overlayDir, cacheDir, workspaceGitStateDir string) error`

**Signature:** `func validateClientPaths(mountpoint, overlayDir, cacheDir, workspaceGitStateDir string) error`

Validates that overlay, cache, and workspace git state directories are outside the mountpoint (to prevent recursive issues). Resolves all paths to absolute clean forms and checks they are not the same as or nested under the mountpoint.

**Parameters:**
- `mountpoint` — FUSE mount point.
- `overlayDir` — Overlay directory.
- `cacheDir` — Cache directory.
- `workspaceGitStateDir` — Workspace git state directory.

**Returns:** Error if any path is inside the mountpoint.

---

### `absoluteCleanPath(path string) (string, error)`

**Signature:** `func absoluteCleanPath(path string) (string, error)`

Cleans and resolves a path to its absolute form. Returns error if the path is empty.

**Parameters:**
- `path` — Raw path.

**Returns:** Cleaned absolute path or error.

---

### `isSameOrNestedPath(path, base string) bool`

**Signature:** `func isSameOrNestedPath(path, base string) bool`

Checks if `path` is equal to `base` or is a subdirectory of `base`.

**Parameters:**
- `path` — Path to check.
- `base` — Base path.

**Returns:** True if path is same or nested under base.

---

### `buildLogger(debug bool, logFile string) *slog.Logger`

**Signature:** `func buildLogger(debug bool, logFile string) *slog.Logger`

Constructs the MonoFS structured logger. Always produces stdout text handler at Info+ (promoted to Debug+ if `--debug` is set without `--log-file`). If `logFile` is set, also writes JSON at Debug+/Info+ to file. Uses a `multiHandler` to fan out to both handlers.

**Parameters:**
- `debug` — Enable debug logging.
- `logFile` — Path for JSON log file.

**Returns:** Configured `*slog.Logger`.

---

### `buildFuseLogWriter(logFile string, fuseDebug bool) io.Writer`

**Signature:** `func buildFuseLogWriter(logFile string, fuseDebug bool) io.Writer`

Returns the writer for go-fuse C-layer debug output. If `fuseDebug` is false, returns `io.Discard`. If true and `logFile` is set, writes to `<logFile>.fuse`. Otherwise writes to stderr.

**Parameters:**
- `logFile` — Base log file path.
- `fuseDebug` — Enable FUSE C-layer debug.

**Returns:** Writer for go-fuse log output.

---

### Type: `multiHandler`

**Signature:** `type multiHandler struct { handlers []slog.Handler }`

A `slog.Handler` that fans records out to multiple underlying handlers. Each handler applies its own level filter.

#### `(m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool`

Returns true if any handler is enabled at the given level.

#### `(m *multiHandler) Handle(ctx context.Context, r slog.Record) error`

Delegates to all enabled handlers. Returns the first error encountered.

#### `(m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler`

Creates a new `multiHandler` with attrs applied to all underlying handlers.

#### `(m *multiHandler) WithGroup(name string) slog.Handler`

Creates a new `multiHandler` with the group applied to all underlying handlers.

---

## cmd/monofs-fetcher/main.go

Package: `main`

The MonoFS Fetcher service — a stateless proxy that handles external network access (Git remotes, S3, etc.) on behalf of storage nodes. Runs in the DMZ with external connectivity.

### Structs

**`fetcherConfig`** — JSON-loadable configuration struct with fields for port, cache, encryption, git backend, log level, and storage backend config.

**`storageConfig`** — Nested config for blob storage backend (local, S3, GCS).

---

### `defaultConfig() fetcherConfig`

**Signature:** `func defaultConfig() fetcherConfig`

Returns built-in defaults: port 9200, cache at `/data/fetcher-cache`, 50GB max cache, 2h cache age, 4 prefetch workers, info log level, local storage.

**Returns:** Default `fetcherConfig`.

---

### `loadConfigFile(path string) (fetcherConfig, error)`

**Signature:** `func loadConfigFile(path string) (fetcherConfig, error)`

Reads a JSON config file, falling back to defaults if the file doesn't exist. Returns parse errors.

**Parameters:**
- `path` — Path to JSON config file.

**Returns:** Merged config or error.

---

### `main()`

**Signature:** `func main()`

Entry point. Parses CLI flags (`--config`, `--port`, `--cache-dir`, `--max-cache-gb`, `--cache-age-hours`, `--prefetch-workers`, `--encryption-key`, `--enable-git`, `--diagnostics-addr`, `--log-level`). Loads config (file → env → flags precedence). Sets up telemetry and logging. Creates the blob backend (and optional git backend), registers them, starts a gRPC server with reflection enabled. Registers diagnostics endpoints for key status, key confirmation, and storage object listing. Handles SIGINT/SIGTERM for graceful shutdown.

---

## cmd/monofs-loadtest/main.go

Package: `main`

A load testing tool that generates filesystem operations against a mounted MonoFS filesystem.

### Struct: `Stats`

| Field | Type | Description |
|-------|------|-------------|
| `reads` | `atomic.Int64` | Number of read operations. |
| `writes` | `atomic.Int64` | Number of write operations. |
| `deletes` | `atomic.Int64` | Number of delete operations. |
| `mkdirs` | `atomic.Int64` | Number of mkdir operations. |
| `lists` | `atomic.Int64` | Number of list operations. |
| `errors` | `atomic.Int64` | Number of errors. |
| `totalOps` | `atomic.Int64` | Total successful operations. |
| `bytesRead` | `atomic.Int64` | Total bytes read. |
| `bytesWritten` | `atomic.Int64` | Total bytes written. |

---

### `main()`

**Signature:** `func main()`

Entry point. Parses flags (`--mount`, `--duration`, `--concurrency`, `--file-size`, `--read-ratio`, `--write-ratio`, `--delete-ratio`, `--mkdir-ratio`, `--list-ratio`, `--verbose`, `--read-existing`, `--repo-path`, `--max-scan-files`, `--read-only`). Validates ratios sum ≤ 1.0. Optionally scans existing files. Creates test directory. Starts progress reporter and `concurrency` workers. Runs for `duration`, then prints final report. Cleans up test directory.

---

### `runWorker(id int, testDir string, existingFiles []string, stats *Stats, stopChan <-chan struct{})`

**Signature:** `func runWorker(id int, testDir string, existingFiles []string, stats *Stats, stopChan <-chan struct{})`

Runs a single worker goroutine that loops performing random filesystem operations based on configured ratios. Creates a per-worker subdirectory. Operations are dispatched by rolling a random float against cumulative ratios:
1. Read (read existing files or created files).
2. Write (create random data file).
3. Delete (remove a created file).
4. Mkdir (create directory).
5. List (read directory contents).

**Parameters:**
- `id` — Worker ID.
- `testDir` — Base test directory.
- `existingFiles` — Pre-scanned existing files for reads.
- `stats` — Shared stats counters.
- `stopChan` — Stop signal channel.

---

### `readFile(path string, stats *Stats) error`

**Signature:** `func readFile(path string, stats *Stats) error`

Reads a file using `os.ReadFile` and updates read stats counter and bytes read.

**Parameters:**
- `path` — File path to read.
- `stats` — Stats to update.

**Returns:** Error from `os.ReadFile`.

---

### `writeFile(path string, size int, stats *Stats) error`

**Signature:** `func writeFile(path string, size int, stats *Stats) error`

Writes a file with `size` bytes of random data using `os.WriteFile`. Updates write stats and bytes written.

**Parameters:**
- `path` — File path.
- `size` — Size in bytes.
- `stats` — Stats to update.

**Returns:** Error from `os.WriteFile`.

---

### `deleteFile(path string, stats *Stats) error`

**Signature:** `func deleteFile(path string, stats *Stats) error`

Deletes a file using `os.Remove`. Updates delete stats.

**Parameters:**
- `path` — File path.
- `stats` — Stats to update.

**Returns:** Error from `os.Remove`.

---

### `mkdirOp(path string, stats *Stats) error`

**Signature:** `func mkdirOp(path string, stats *Stats) error`

Creates a directory using `os.MkdirAll`. Updates mkdir stats.

**Parameters:**
- `path` — Directory path.
- `stats` — Stats to update.

**Returns:** Error from `os.MkdirAll`.

---

### `listDir(path string, stats *Stats) error`

**Signature:** `func listDir(path string, stats *Stats) error`

Lists directory contents using `os.ReadDir`. Updates list stats.

**Parameters:**
- `path` — Directory path.
- `stats` — Stats to update.

**Returns:** Error from `os.ReadDir`.

---

### `reportProgress(stats *Stats, startTime time.Time, stopChan <-chan struct{})`

**Signature:** `func reportProgress(stats *Stats, startTime time.Time, stopChan <-chan struct{})`

Prints periodic progress updates every 5 seconds. Shows elapsed time, total ops, ops/sec, and breakdown by operation type. Exits when `stopChan` is closed.

**Parameters:**
- `stats` — Shared stats.
- `startTime` — Test start time.
- `stopChan` — Stop signal.

---

### `printFinalReport(stats *Stats, elapsed time.Duration)`

**Signature:** `func printFinalReport(stats *Stats, elapsed time.Duration)`

Prints the final test results: duration, total operations, ops/sec, operation breakdown with percentages, read/write throughput, and error summary.

**Parameters:**
- `stats` — Shared stats.
- `elapsed` — Total elapsed time.

---

### `cleanup(dir string)`

**Signature:** `func cleanup(dir string)`

Removes the test directory using `os.RemoveAll`. No-op if `dir` is empty. Logs warning on failure.

**Parameters:**
- `dir` — Directory to remove.

---

### `scanExistingFiles(root string, maxFiles int) []string`

**Signature:** `func scanExistingFiles(root string, maxFiles int) []string`

Walks the directory tree collecting file paths for read-existing mode. Prefers source code files (`.go`, `.py`, `.js`, `.ts`, `.java`, `.c`, `.h`, `.cpp`, `.rs`, `.rb`, `.md`, `.txt`, `.json`, `.yaml`, `.yml`, `.toml`). Skips hidden files and common non-code files. Stops scanning after `maxFiles` files.

**Parameters:**
- `root` — Root directory to scan.
- `maxFiles` — Maximum files to collect.

**Returns:** Slice of file paths.

---

### `randFloat() float64`

**Signature:** `func randFloat() float64`

Generates a random float64 in [0, 1) using `crypto/rand` for cryptographic-quality randomness.

**Returns:** Random float between 0.0 and 1.0.

---

### `randInt(max int) int`

**Signature:** `func randInt(max int) int`

Generates a random integer in [0, max) using `crypto/rand`. Returns 0 if max is 0.

**Parameters:**
- `max` — Upper bound (exclusive).

**Returns:** Random integer.

---

### `percent(part, total int64) float64`

**Signature:** `func percent(part, total int64) float64`

Calculates percentage: `(part / total) * 100`. Returns 0 if total is 0.

**Parameters:**
- `part` — Numerator.
- `total` — Denominator.

**Returns:** Percentage as float.

---

### `formatBytes(bytes int64) string` (loadtest)

**Signature:** `func formatBytes(bytes int64) string`

Converts bytes to human-readable format using 1024-based units (B, KB, MB, GB, TB, PB, EB).

**Parameters:**
- `bytes` — Byte count.

**Returns:** Formatted string.

---

## cmd/monofs-pipeline-worker/main.go

Package: `main`

A pipeline worker that executes tasks (build, docker, deploy) dispatched by the MonoFS router.

### `main()`

**Signature:** `func main()`

Entry point. Parses flags (`--router`, `--type`, `--concurrency`, `--guardian-token`, `--id`, `--log-level`, `--mount-path`). Sets up logging and optional telemetry. Connects to router gRPC, creates a worker client, selects the appropriate handler based on worker type:
- `"builder"` → `worker.NewBuilderHandler`
- `"docker"` → `worker.NewDockerHandler`
- `"deployer"` → `worker.NewDeployerHandler`

Creates a worker and runs it. Handles SIGINT/SIGTERM for graceful shutdown.

---

## cmd/monofs-registry/main.go

Package: `main`

MonoFS Registry — a Docker-compatible OCI container registry backed by MonoFS storage.

### `main()`

**Signature:** `func main()`

Entry point. Parses flags (`--addr`, `--router-addr`, `--token`, `--data-ns`, `--log-level`, `--debug`, `--upstream-default`, `--upstreams`, `--upstream-username`, `--upstream-password`, `--upstream-cooldown`, `--diagnostics-addr`). Sets up logging and telemetry. Connects to MonoFS router, ensures `_blobs` and `_uploads` data directories exist. Creates upstream config, registry proxy (blob store + tag store), and registry server. Starts HTTP server and diagnostics server. Handles SIGINT/SIGTERM for graceful shutdown.

---

### `parseUpstreamMappings(raw string) map[string]string`

**Signature:** `func parseUpstreamMappings(raw string) map[string]string`

Parses upstream registry mappings from a comma-separated string of `prefix=url` pairs (e.g., `"docker.io=docker.io,ghcr.io=ghcr.io"`).

**Parameters:**
- `raw` — Raw mappings string.

**Returns:** Map of prefix → upstream URL.

---

### `ensureDataDirs(ctx context.Context, client *registry.Client, logger *slog.Logger)`

**Signature:** `func ensureDataDirs(ctx context.Context, client *registry.Client, logger *slog.Logger)`

Creates required data directories (`_blobs`, `_uploads`) in the MonoFS storage backend. Logs warnings on failure but does not abort.

**Parameters:**
- `ctx` — Context.
- `client` — Registry client for MonoFS operations.
- `logger` — Logger.

---

## cmd/monofs-router/main.go

Package: `main`

The MonoFS Router — the cluster topology coordinator. Handles node registration, health checking, request routing, replication, and the HTTP UI.

### `init()`

**Signature:** `func init()`

Registers storage backends with the default registry:
- Git ingestion backend
- Git fetch backend
- File ingestion backend

Called automatically before `main()`.

---

### `main()`

**Signature:** `func main()`

Entry point. Parses extensive CLI flags for port, HTTP port, cluster ID, nodes, weights, external addresses, peer routers, search/fetcher/registry/server addresses, health intervals, encryption keys, replication, failover, authz, OIDC, and more. Sets up telemetry and logging. Creates a `Router` with configuration, registers initial nodes, starts health checking. Configures search and fetcher clients. Sets up SSO/OIDC authentication with web authenticator for browser login. Creates gRPC server with keepalive, auth interceptors, and large message limits. Starts HTTP UI with public path exemptions. Handles SIGINT/SIGTERM for graceful shutdown.

---

### `parseWeights(weightsStr string) map[string]uint32`

**Signature:** `func parseWeights(weightsStr string) map[string]uint32`

Parses weight specifications in the format `node1=100,node2=200,...`. Skips invalid entries or zero weights.

**Parameters:**
- `weightsStr` — Comma-separated `node=weight` pairs.

**Returns:** Map of node ID → weight.

---

### `parseExternalAddrs(addrsStr string) map[string]string`

**Signature:** `func parseExternalAddrs(addrsStr string) map[string]string`

Parses external address mappings in the format `node1=localhost:9001,node2=localhost:9002,...`.

**Parameters:**
- `addrsStr` — Comma-separated `node=address` pairs.

**Returns:** Map of node ID → external address.

---

### `parsePeerRouters(peersStr string) []router.RouterPeer`

**Signature:** `func parsePeerRouters(peersStr string) []router.RouterPeer`

Parses peer router specifications. Supports `name=http://host:port` format or bare `host:port` (name defaults to address). Returns empty slice if input is empty.

**Parameters:**
- `peersStr` — Comma-separated peer specs.

**Returns:** Slice of `RouterPeer`.

---

### `parseCSVAddrs(raw string) []string`

**Signature:** `func parseCSVAddrs(raw string) []string`

Parses a comma-separated list of addresses, trimming whitespace and filtering empty entries.

**Parameters:**
- `raw` — Comma-separated addresses string.

**Returns:** Cleaned slice of addresses.

---

### `parseServerDiagnostics(raw string) map[string]string`

**Signature:** `func parseServerDiagnostics(raw string) map[string]string`

Parses server diagnostics address mappings: `node-a=node-a:9100,node-b=node-b:9100`. Returns nil if input is empty.

**Parameters:**
- `raw` — Comma-separated `nodeID=addr` pairs.

**Returns:** Map of node ID → diagnostics address.

---

## cmd/monofs-search/main.go

Package: `main`

The MonoFS Search service — provides full-text code search using Zoekt indexes.

### `main()`

**Signature:** `func main()`

Entry point. Parses flags (`--port`, `--index-dir`, `--cache-dir`, `--workers`, `--queue-size`, `--router-addr`, `--diagnostics-addr`, `--debug`, `--version`). Sets up logging and telemetry. Creates search service with `search.Config`. Creates gRPC server with 100MB message limits, registers `MonoFSSearchServer`, health check, and reflection. Handles SIGINT/SIGTERM for graceful shutdown (marks service NOT_SERVING before stopping).

---

## cmd/monofs-server/main.go

Package: `main`

The MonoFS Server — a gRPC backend storage node. Uses NutsDB for local storage, with optional embedded KVS (Raft-based), fetcher client for prediction, and server-side request forwarding.

### Type: `stringSlice`

A flag type that can be specified multiple times on the CLI.

#### `(s *stringSlice) String() string`

Returns the slice joined by commas.

#### `(s *stringSlice) Set(value string) error`

Appends a value to the slice.

---

### Type: `kvsOffloaderAdapter`

Implements `kvsapi.FetcherOffloader` using a `fetcher.Client` to route KVS archive blobs through the fetcher tier.

#### `(a *kvsOffloaderAdapter) StoreBlob(ctx context.Context, blobHash string, content []byte) error`

Stores a blob via the fetcher client using `StoreBlobBatch`.

#### `(a *kvsOffloaderAdapter) FetchBlob(ctx context.Context, blobHash string) ([]byte, error)`

Fetches a blob via the fetcher client using `FetchBlob` with content ID, source key `"kvs-archive"`, and priority 3.

---

### `main()`

**Signature:** `func main()`

Entry point. Parses flags (`--addr`, `--node-id`, `--router`, `--db-path`, `--db-sync`, `--git-cache`, `--debug`, `--log-level`, `--fetcher`, `--enable-prediction`, `--kvs-data-dir`, `--kvs-api-addr`, `--kvs-raft-addr`, `--kvs-raft-advertise-addr`, `--kvs-bootstrap`, `--kvs-max-hot-versions`, `--kvs-peer`, `--logengine-dir`, `--metrics-addr`). Sets up telemetry and logging. Creates NutsDB storage server. Optionally creates an embedded KVS store with Raft. Configures fetcher client for prediction. Enables server-side request forwarding if router is configured. Initializes doctor telemetry log engine. Registers gRPC services. On shutdown (SIGINT/SIGTERM/SIGHUP), attempts graceful failover via `requestFailover` before stopping.

---

### `newMetricsHandler() http.Handler`

**Signature:** `func newMetricsHandler() http.Handler`

Returns a new diagnostics HTTP handler for Prometheus and pprof endpoints.

**Returns:** Handler from `diagnostics.NewHandler()`.

---

### `requestFailover(routerAddr, nodeID string, logger *slog.Logger) error`

**Signature:** `func requestFailover(routerAddr, nodeID string, logger *slog.Logger) error`

Requests graceful failover from the router when the server is shutting down. Dials the router gRPC with 20-second timeout, calls `RequestFailover` RPC with the node ID and current timestamp.

**Parameters:**
- `routerAddr` — Router gRPC address.
- `nodeID` — This node's ID.
- `logger` — Logger.

**Returns:** Error if connection or RPC fails, or failover is rejected.

---

### `parseKVSPeers(values []string) ([]raftstore.Peer, error)`

**Signature:** `func parseKVSPeers(values []string) ([]raftstore.Peer, error)`

Parses KVS peer specifications in the format `nodeID,apiAddress,raftAddress`. Validates that all three fields are non-empty.

**Parameters:**
- `values` — Slice of peer spec strings.

**Returns:** Slice of `raftstore.Peer` or error if parsing fails.

---

## cmd/monofs-session/main.go

Package: `main`

The MonoFS Session CLI — manages write sessions, staging, commits, branches, search, and blob operations via a Unix domain socket to the FUSE client.

### Constants

- `defaultSocketTimeout = 30 * time.Second` — Default timeout for socket RPC.
- `pushSocketTimeout = 10 * time.Minute` — Extended timeout for push/commit/pull/push-blobs.

### Structs

**`SessionRequest`** — JSON request sent to FUSE daemon over Unix socket. Fields: Action, Path, Paths, BranchOp, BranchName, ShowBlobs, LogicalCommitMessage, AuthorName, AuthorEmail, RequestedBranchStrategy.

**`FileDiff`** — Unified diff output for a single changed file. Fields: Path, ChangeType, Repository, StorageID, Diff.

**`SessionResponse`** — JSON response from FUSE daemon. Fields: Success, SessionID, CreatedAt, Changes, UnstagedChanges, StagedChanges, PendingCommits, BlobChanges, ExcludedChanges, Message, Error, ChangeList, StagedChangeList, LocalCommitList, PendingCommitList, CurrentBranch, BranchList, BranchMappings, BlobChangeList, WorkspaceRefs, DepsInfo, DiffData, BlobDiffData.

**`WorkspaceRef`** — Authoritative tracked ref for a mounted repo. Fields: DisplayPath, Ref, CommitHash.

**`BlobsInfoData`** — Blob file information. Fields: TotalFiles, TotalBytes, Tools.

**`BlobsToolInfo`** — Per-tool dep info. Fields: Tool, Files, Bytes, FileList.

**`BlobFileInfo`** — Single blob file. Fields: Path, Size.

**`setupEnvEntry`** — For setup command output. Fields: envVar, dir.

**`ChangeInfo`** — Single change for display. Fields: Type, Path, Repository, StorageID, Timestamp.

**`LocalCommitInfo`** — Local virtual commit. Fields: ID, ParentID, Message, LogicalBranch, AuthorName, AuthorEmail, PrincipalID, CreatedAt, RepositoryCount, OperationCount, Pushed.

**`BranchInfo`** — Branch state. Fields: Name, Current, PendingCommits, HasMappings.

**`BranchMappingInfo`** — Branch-to-repo mapping. Fields: DisplayPath, OriginalBranch, ActualBranch, LastPushedCommit.

**`SessionCommand`** — Holds the Unix socket path.

---

### `NewSessionCommand(overlayDir string) *SessionCommand`

**Signature:** `func NewSessionCommand(overlayDir string) *SessionCommand`

Creates a session command handler with socket path at `<overlayDir>/session.sock`.

**Parameters:**
- `overlayDir` — Overlay directory path.

**Returns:** `*SessionCommand`.

---

### `defaultSessionSocketPath() string`

**Signature:** `func defaultSessionSocketPath() string`

Resolves the default socket path by checking environment variables (`MONOFS_OVERLAY_DIR`, `GITFS_OVERLAY_DIR`, `MONOFS_OVERLAY`), then falls back to `~/.monofs/overlay/session.sock`.

**Returns:** Socket file path.

---

### `main()`

**Signature:** `func main()`

Entry point. Extracts `--socket` flag from args, determines socket path, creates `SessionCommand`, and calls `Execute` with remaining args. Exits with error message on failure.

---

### `(sc *SessionCommand) Execute(args []string) error`

**Signature:** `func (sc *SessionCommand) Execute(args []string) error`

Dispatches to the appropriate handler based on the first argument:
- `"start"` → `startSession()`
- `"add"` → `addToIndex()`
- `"rm"` → `removeFromIndex()`
- `"status"` → `showStatus()`
- `"branch"` → `manageBranches()`
- `"refs"` → `showRefs()`
- `"log"` → `showLog()`
- `"commit"` → `commitSession()`
- `"pull"` → `pullWorkspace()`
- `"discard"` → `discardSession()`
- `"search"` → `searchCode()`
- `"setup"` → `setupBlobs()`
- `"diff"` → `showDiff()`
- `"blobs-info"` → `showBlobsInfo()`
- `"push"` → `pushSource()`
- `"push-blobs"` / `"push-deps"` → `uploadBlobs()`
- `"help"` / `"--help"` / `"-h"` → `printUsage()`

**Returns:** Error for unknown command or handler error (uses `printUsage` for missing command).

---

### `(sc *SessionCommand) printUsage() error`

**Signature:** `func (sc *SessionCommand) printUsage() error`

Prints comprehensive usage help including all commands, options, environment variables, examples, and the current socket path.

**Returns:** nil.

---

### `(sc *SessionCommand) sendCommand(action string) (*SessionResponse, error)`

**Signature:** `func (sc *SessionCommand) sendCommand(action string) (*SessionResponse, error)`

Sends a simple action-only request to the FUSE daemon via Unix socket. Checks socket existence, dials, sends JSON-encoded `SessionRequest{Action: action}`, decodes the response. Includes helpful error messages with common socket locations if the socket is not found.

**Parameters:**
- `action` — Action string (e.g., `"start"`, `"status"`).

**Returns:** `*SessionResponse` or error.

---

### `(sc *SessionCommand) sendCommandWithPath(action, path string) (*SessionResponse, error)`

**Signature:** `func (sc *SessionCommand) sendCommandWithPath(action, path string) (*SessionResponse, error)`

Convenience wrapper around `sendRequest` for requests with an action and a single path.

**Parameters:**
- `action` — Action string.
- `path` — File/directory path.

**Returns:** `*SessionResponse` or error.

---

### `(sc *SessionCommand) sendRequest(req SessionRequest) (*SessionResponse, error)`

**Signature:** `func (sc *SessionCommand) sendRequest(req SessionRequest) (*SessionResponse, error)`

Sends an arbitrary `SessionRequest` to the FUSE daemon via Unix socket. Checks socket existence, dials, sets deadline based on `socketTimeoutForAction`, encodes request as JSON, decodes response.

**Parameters:**
- `req` — Full `SessionRequest` to send.

**Returns:** `*SessionResponse` or error.

---

### `socketTimeoutForAction(action string) time.Duration`

**Signature:** `func socketTimeoutForAction(action string) time.Duration`

Returns the appropriate timeout for a socket operation. `"commit"`, `"push"`, `"push-blobs"`, and `"pull"` get `pushSocketTimeout` (10 min). All others get `defaultSocketTimeout` (30 sec).

**Parameters:**
- `action` — Action name.

**Returns:** Timeout duration.

---

### `firstNonEmpty(values ...string) string`

**Signature:** `func firstNonEmpty(values ...string) string`

Returns the first non-empty (after trimming) string from the given values.

**Parameters:**
- `values` — String values to check.

**Returns:** First non-empty value or `""`.

---

### `(sc *SessionCommand) showDiff(args []string) error`

**Signature:** `func (sc *SessionCommand) showDiff(args []string) error`

Shows unified diffs between original and changed files. Supports `--deps` flag to include blob file diffs. Sends a `"diff"` request with optional path filter.

**Parameters:**
- `args` — CLI args (supports `--deps` and positional path filter).

**Returns:** Error if socket communication fails.

---

### `(sc *SessionCommand) startSession() error`

**Signature:** `func (sc *SessionCommand) startSession() error`

Starts a new write session (or shows current if active). Sends `"start"` command and prints session ID, creation time, and usage hints.

**Returns:** Error if session start fails.

---

### `(sc *SessionCommand) addToIndex(args []string) error`

**Signature:** `func (sc *SessionCommand) addToIndex(args []string) error`

Stages source changes for commit. Accepts one or more path arguments. Sends `"add"` request with paths. Prints staged change count and change list.

**Parameters:**
- `args` — Paths to stage.

**Returns:** Error if no paths given or operation fails.

---

### `(sc *SessionCommand) removeFromIndex(args []string) error`

**Signature:** `func (sc *SessionCommand) removeFromIndex(args []string) error`

Removes source paths and stages the deletes. Accepts path arguments. Sends `"rm"` request with paths.

**Parameters:**
- `args` — Paths to remove.

**Returns:** Error if no paths or operation fails.

---

### `(sc *SessionCommand) showStatus(args []string) error`

**Signature:** `func (sc *SessionCommand) showStatus(args []string) error`

Shows current session status and pending changes. Supports `--deps` flag to show individual blob file changes. Sends `"status"` request. Displays session ID, creation time, workspace file count, unstaged/staged/pending counts, blob changes, and change lists. Gracefully handles "no active session" state.

**Parameters:**
- `args` — CLI args (supports `--deps`).

**Returns:** Error if operation fails.

---

### `(sc *SessionCommand) showLog(args []string) error`

**Signature:** `func (sc *SessionCommand) showLog(args []string) error`

Shows local virtual commit history. Sends `"log"` command. Displays each commit with ID, creation time, branch, parent, principal, author, state (pushed/pending), and message.

**Parameters:**
- `args` — CLI args (currently unused).

**Returns:** Error if operation fails.

---

### `printChangeInfoSection(title string, changes []ChangeInfo)`

**Signature:** `func printChangeInfoSection(title string, changes []ChangeInfo)`

Prints a formatted section of changes, grouped by repository. Shows change symbols (`[+]` for create, `[M]` for modify, `[-]` for delete, `[D+]` for mkdir, `[D-]` for rmdir, `[L]` for symlink, etc.).

**Parameters:**
- `title` — Section heading.
- `changes` — List of changes to display.

---

### `formatLocalCommitAuthor(commit LocalCommitInfo) string`

**Signature:** `func formatLocalCommitAuthor(commit LocalCommitInfo) string`

Formats author info for display: `"Name <email>"`, `"Name"`, `"<email>"`, or empty string.

**Parameters:**
- `commit` — Local commit info.

**Returns:** Formatted author string.

---

### `localCommitState(commit LocalCommitInfo) string`

**Signature:** `func localCommitState(commit LocalCommitInfo) string`

Returns `"pushed"` if the commit has been pushed, `"pending"` otherwise.

**Parameters:**
- `commit` — Local commit info.

**Returns:** State string.

---

### `displayLogicalBranchName(branchName string) string`

**Signature:** `func displayLogicalBranchName(branchName string) string`

Returns the branch name or `"(default)"` if empty.

**Parameters:**
- `branchName` — Branch name.

**Returns:** Display-friendly branch name.

---

### `(sc *SessionCommand) showRefs(args []string) error`

**Signature:** `func (sc *SessionCommand) showRefs(args []string) error`

Shows authoritative tracked workspace refs for mounted repositories. Sends `"refs"` command. Displays a table with branch, commit (short hash), and repository path.

**Parameters:**
- `args` — CLI args (currently unused).

**Returns:** Error if operation fails.

---

### `(sc *SessionCommand) manageBranches(args []string) error`

**Signature:** `func (sc *SessionCommand) manageBranches(args []string) error`

Manages logical branches. Dispatches based on first arg:
- `"show"` → `showLogicalBranches()`
- `"create"` → sends `"branch"` with `BranchOp="create"` and branch name.
- `"switch"` → sends `"branch"` with `BranchOp="switch"` and branch name.

**Parameters:**
- `args` — First arg is operation, second is branch name (for create/switch).

**Returns:** Error for unknown subcommand or operation failure.

---

### `(sc *SessionCommand) showLogicalBranches() error`

**Signature:** `func (sc *SessionCommand) showLogicalBranches() error`

Shows all logical branches, current branch, and per-repo branch mappings. Sends `"branch"` request with `BranchOp="show"`. Displays branch list with pending commit counts and current indicator.

**Returns:** Error if operation fails.

---

### `(sc *SessionCommand) commitSession(args []string) error`

**Signature:** `func (sc *SessionCommand) commitSession(args []string) error`

Creates a local virtual commit from staged source changes. Supports `-m` / `--message`, `--author-name`, `--author-email`, `--branch-strategy` flags. Author defaults from environment variables (`MONOFS_AUTHOR_NAME`, `GIT_AUTHOR_NAME`, `GIT_COMMITTER_NAME`, `MONOFS_AUTHOR_EMAIL`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_EMAIL`). Sends `"commit"` request.

**Parameters:**
- `args` — CLI args with flags.

**Returns:** Error if no staged changes or operation fails.

---

### `(sc *SessionCommand) pushSource(args []string) error`

**Signature:** `func (sc *SessionCommand) pushSource(args []string) error`

Pushes pending local virtual commits upstream. Sends `"push"` command. Warns that pending commits are squashed into one upstream Git commit per affected repository.

**Parameters:**
- `args` — CLI args (no positional args accepted).

**Returns:** Error if operation fails.

---

### `(sc *SessionCommand) pullWorkspace() error`

**Signature:** `func (sc *SessionCommand) pullWorkspace() error`

Re-ingests included workspace repositories from their upstream sources. Sends `"pull"` command.

**Returns:** Error if operation fails.

---

### `(sc *SessionCommand) discardSession() error`

**Signature:** `func (sc *SessionCommand) discardSession() error`

Abandons all local changes and deletes the session. Sends `"discard"` command. Irreversible — all local modifications are removed.

**Returns:** Error if operation fails.

---

### `getChangeSymbol(changeType string) string`

**Signature:** `func getChangeSymbol(changeType string) string`

Returns a display symbol for a change type: `[+]` for create, `[M]` for modify, `[-]` for delete, `[D+]` for mkdir, `[D-]` for rmdir, `[L]` for symlink, `[U+]` for user_root_dir, `[U-]` for remove_user_root_dir, `[?]` for unknown.

**Parameters:**
- `changeType` — Change type string.

**Returns:** Display symbol string.

---

### `shortCommitHash(hash string) string`

**Signature:** `func shortCommitHash(hash string) string`

Truncates a commit hash to 12 characters (or returns the full hash if shorter). Also trims whitespace.

**Parameters:**
- `hash` — Full commit hash.

**Returns:** Short hash.

---

### `(sc *SessionCommand) showBlobsInfo(args []string) error`

**Signature:** `func (sc *SessionCommand) showBlobsInfo(args []string) error`

Displays blob file information for the current session. Supports `--format` (table/json) and `--detailed` flags. Sends `"blobs-info"` command. Shows total files, total size, and per-tool breakdown.

**Parameters:**
- `args` — CLI args with flags.

**Returns:** Error if operation fails.

---

### `formatBytes(b int64) string` (session)

**Signature:** `func formatBytes(b int64) string`

Converts bytes to human-readable format using 1024-based units (B, KiB, MiB, GiB, TiB, PiB, EiB).

**Parameters:**
- `b` — Byte count.

**Returns:** Formatted string.

---

### `(sc *SessionCommand) uploadBlobs(args []string) error`

**Signature:** `func (sc *SessionCommand) uploadBlobs(args []string) error`

Uploads dependency blob files to the storage backend. Sends `"push-blobs"` command. The actual packaging (ZIP) and upload work happens server-side on the FUSE daemon.

**Parameters:**
- `args` — CLI args (currently no flags).

**Returns:** Error if upload fails.

---

### `(sc *SessionCommand) setupBlobs(args []string) error`

**Signature:** `func (sc *SessionCommand) setupBlobs(args []string) error`

Creates blob cache directories on the MonoFS filesystem and prints environment variable exports for shell evaluation. Supports `--mount` (required), `--shell` (posix/fish/json), and `--tools` (go/npm/pip/bazel/cargo or "all"). Configures environment variables for Go (`GOMODCACHE`, `GOPATH`), npm (`npm_config_cache`, `NPM_CONFIG_PREFIX`, `YARN_CACHE_FOLDER`, `PNPM_HOME`), pip (`PIP_CACHE_DIR`, `PYTHONUSERBASE`), Bazel (`BAZEL_REPOSITORY_CACHE`, `BAZEL_OUTPUT_BASE`), and Cargo (`CARGO_HOME`). Prints summary to stderr so eval only captures exports.

**Parameters:**
- `args` — CLI args with flags.

**Returns:** Error if mount point invalid or directory creation fails.

---

### `(sc *SessionCommand) searchCode(args []string) error`

**Signature:** `func (sc *SessionCommand) searchCode(args []string) error`

Performs code search using the `MonoFSSearch` gRPC service. Supports `--query` (required), `--search` (address), `--storage-id`, `--max-results`, `--case-sensitive`, `--regex`, `--file-pattern` flags. Connects to search service, builds `SearchRequest`, and displays results with `displaySearchResults`.

**Parameters:**
- `args` — CLI args with flags.

**Returns:** Error if connection or search fails.

---

### `displaySearchResults(resp *pb.SearchResponse, query string) error`

**Signature:** `func displaySearchResults(resp *pb.SearchResponse, query string) error`

Formats and displays search results. Shows total matches, files searched, duration. Warns if results are truncated. For each result, displays repository path, file path, line number, context lines (before/after), and the matched line with highlighted matches.

**Parameters:**
- `resp` — Search response.
- `query` — Original search query.

**Returns:** nil.

---

### `highlightMatches(line string, matches []*pb.MatchRange) string`

**Signature:** `func highlightMatches(line string, matches []*pb.MatchRange) string`

Highlights match ranges in a line using ANSI bold yellow color codes. Clamps match ranges to line bounds. Builds the highlighted string piece by piece.

**Parameters:**
- `line` — Original line content.
- `matches` — Match ranges.

**Returns:** Line with ANSI highlighted matches.

---

### `colorize(text string, color string, bold bool) string`

**Signature:** `func colorize(text string, color string, bold bool) string`

Adds ANSI color codes for terminal output. If no color specified, defaults to bold yellow. Supports: black, red, green, yellow, blue, magenta, cyan, white. Returns unmodified text for unknown colors.

**Parameters:**
- `text` — Text to colorize.
- `color` — Color name (empty = yellow).
- `bold` — Whether to apply bold formatting.

**Returns:** ANSI-colored text string.

---

## cmd/monofs-trace-dump/main.go

Package: `main`

A CLI tool for dumping and querying distributed traces from the MonoFS cluster via gRPC streaming.

### Structs

**`config`** — Holds all CLI configuration: addr, api, traceID, service, from, to, rangeSpec, format, output, timezone, timeout, limit, showVer.

**`timeWindow`** — A time range with `from` and `to` time.Time values.

---

### `main()`

**Signature:** `func main()`

Entry point. Parses flags, resolves timezone and time window, calls `dumpTraces` to fetch spans via gRPC streaming, resolves output, and writes spans in the requested format. Exits with error on failure.

---

### `parseFlags(args []string) (config, error)`

**Signature:** `func parseFlags(args []string) (config, error)`

Parses CLI flags: `--addr`, `--api`, `--trace-id`, `--service`, `--from`, `--to`, `--range`, `--format`, `--output`, `--timezone`, `--timeout`, `--limit`, `--version`. Validates `--limit >= 0` and `--timeout > 0`. Rejects unexpected positional args.

**Parameters:**
- `args` — CLI arguments (os.Args[1:]).

**Returns:** Parsed `config` or error.

---

### `resolveOutput(path string) (io.Writer, *os.File, error)`

**Signature:** `func resolveOutput(path string) (io.Writer, *os.File, error)`

Resolves the output destination. Empty or `"-"` returns stdout. Otherwise opens the file for writing.

**Parameters:**
- `path` — Output file path.

**Returns:** Writer, file handle (nil for stdout), error.

---

### `parseLocation(name string) (*time.Location, error)`

**Signature:** `func parseLocation(name string) (*time.Location, error)`

Parses a timezone name. `"UTC"` (case-insensitive) or empty returns `time.UTC`. `"local"` returns `time.Local`. Otherwise uses `time.LoadLocation`.

**Parameters:**
- `name` — Timezone name.

**Returns:** `*time.Location` or error.

---

### `resolveWindow(fromSpec, toSpec, rangeSpec string, now time.Time, loc *time.Location) (timeWindow, error)`

**Signature:** `func resolveWindow(fromSpec, toSpec, rangeSpec string, now time.Time, loc *time.Location) (timeWindow, error)`

Resolves the time window from `--range` or `--from`/`--to` specifications. Supports `"start to end"` and `"start,end"` formats for `--range`. Requires either `--from` or `--range`. Validates that start is before end.

**Parameters:**
- `fromSpec` — Start time spec.
- `toSpec` — End time spec (defaults to now).
- `rangeSpec` — Combined range spec.
- `now` — Current time as reference.
- `loc` — Timezone for absolute timestamps without offset.

**Returns:** Resolved `timeWindow` or error.

---

### `splitRangeSpec(spec string) (string, string, error)`

**Signature:** `func splitRangeSpec(spec string) (string, string, error)`

Splits a range specification into start and end parts. Supports `"start to end"` and `"start,end"` formats. Validates both parts are non-empty.

**Parameters:**
- `spec` — Combined range string.

**Returns:** Start and end time specs, or error.

---

### `parseTimeSpec(spec string, now time.Time, loc *time.Location) (time.Time, error)`

**Signature:** `func parseTimeSpec(spec string, now time.Time, loc *time.Location) (time.Time, error)`

Parses a time specification:
- `"now"` → current time.
- `"now-2h"`, `"now-15m"`, `"now+30s"` → relative offsets.
- RFC3339/RFC3339Nano with timezone.
- Absolute timestamps without timezone (use `loc`).
- Formats: `2006-01-02 15:04:05`, `2006-01-02 15:04`, `2006-01-02T15:04:05`, `2006-01-02T15:04`, `2006-01-02`.

**Parameters:**
- `spec` — Time spec string.
- `now` — Current time reference.
- `loc` — Location for zone-less timestamps.

**Returns:** Parsed `time.Time` or error.

---

### `dumpTraces(ctx context.Context, cfg config, window timeWindow) ([]logengine.SpanRecord, error)`

**Signature:** `func dumpTraces(ctx context.Context, cfg config, window timeWindow) ([]logengine.SpanRecord, error)`

Connects to the MonoFS cluster via gRPC and streams trace data. Based on `--api`, uses either `MonoFSRouterClient.StreamQueryTraces` or `MonoFSClient.StreamQueryTraces`. Builds `QueryTracesRequest` with trace ID, service, time window, and limit. Reads all items from the stream, unmarshals JSON into `SpanRecord` structs, and sorts them by timestamp.

**Parameters:**
- `ctx` — Context.
- `cfg` — CLI config.
- `window` — Time window for query.

**Returns:** Sorted slice of `SpanRecord` or error.

---

### `sortSpans(spans []logengine.SpanRecord)`

**Signature:** `func sortSpans(spans []logengine.SpanRecord)`

Sorts spans by timestamp, then end time, then trace ID, then span ID.

**Parameters:**
- `spans` — Spans to sort (in-place).

---

### `writeOutput(file io.Writer, spans []logengine.SpanRecord, format string) error`

**Signature:** `func writeOutput(file io.Writer, spans []logengine.SpanRecord, format string) error`

Writes spans in the specified format:
- `"json"` → compact JSON.
- `"json-pretty"` / `"pretty"` / `""` → indented JSON.
- `"table"` → tab-separated table with columns: START, END, SERVICE, TRACE_ID, SPAN_ID, PARENT_SPAN_ID, STATUS, NAME.

**Parameters:**
- `file` — Output writer.
- `spans` — Spans to write.
- `format` — `"json"`, `"json-pretty"`, or `"table"`.

**Returns:** Error if format is unknown or write fails.

---

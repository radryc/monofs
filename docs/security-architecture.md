# Security Architecture

**Scope:** code-derived security architecture and assessment. Findings are based on the current code and checked-in deployment files, not on an assumed production hardening layer that is absent from the repository.

## 1. Method and severity

Severity reflects impact in the checked-in default deployment and the likely production topology implied by the code:

- **Critical** — unauthenticated network access leads to cluster control, arbitrary code execution, or host/filesystem compromise.
- **High** — authentication/authorization bypass, credential leakage, cross-tenant data access, destructive operations by a low-privilege principal, or supply-chain compromise.
- **Medium** — defense-in-depth failure, limited-scope bypass, denial of service, or a control that is only effective under specific configuration.
- **Low** — hardening/cleanup with limited direct impact.

The code has several “controls” that are disabled by default or only active when an operator supplies a policy source. Those are treated as absent for the default posture, with the condition stated.

**Remediation status:** this document describes the current controls and the
remaining risks. Treat the risk sections as current behavior, not as a snapshot
of an older tree.

## 2. Assets

| Asset | Where it lives | Why it matters |
|---|---|---|
| Repository source and file content | Fetcher archives, node blobs, Git caches, client overlay | Confidential source code, secrets accidentally committed, customer data |
| File metadata and directory structure | Pebble on storage nodes | Reveals repository names, paths, sizes, activity |
| Git credentials and forge tokens | Process environment (`MONOFS_GIT_TOKEN*`, `MONOFS_GITHUB_TOKEN*`, `MONOFS_GITLAB_TOKEN*`) | Read/write access to upstream repositories and forge APIs |
| `MONOFS_TOKEN` | Environment/flags on router, server, search, registry, admin, clients | Currently a master secret across five trust domains |
| Guardian tokens and principal store | Router state dir, message payloads | Control of partitions, queues, scans, pipeline config, Doctor paths |
| OIDC client secret, ID tokens, session cookies | Router env, browser cookie, session files | Human identity and UI/API access |
| Encryption key | Fetcher/router config/env | Decrypts every blob archive |
| Pipeline task content and worker credentials | Guardian queues, worker environment | Arbitrary CI code execution, deploy credentials |
| Registry images and tags | MonoFS `docker-registry` namespace | Supply-chain integrity for deployed workloads |
| Telemetry logs/metrics/traces | Doctor log engine | Secrets in logs, operational intelligence, tenant data |
| Bazel cache/executor content | Local disk, CAS | Build integrity and remote code execution |
| Cluster topology and node addresses | Router state, UI | Attack surface discovery |

## 3. Threat model

### 3.1 Actors

| Actor | Capability assumed |
|---|---|
| Unauthenticated network client | Can reach published ports or any service on the shared bridge network |
| Authenticated human | Valid OIDC session/bearer; may have no admin role |
| Guardian client | Valid Guardian token; roles include control-plane, cli, pipeline, pusher, doctor |
| Pipeline worker / CI job | Can submit tasks and execute repo-controlled commands |
| Compromised container | Can reach other services on the same Docker network and read its own env/secrets |
| Malicious repository content | Controls file paths, symlinks, scripts, CODEOWNERS/OWNERS, Bazel config, webhook payloads |
| Malicious upstream registry/Git server | Controls responses, redirects, token realms, Git objects |
| Supply-chain attacker | Can influence unpinned dependencies, base images, CI checkout refs, release artifacts |

### 3.2 Trust boundaries

| Boundary | Current enforcement | Problem |
|---|---|---|
| Network → router gRPC `:9090` | Optional bearer interceptor in observe mode; per-method checks; mTLS optional | Most RPCs have no authentication or authorization |
| Network → router HTTP `:8080` | OIDC/bearer middleware; public path exemption; TLS when required | Coarse authorization; any authenticated user can mutate admin state |
| Network → native gateway | Shared mount token (when configured) | No per-principal authorization or read-only enforcement; plaintext TCP |
| Network → storage node gRPC `:9000` | Optional policy source; otherwise none; mTLS optional | Compose publishes nodes on loopback, still bypassing router policy |
| Network → fetcher gRPC `:9200` | No application authentication; mTLS optional | Read/write/delete content, stage/publish/push workspaces |
| Network → registry `:5000` | No application authentication; TLS when required | Pull, push, overwrite, delete images |
| Network → search `:9100` | Observe-only authenticator; mTLS optional | Unfiltered code search; delete/index operations |
| Network → pipeline executor | Bearer/`X-Monofs-Executor-Token`; mTLS optional | Any authenticated caller can still execute arbitrary commands |
| Network → Bazel cache `:9092` | No application authentication; TLS when required | Cache poisoning/read/write |
| Router → node/search | Shared `x-monofs-internal` token; plaintext unless mTLS is required | Token leak compromises policy bypass |
| Router → fetcher | No application authentication; plaintext unless mTLS is required | Any fetcher can be impersonated or poisoned |
| Guardian client → router | Payload token; plaintext unless TLS is required | Token transmitted in messages |
| Browser → router | Session cookie containing raw ID token | Replayable until expiry; no revocation/nonce |
| Container → host | Privileged/SYS_ADMIN clients, `/dev/fuse`, host ports | Container escape/credential access |

## 4. Current security architecture

### 4.1 Identity and authentication

**Human browser sessions.** OIDC Authorization Code + PKCE (S256), random state, HttpOnly/SameSite=Lax cookie. On callback, the router verifies the ID token signature against JWKS with an asymmetric algorithm allowlist, issuer, audience, and expiry. It then stores the **raw ID token** in the `monofs_session` cookie. There is no nonce, no refresh, no server-side revocation, and the opaque `SessionStore` is not used by the login flow.

**Bearer tokens for APIs.** `pkg/authz.Authenticator` reads `Authorization: Bearer` and runs a verifier chain (OIDC, static/break-glass, noop). The router constructs its gRPC authenticator with `RequireToken=false`; search does the same; storage nodes install the interceptor only when a policy file/URL exists.

**Machine tokens.** `pkg/authz` supports client credentials and a static break-glass verifier. `MONOFS_TOKEN` maps to `Identity{ClientID:"break-glass-admin"}`. There is no token minting, scoping, rotation, or expiry for internal tokens.

**Guardian tokens.** Opaque tokens are carried in message fields (`GuardianConfig.auth_token`, `guardian_token`, `token`) or the `X-Guardian-Token` header. The router stores unsalted SHA-256 hashes in `guardian_principals.json`, validates by hash lookup, and infers a role from the client ID. `RegisterClient` requires a verified caller identity; only break-glass/admin callers may assert another principal or role, and other callers register themselves as `viewer`.

**Internal cluster token.** `x-monofs-internal` (or a bearer equal to the token) marks a call internal on storage nodes/search and bypasses read policy. The default value is `MONOFS_TOKEN`.

**Client identity.** `x-client-id` / `X-Client-ID` / `client_id` is used for ingestion whitelist and workspace attribution. It is caller-asserted and not cryptographically bound.

### 4.2 Authorization layers

Runtime authorization has one source of truth:

1. **`authz/policy`** — `access.yaml` compiled into a bundle; partition/log/trace/MonoFS path grants, defaults, selectors, AWS bindings; loaded by router/nodes/search and Guardian. Enforcement is opt-in; observe mode logs and allows. `admin` is scope-aware: wildcard partitions are global, explicit lists are scoped.
2. **OWNERS / CODEOWNERS** — repo-maintained inputs for merge governance and Doctor endpoint ownership. They are not a second runtime grant store.
3. **Per-handler role checks** — pipeline/pusher namespace scoping and merge-request owner checks.
4. **In-memory ingestion whitelist** — exact client ID allowlist; disabled by default; resets on restart.
5. **Component-specific** — registry has none; search filters after querying; pipeline has no admin role for UI actions; Bazel cache/executor have none.

### 4.3 Transport security

Transport security is available through `internal/transport`, which resolves the shared `STRATA_TLS_*` contract (`github.com/radryc/tlsx`) and is wired into the router, storage nodes, fetcher, search, registry, admin, client, session, cache, and load generator:

- `STRATA_TLS_MODE=off` is the default and keeps gRPC and HTTP plaintext;
- `optional` terminates TLS/mTLS on gRPC when a certificate/key exist and keeps HTTP plaintext for local UIs and probes;
- `required` serves TLS/mTLS on gRPC and HTTP and verifies client certificates unless `STRATA_TLS_REQUIRE_CLIENT_CERT=false`.

The native gateway remains raw TCP, and diagnostics servers are not wrapped. HAProxy in the checked-in Compose topology is still plain TCP/HTTP. The repository now includes `docker-compose.mtls.yml` for the local stack, but the default deployment is plaintext, so bearer tokens, session cookies, Guardian tokens, internal tokens, Git credentials used by the fetcher, and file content are cleartext unless an operator enables TLS or adds an external terminator.

### 4.4 Secrets and key management

- `MONOFS_TOKEN` is a single master secret reused for break-glass admin, Guardian control-plane seeding, node/search internal bypass, peer-router auth, registry auth, ingest, and `dogfood`.
- `MONOFS_ENCRYPTION_KEY` is a 32-byte key; all fetchers/routers must share it. It can be passed via flag (visible in `ps`), env, or JSON config. The fetcher key guard stores only a SHA-256 fingerprint and auto-accepts the first key.
- Git tokens are static environment variables; the per-repo override key omits the host, so a token for `github.com/example/strata` is also sent to `https://attacker.example/example/strata.git`. Plain `http://` is accepted.
- Browser session cookies contain the raw ID token.
- The persistent session file, if used, is JSON and trusts any unexpired identity on disk.
- Registry upstream username/password flags are parsed but never used.
- No secret redaction helper exists for URLs/errors/logs.

### 4.5 Data protection and tenancy

- Blob archives are encrypted at rest with the shared encryption key.
- Metadata (Pebble), workspace WAL/snapshots, log-engine Parquet/manifests, cache DBs, and registry manifests/tags are **plaintext** by default.
- Optional telemetry encryption (`MONOFS_TELEMETRY_ENCRYPTION=envelope`) seals logengine chunk objects and sensitive ClickHouse payload columns with tenant/partition subkeys. ClickHouse can override the global setting with `MONOFS_CLICKHOUSE_ENCRYPTION=inherit|off|read|envelope`. It is off by default. `envelope` writes new data encrypted and reads legacy plaintext only in `observe` mode; `read` stops encrypting new writes but still decrypts existing envelopes so a deployment can be toggled off without stranding historical data; `enforce` rejects plaintext. The DuckDB query sidecar (`monofs-logquery`) and the offline `monofs-logengine-compact` command consume the same settings, so the node and sidecar always decrypt with the same keys. The key file is supplied by the operator (Guardian Secret or inline JSON for the demo); keys are never logged. DuckDB cannot read envelope-encrypted objects directly, so the query sidecar materialises decrypted Parquet under `<logengine-dir>/parquet` for queries; those copies are swept after one hour. Mount that cache on an ephemeral volume if plaintext on the node disk violates the threat model.
- There is no tenant identifier in paths, storage IDs, metadata keys, blob indexes, log chunks, workspace jobs, or cache keys. A single cluster is one trust domain.
- Blob indexes are keyed only by content hash; any caller who knows a hash can fetch that content regardless of repository/tenant.
- Registry blobs are global by digest; cross-repository blob reads and deletes are possible.
- Search indexes/caches are created `0755`, shards `0644`, and cloned private repos may be readable by other local users.
- The FUSE overlay stores fetched repository content outside the mount permission model at `0755/0644`; `AllowOther` exposes read-only content to all local users.

### 4.6 Audit and monitoring

- Prometheus metrics exist for ingest, Guardian flows, native reads, workspace sync, cluster gauges, auth outcomes.
- Metrics are missing for access-policy allow/deny, Guardian auth failures, whitelist changes, deletions, failover assignments, and ownership-gate decisions.
- Auth outcome metrics are not emitted for HTTP in browser-OIDC mode because the token authenticator is replaced, not layered.
- Cluster gauges are only refreshed when the UI status endpoint is polled.
- Doctor stores telemetry but there is no security audit log with actor/action/resource/decision.
- Delete/partial-failure behavior often reports success, which undermines incident reconstruction.

### 4.7 Runtime and supply chain

- Server/router/registry/fuse-plugin images run as root.
- Client containers run privileged with `SYS_ADMIN`, `/dev/fuse`, unconfined AppArmor, SSH enabled, a known `monofs` password, and passwordless sudo.
- MinIO uses `minioadmin/minioadmin` and makes the bucket publicly readable.
- Node gRPC ports `9001`-`9005` are published on loopback; they are no longer reachable on external interfaces but still bypass the router/HAProxy path locally.
- CI checks out `radryc/kvs`/`cfg` default branches while Docker pins different SHAs.
- `go.sum` was deleted on release and Docker builds disabled checksum verification. **Fixed in the working tree**; see the remediation decisions.
- Base images and tools are mostly tags/`latest`; GitHub Actions use mutable major tags.
- The Docker CI matrix now builds `server`, `router`, `search`, and `logquery` with explicit `target:` values.
- `Dockerfile.pipeline-worker` and `Dockerfile.guardianctl` depend on an external `guardian` build context not in this repository.

## 5. Component control matrix

| Component | Authentication | Authorization | Transport | Secrets | Audit |
|---|---|---|---|---|---|
| Router gRPC | Observe-only bearer | Per-method; many RPCs none | Plaintext default; mTLS optional | `MONOFS_TOKEN`, OIDC secret | Partial metrics |
| Router HTTP | OIDC/bearer | Any authenticated user for many mutations; repo ACL reads | Plaintext default; TLS when required | Session cookie, OIDC secret | Partial |
| Router native | Shared mount token (when enabled) | None | Plaintext TCP | `MONOFS_TOKEN`/`--native-auth-token` | Native metrics only |
| Storage node | Optional policy source; internal token | Read policy only; no ingest/delete authz | Plaintext default; mTLS optional | Internal token, policy token | Partial |
| Fetcher | None | None | Plaintext default; mTLS optional | Git tokens, encryption key | Counters only |
| FUSE client | None locally | OS file permissions + mount owner | Plaintext default; TLS optional | Client token in memory | Local metrics |
| Search | Observe-only token | Post-query filter; off by default | Plaintext default; mTLS optional | Internal token, Git token | Partial |
| Registry | None | None | HTTP plaintext default; TLS when required | Guardian token | Process counters |
| Pipeline worker | Guardian token on queue | Task claim; no executor auth | Plaintext default; mTLS optional | Worker env/cloud creds | Task results only |
| Pipeline executor | Bearer/dedicated token required | None (any authenticated caller can execute) | Plaintext default; TLS optional | Inherits worker env | None |
| Bazel cache | None | None | HTTP plaintext default; TLS when required | None | Counters |
| Doctor | Depends on node/router auth | None on node Doctor RPCs | Plaintext default; mTLS optional | None | Log data |
| UI | OIDC/bearer | No admin role for mutations | Plaintext default; TLS when required | Raw ID token cookie | Partial |
| Kernel module | Token option enforced by gateway | None | Plaintext TCP | Token in mount option | None |

## 6. Findings

### 6.1 Critical

#### C1. Router gRPC control plane is unauthenticated in practice

`cmd/monofs-router/main.go` builds the gRPC authenticator with `RequireToken=false`. `pkg/authz/interceptor.go` documents that observe mode never rejects a request. Destructive and control RPCs have no handler-level authorization:

- `DeleteRepository` (`internal/router/delete.go`)
- `DrainCluster` / `UndrainCluster` (`drain.go`)
- `RequestFailover` (`router.go`)
- whitelist CRUD/toggle (`whitelist.go`)
- `UnregisterClient` (`router.go`; `RegisterClient` now requires a verified identity)
- `UploadWorkspaceBundle` / `PublishWorkspace` / `PushWorkspaceCommits` / `RefreshWorkspace`
- `GetWorkspaceSyncJob` / `ListWorkspaceSyncJobs` / `CancelWorkspaceSyncJob`
- `QueryLedger`, `GetUpstream*`, `GetClusterStats`, `GetNodeStats`

Anyone who can reach `:9090` can delete data, disable failover behavior, unregister clients, read telemetry/upstream data, and start workspace writes.

#### C2. Guardian principal registration is self-service and cannot be revoked

`RegisterClient` now requires a verified caller identity. Break-glass/admin callers may assert another principal or role (for example `guardiand`); other callers are persisted as `viewer` and cannot self-assign `control-plane`. The remaining problem is lifecycle: any authenticated identity can persist its own Guardian token, and there is no disable/delete/rotate/revoke RPC; `UnregisterClient` removes the client record but leaves the persisted principal token valid (`guardian_principals.go`). A leaked control-plane token therefore remains usable for every Guardian path, including pipeline configs, queues, and partitions.

#### C3. TLS/mTLS is implemented but off by default

`internal/transport` wraps the gRPC and HTTP listeners and clients in the router, storage nodes, fetcher, search, registry, admin, client, session, cache, and load generator with the shared `STRATA_TLS_*` contract. With the default `STRATA_TLS_MODE=off`, all of those connections stay plaintext; `optional` encrypts only gRPC and leaves HTTP plaintext. The native gateway, diagnostics servers, and checked-in HAProxy config are not covered. Tokens, cookies, Git credentials, and content therefore cross the network in cleartext unless the operator sets `required` (or an external terminator is added).

#### C4. Native gateway has no authorization and only a shared token

`router.NativeGateway` now checks `MountRequest.AuthToken` against the configured shared token and fails startup when the native listener is enabled without one. It still has no per-principal authorization; `MountFlags` are ignored, so read-only mount semantics are not enforced; `Lookup` accepts `.` and `..`; and reads can exceed the advertised 256 KiB limit. **Status:** token enforcement implemented; authorization/read-only enforcement remains open.

#### C5. Fetcher plane has no application authentication

`BlobFetcher` and `RepoSyncWorker` expose `StoreBlob`, `StoreBlobBatchStream`, `DeleteBlobs`, `StoreArchive`, workspace staging, publish, source push, and upstream reads with no application auth. Any network client that can reach `:9200` can read/poison/delete content, stage bundles, and push to upstream repositories using the fetcher's credentials; mTLS protects the channel only when `STRATA_TLS_MODE=required`. `fetcher.Client` has no token field.

#### C6. Pipeline executor is authenticated remote code execution

`POST /execute` accepts arbitrary `arguments`, environment variables, input/output files, working directory, and timeout. It now requires a bearer/`X-Monofs-Executor-Token` credential and bounds the request body, but still runs `exec.CommandContext` with the worker's full environment, no sandbox and no resource limits; transport TLS applies only when `STRATA_TLS_MODE=required`. **Status:** authentication implemented; sandboxing/limits remain open.

#### C7. Client containers are privileged SSH boxes with a known password

The client image enables SSH password authentication for `monofs:monofs`, grants passwordless sudo, and Compose runs it with `privileged: true`, `SYS_ADMIN`, `/dev/fuse`, and unconfined AppArmor. Anyone reaching ports 2222-2224 can log in, become root, access the mount and host devices, and potentially reach the cluster network.

#### C8. Release process removed dependency integrity

The release workflow used to delete every `go.sum` and Docker builds disabled checksum verification. **Status:** the workflow/action were removed and checksum verification is restored; pinning/signing remain open.

### 6.2 High

#### H1. Guardian read APIs lack role/scope authorization

`ListGuardianVersions`, `GetGuardianVersion`, and `SubscribeGuardianChanges` validate only that the token is valid. Any Guardian token (pusher, doctor, pipeline) can read every partition's version history and subscribe to all changes with an empty prefix. Slow subscribers are dropped with no cursor/replay.

#### H2. Guardian HTTP deletes accept any Guardian token

`DELETE /api/guardian/partitions/{name}[/files|/dirs]` validates the token but not the role, and passes `?path=` to deletion helpers without validation. Any pusher/doctor token can delete files/directories/partitions.

#### H3. Storage node read policy is optional and bypassable

`server.NewAccessControl` returns nil without a policy file/URL, so no interceptor and no read checks are installed. Compose publishes nodes on loopback. When policy is installed:

- `PolicyAllowsPath` allows root and any path shape that `SplitDisplayPath` cannot parse (including short repo paths such as `myrepo` or `docker-registry/foo`).
- `ReadDir` of root/intermediate dirs is allowed to everyone.
- Ingest, delete, replication, discovery, ledger, and Doctor RPCs have no authorization.
- Forwarded RPCs drop the caller's bearer/`x-client-id`.

#### H4. Search is unauthenticated and unfiltered by default

The authenticator is `requireToken=false`; `applyReadPolicy` returns all hits when no policy is configured, when enforcement is off, or when the caller is anonymous in observe mode. `Search` does not require a storage ID and supports repository filters. `DeleteIndex` with an empty/unknown storage ID removes every `.zoekt` shard. `IndexRepository` accepts arbitrary `StorageID`/`DisplayPath`/source, enabling local path deletion and Git SSRF.

#### H5. Registry has no application authentication

Every `/v2/` route is open, including push, tag overwrite, and delete. TLS is available when `STRATA_TLS_MODE=required`, but the API itself has no auth. Blobs are global by digest, so deleting one repository's layer can break another; any client can replace `latest`. There is no `WWW-Authenticate` challenge, so standard Docker auth cannot be negotiated without protocol changes. The proxy stores upstream blobs without digest verification and accepts HTTP upstreams.

#### H6. Access-policy read controls are bypassed or dead

- `/api/routers` embeds unfiltered repository data from local and peer routers.
- `/api/merge-requests` lists all proposals.
- Workspace job cancellation is not filtered.
- Search, registry, telemetry, upstream reads, and native reads have no repository ACL.
- `filterRepositoriesData` unfilters entries with empty `repo_id`.
- `canReadWorkspaceJob` uses OR semantics, exposing all repos in a multi-repo job.
- `/api/fs/ls` lists arbitrary host directories.
- `/api/file/content` and Guardian deletion paths do not clean/contain `?path=`.

#### H7. Any authenticated user can perform administrative/security mutations

The access-policy enforcement toggle is admin-gated, but no admin role is required for:

- whitelist add/remove/toggle;
- rebalance, repository refresh, pprof collection;
- Guardian partition inject/delete (any Guardian token);
- pipeline run/cancel/unregister;
- fetcher key confirmation.

The UI's "authenticated" check is not an authorization model.

#### H8. `MONOFS_TOKEN` is a master secret across five trust domains

It is simultaneously break-glass admin, Guardian control-plane seed, internal node/search bypass, peer-router token, registry token, ingest token, and default `dogfood` token. A leak compromises every layer. There is no scoping, rotation, per-service identity, or audit binding.

#### H9. Ingestion whitelist is spoofable and disabled by default

The whitelist trusts `x-client-id`/`client_id`/`X-Client-ID`, all caller-controlled. It is in-memory, resets on restart, and the file-upload endpoint skips it. Local filesystem ingestion is enabled by default, so a remote caller can ask the router to ingest router-local paths and expose them.

#### H10. Git credentials are not bound to hosts, and SSRF is unrestricted

`gittoken.Slug` omits the host. A token for `github.com/example/strata` is sent to any host with the same owner/repo path. `IsHTTPS` accepts `http://`. `AuthForRepo` applies the global token to any HTTPS URL. Repository URLs are accepted from ingestion/workspace/webhook/API callers and passed to go-git, which supports `file://` and local paths. This is both credential exfiltration and SSRF.

#### H11. Workspace bundles allow symlink escape and unsigned upstream writes

`workspacebundle` validates operation paths but only requires symlink targets to be non-empty. A bundle can create `link -> /etc`, then `rename link/shadow -> stolen`, passing validation and moving host files into the repository before commit/push. The same apply code is used by publish and source push. Commits are explicitly unsigned; bundle JSON is unsigned and trusted for author, principal, storage ID, repo URL, and base commit.

#### H12. Source push bypasses the sensitive-path filter

Publish applies a deny-list for `.env`, keys, `secrets/`, etc. Source push does not. A client can push those paths upstream through the source-push API, subject only to policy/ownership checks that are not applied to publish/autopush.

#### H13. Policy bundles are unsigned and fail open without a source

`ParseBundle` accepts any JSON with `format_version=1`; the loader trusts any fetched bundle and uses ETag only for “unchanged,” not authenticity. A compromised HTTP source/MITM can serve `defaults: allow` and admin grants. If enforcement is enabled without a policy source, router/node/search code silently no-ops or allows.

#### H14. OIDC identity handling has authorization-relevant flaws

- `azp` is used as machine `ClientID`; human tokens can inherit machine grants, and `PrincipalID()` prefers `ClientID` for audit.
- A token without `sub` and `client_id` is rejected, but `exp`/`iat`/`nbf` are not required (`jwt.Claims.Validate` only checks claims that are present).
- `email_verified` is ignored while email is an authorization subject.
- No nonce, no ID-token type check, no revocation; the session cookie is the raw ID token.
- A persistent session file trusts any unexpired identity on disk.
- `/debug` is not a default OIDC exemption, but it sits outside `/api/` and is therefore anonymously reachable in token-only mode.

#### H15. Authorization model duplicates and disagrees with itself

- Three break-glass checks (`accessAdmin`, `isBreakGlassAdmin`, server `isAdmin`) plus the `AddBreakGlassAdmin` seed list still duplicate the same decision. They now accept both `Subject` and `ClientID`, but each reimplements the magic-string rule and there is no role-based replacement.
- Admin semantics are now scope-aware: wildcard admin is global, explicit partition lists are scoped. Missing `logs`/`traces`/`monofs` lists still deny those signals even for an admin.
- Policy `defaults.partitions: allow` makes `CanSeePartition` true but `VisiblePartitions` returns empty.
- Empty selector labels match absent labels; selectors can shadow partition names; missing catalog silently drops rules.
- CODEOWNERS overmatches middle-slash patterns and parses inline comments as owners; it is not wired in the router.
- OWNERS `.guardian/OWNERS` is a dot-prefixed machinery path, never persisted, so ownership gate fails open after restart.

#### H16. Path traversal and containment are not enforced at trust boundaries

- Router `/api/fs/ls` reads any host directory.
- Router file content and Guardian delete paths do not clean/contain.
- Server accepts `..`, empty components, NUL, and arbitrary `storageID`; keys are delimiter-ambiguous, so a `storageID` containing `:` can alias another repo.
- Blob `StoreArchive`/`Cleanup`/`StoreBlob` interpolate caller identifiers into filesystem paths.
- Fetcher `StoreBlob`/`StoreArchive` accept arbitrary hash/storage ID; no hash verification.
- Search `IndexRepository` uses `StorageID` in `filepath.Join(cacheDir, ...)` then `os.RemoveAll`, allowing cache escape/deletion.
- Registry tags/repos/digests are concatenated into keys without validation.
- Kernel gateway `Lookup` accepts `.`/`..`.

#### H17. Registry integrity and upload controls are weak

- Proxy fetches are stored under the requested digest without verification.
- Chunked blobs are stored chunk-by-chunk with no completion marker; a later GET silently truncates at the first missing chunk and returns 200.
- Unknown `Content-Length` (-1) stores an empty blob and ignores the body.
- Upload sessions are in-memory, global-lock serialized, unbounded, and not cleaned on shutdown.
- Manifest PUT stores the blob before digest verification; orphan blobs remain on mismatch.
- No reference counting; deleting a shared blob breaks other repos.
- `HEAD` requires size > 0, so empty blobs are reported missing.

#### H18. Supply chain and deployment defaults

- Compose publishes node ports on loopback and both networks are ordinary bridges; the “DMZ” is conceptual.
- MinIO default credentials and public-read bucket.
- Server/router/registry/plugin images run as root.
- CI checks out unpinned sibling repos; Docker pins different SHAs. `go.sum` deletion is fixed, but the pinning/signing work remains.
- Base images/tools are unpinned.
- Pipeline examples invoke non-existent targets, encouraging unsafe ad-hoc deployment.

### 6.3 Medium

- **Anonymous enforce-mode bypass:** `Authenticator.authenticate` accepts `(Identity{}, nil)` even with `RequireToken=true`; `NoopVerifier` always does this.
- **JWKS DoS/race:** unknown `kid` triggers a JWKS fetch before signature verification, with no single-flight/cooldown; `jwksURL` is written without a lock.
- **Token cache:** valid tokens are cached for up to 5 minutes, delaying revocation; raw tokens are retained in memory.
- **Policy loader:** parses/indexes under the write lock; never sends `If-None-Match`; trusts reused ETags.
- **CODEOWNERS matcher:** recursive `**` can be exponential; no memoization.
- **Ownership TOCTOU:** OWNERS are read from the live store then the write proceeds; a concurrent OWNERS change can invalidate the decision.
- **Grant store:** whole-file rewrite with no lock/fsync; two replicas can lose updates; load bypasses validation.
- **Guardian versions:** asset version metadata is persisted in Pebble (bounded to 50 versions per path) but content is only a bounded in-memory cache; machinery paths (dot-prefixed) are ephemeral; deletions leave child versions “current.”
- **Guardian upsert/delete not transactional:** node writes happen before version commits; partial groups can diverge.
- **Workspace jobs/bundles:** replayable for 30 minutes, unbounded memory, not removed by fetcher after publish/push; no job retention.
- **Workspace WAL:** default `FsyncEnabled=false`; snapshots/checkpoints not fsynced; corrupt checkpoint can lose compacted history.
- **Blob cache:** global lock across cloud HEAD/upload (5-minute timeouts); shared archive readers can be closed in-flight; no refcount.
- **Blob integrity:** no hash verification on read or write; cloud backup checks existence only; local index loss makes cloud-only archives undiscoverable.
- **Fetcher stage/publish SSRF:** arbitrary repo URLs; file:// accepted; no allowlist; worktree operations follow symlinks.
- **Search:** indexes/caches world-readable; unbounded queries/results; no query timeout; full clone per job.
- **Registry:** no timeouts, no range support, no pagination/catalog, no manifest validation, no referrers, no upload resume.
- **Pipeline:** retry task loses steps and runner type; matrix jobs collapse into one status; failed/cancelled dependencies count as satisfied; cancelled runs can be overwritten to succeeded; runs are in-memory; webhooks under `/api/` are unreachable by real forges; tag events never have a SHA.
- **Pipeline KVS:** claims are not atomic and have no expiry, so a crashed worker can leave a task stuck.
- **Bazel cache:** unauthenticated AC/CAS; AC PUT accepted without validation; CAS PUT buffers whole body before size check.
- **Bazel migration:** `GenerateFallbackGenrule` interpolates repo-controlled action into a Bazel command; `RemoveFallbackGenrules` deletes any BUILD containing the marker; display paths are joined without containment.
- **Pipeline executor:** lexical `resolveWithin`; `.`/empty working dir can delete the executor root; symlink output exfiltration; no process-group kill; env leakage.
- **Native protocol:** `DecodeReadDirResponse` allocation amplification (~40 MB from 1 MiB), `MaxReadBytes`/`MaxBytes` not enforced, 32-bit length overflow. A shared mount token is now required, but the transport is still unencrypted.
- **Diagnostics:** `/metrics` is unauthenticated on all interfaces; pprof is added only with `MONOFS_DIAGNOSTICS_PPROF=true` and is then also unauthenticated; no HTTP timeouts.
- **FUSE:** socket `rm` accepts `..` (arbitrary remove/chmod), `current` symlink escape (chmod/RemoveAll outside overlay), dependency symlink exfiltration, world-readable overlays, hardcoded writable path, write-then-crash invisibility, backend error treated as empty file.
- **Registry proxy:** token realm trusted from `WWW-Authenticate` (blind SSRF); no size limits on manifests/tags; upstream credentials parsed but unused.
- **Kernel module:** `FS_USERNS_MOUNT` allows unprivileged mounts; plaintext token; no frame-size limit in kmod; no reply request-ID validation; partial send desync; readdir capped at 256 entries.

### 6.4 Low / hardening

- `monofs-admin` destructive commands have no token flag, so they cannot work if gRPC auth is later enforced.
- `Authenticate` accepts any token and returns a 24-hour session.
- `--insecure-dev-disable-auth` is a single env-var kill switch.
- Client cache deletion is destructive (`--cache` directory removed unless `--keep-cache`).
- `--overlay` silently enables writable mode.
- `--writable` without `--overlay` hardcodes `/home/monofs/.monofs/overlay`.
- SIGHUP is treated as shutdown.
- HTTP servers have no timeouts; shutdown can block indefinitely on streams.
- `Close` can panic if `StopHealthCheck` is called twice.
- Access-policy loader is never canceled.
- `filterWorkspaceJobs`/`filterRepositoriesData` have edge-case fail-open behavior.
- Registry `/health` always returns ok.

## 7. Positive controls worth preserving

- PKCE S256 implementation and state binding are correct.
- JWT verification uses an asymmetric algorithm allowlist by default and validates issuer/audience/expiry.
- Static tokens are stored as hashes, not plaintext, in the verifier and Guardian principal store.
- Anonymous identities are denied by every `policy.Policy` method except metrics.
- Ingest authorization fails closed when enforcement is on and the policy bundle is missing.
- The ownership gate fails closed on resolver errors (when it is enabled and correctly wired).
- Blob archives are encrypted at rest and use content-addressed entry names.
- The shared `internal/transport` helper implements verified TLS/mTLS (including optional SPIFFE allowlists) for gRPC and HTTP; it is off by default but needs no new code to enable.
- Workspace bundles validate relative paths, reject `.git` components, and reject `.`/`..` escapes for operation paths.
- The embedded KVS test demonstrates repository namespace isolation.
- The fetcher key guard refuses writes when the key fingerprint is pending (except the streaming batch path).
- Guardian mutation roles are namespace-scoped for pusher/doctor/pipeline (though the pipeline role is over-broad).

## 8. Secure deployment checklist

Before running MonoFS outside a trusted lab:

- [ ] TLS/mTLS enabled (`STRATA_TLS_MODE=required`), including the native gateway and diagnostics; no plaintext internal traffic.
- [ ] Router gRPC `RequireToken=true`; every destructive RPC authorized.
- [ ] Guardian principal registration requires admin/control-plane credentials.
- [ ] Native gateway disabled or authenticated and network-restricted.
- [ ] Fetcher, search, registry, pipeline executor, and Bazel cache on private networks with auth.
- [ ] `MONOFS_TOKEN` split into scoped secrets; no reuse across services.
- [ ] Git tokens host-bound; repository URL allowlist; `file://`/SSH policy explicit.
- [ ] Storage node ports not published beyond loopback; only HAProxy/ingress exposed.
- [ ] Client containers unprivileged; no SSH password; no `SYS_ADMIN` unless required.
- [ ] MinIO/S3 private, strong credentials, encryption, no public bucket.
- [ ] Registry auth + TLS + per-repo ownership; image signature verification.
- [ ] `go.sum` present; checksums enforced; pinned images/dependencies; signed releases.
- [ ] Policy source configured and enforcement enabled; startup fails if missing.
- [ ] Admin role checks on UI/API mutations; CSRF/CSP/security headers.
- [ ] Audit logging and metrics for security decisions.
- [ ] Backups and restore tests for Pebble, workspace WAL, blob archives, and KVS.

## 9. Open questions and residual risks

- The repository now includes the shared `STRATA_TLS_*` helper and `docker-compose.mtls.yml`; an external TLS terminator, HAProxy image, or Guardian deployment that production may rely on is not verifiable here.
- Several findings depend on configuration: for example, node auth is absent only when no policy source is configured; search filtering is absent when enforcement is off; transport is plaintext unless `STRATA_TLS_MODE` is `optional` or `required`. The default checked-in configuration is permissive.
- Some code comments and older docs still describe behavior that has since changed (stateless fetchers, “DMZ” isolation, implemented S3 ingestion, Guardian grants). Treat comments as intent, not evidence.
- No penetration test, fuzzing, or dynamic analysis was performed. The assessment is static and code-derived.
- The kernel module was reviewed statically; no runtime exploitability testing was performed.

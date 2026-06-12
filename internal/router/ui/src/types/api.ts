// API response types matching Go server exactly

export interface NodeStatus {
  id: string
  address: string
  healthy: boolean
  weight: number
  status: string
  file_count: number
  // Disk fields are flat (not nested) in the actual API response
  disk_used: number
  disk_total: number
  disk_free: number
  kvs: KVSInfo
  // backing_up is a string[] of node IDs being covered (omitted when empty)
  backing_up?: string[]
  // covered_by is set on unhealthy nodes to indicate which node covers them
  covered_by?: string
  sync_progress: number
}

export interface KVSInfo {
  enabled: boolean
  healthy: boolean
  mode: string      // 'local' | 'raft'
  role: string      // 'leader' | 'follower' | 'disabled' | ...
  leader_id: string
  peer_count: number
  key_count: number
}

export interface StatusData {
  nodes: NodeStatus[]
  // failovers is a map of failed_node_id -> backup_node_id
  failovers: Record<string, string>
  drain_mode: { active: boolean; reason?: string; drained_at?: number; duration?: number }
  version: { version: string; commit: string; build_time: string }
}

export interface Repository {
  storage_id: string
  repo_id: string
  repo_url: string
  branch: string
  commit_hash: string
  commit_time?: number        // Unix timestamp (seconds)
  commit_message?: string
  files_count: number
  total_files?: number        // Total files (in-progress only)
  ingested_at: number         // Unix timestamp (seconds)
  topology_version?: number
  rebalance_state: string
  rebalance_progress: number
  target_topology?: number
  guardian_url: string
  product_kind: string
  product_ui_url?: string
  product_ui_label?: string
  is_guardian?: boolean
  is_doctor?: boolean
  in_progress?: boolean
  stage?: string
  message?: string
}

export interface RepositoriesData {
  repositories: Repository[]
  current_topology_version: number
  in_progress?: Repository[]
}

export interface RouterInfo {
  address: string
  name: string
  url: string
  local: boolean
  status: StatusData
  repositories: RepositoriesData
  error?: string
}

export interface RoutersData {
  routers: RouterInfo[]
  generated_at: number
}

export interface Client {
  client_id: string
  mount_point: string
  hostname: string
  writable: boolean
  version: string
  state: number  // 0=UNKNOWN 1=CONNECTED 2=STALE 3=DISCONNECTED
  connected_at: number   // Unix timestamp (seconds)
  last_heartbeat: number // Unix timestamp (seconds)
  operations_count: number
  bytes_read: number
  errors_count: number
}

export interface ListClientsResponse {
  clients: Client[]
}

export interface GuardianClient {
  client_id: string
  base_url: string
  last_heartbeat: number  // Unix timestamp (seconds)
  connected_sec: number
  state: string
  router: string
}

export interface ListGuardianClientsResponse {
  guardian_clients: GuardianClient[]
  count: number
}

export interface WhitelistEntry {
  client_id: string
  label: string
  added_at: number  // Unix timestamp (seconds)
}

export interface WhitelistData {
  enabled: boolean
  clients: WhitelistEntry[]
}

// FetcherClusterStats matches fetcher.ClusterStats from Go
export interface FetcherClusterStats {
  total_fetchers: number
  healthy_fetchers: number
  total_requests: number
  total_cache_hits: number
  total_cache_misses: number
  aggregated_hit_rate: number
  total_cache_size_bytes: number
  total_cache_entries: number
  total_active_fetches: number
  total_queued_prefetch: number
  total_bytes_fetched: number
  total_bytes_served: number
  client_affinity_hits: number
  client_affinity_misses: number
  client_total_requests: number
  sync_worker: SyncWorkerStatsInfo
  fetchers: FetcherInstanceStats[]
  blob_stats?: Record<string, BlobBackendSum>
  storage_blobs?: Record<string, BlobBackendSum>
}

// FetcherInstanceStats matches fetcher.FetcherStats from Go
export interface FetcherInstanceStats {
  address: string
  fetcher_id: string
  healthy: boolean
  uptime_seconds: number
  total_requests: number
  cache_hits: number
  cache_misses: number
  cache_hit_rate: number
  cache_size_bytes: number
  cache_entries: number
  active_fetches: number
  queued_prefetches: number
  bytes_fetched: number
  bytes_served: number
  sync_worker: SyncWorkerStatsInfo
  source_stats?: Record<string, SourceStatsInfo>
  error_count: number
  last_error?: string
}

export interface SyncWorkerStatsInfo {
  total_jobs: number
  active_jobs: number
  completed_jobs: number
  failed_jobs: number
  refresh_probes: number
  refresh_probe_failures: number
  git_cache_entries: number
  publish_jobs: number
  published_repositories: number
  staged_bundles: number
  staged_bundle_bytes: number
  worktree_bytes: number
  bundle_stage_failures: number
}

export interface BlobBackendSum {
  blob_count: number
  blob_bytes: number
}

export interface WorkspaceSyncSummary {
  repositories_total: number
  repositories_succeeded: number
  repositories_conflicted: number
  repositories_failed: number
  repositories_refreshed: number
  repositories_published: number
}

export interface WorkspaceSyncRepositoryResult {
  storage_id: string
  display_path: string
  repo_url: string
  branch: string
  base_commit: string
  remote_commit: string
  status: number
  message: string
  conflict_reason: string
  target_branch: string
  pushed_commit: string
}

export interface WorkspaceSyncJob {
  job_id: string
  workspace_id: string
  action: number
  state: number
  requested_by_client_id: string
  created_at_unix: number
  started_at_unix: number
  finished_at_unix: number
  summary?: WorkspaceSyncSummary
  repositories: WorkspaceSyncRepositoryResult[]
  error_message: string
  allow_fast_forward_only: boolean
  reject_if_local_changes: boolean
  bundle_id: string
  logical_commit_message: string
}

export interface WorkspaceSyncJobsResponse {
  jobs: WorkspaceSyncJob[]
}

export interface SourceStatsInfo {
  requests: number
  errors: number
  bytes_fetched: number
  avg_latency_ms: number
  cached_items: number
  cache_bytes: number
}

// Keep FetcherStats as alias for backwards compatibility
export type FetcherStats = FetcherClusterStats

export interface LogEngineData {
  enabled: boolean
  log_chunks: number
  metric_chunks: number
  trace_chunks: number
  nodes: LogEngineNode[]
}

export interface LogEngineNode {
  node_id: string
  address: string
  enabled: boolean
  log_chunks: number
  metric_chunks: number
  trace_chunks: number
}

// DependenciesData matches router.DependenciesData from Go
export interface DependenciesData {
  total_files: number
  ecosystems: number
  nodes_with_data: number
  ingested_at: number   // Unix timestamp (seconds)
  tools: DepsToolSummary[]
  nodes: DepsNodeInfo[]
}

export interface DepsToolSummary {
  tool: string    // e.g. 'go', 'npm', 'pip', 'cargo'
  files: number
}

export interface DepsNodeInfo {
  node_id: string
  files: number
}

export interface PredictorStats {
  nodes: PredictorNode[]
  total_predictions: number
  total_prefetches: number
  total_hits: number
  total_misses: number
  cluster_hit_rate: number
  enabled_nodes: number
  total_nodes: number
  total_chains: number
  total_dir_maps: number
}

export interface PredictorNode {
  node_id: string
  address: string
  enabled: boolean
  markov_chains: number
  directory_maps: number
  predictions: number
  prefetches: number
  prefetch_hits: number
  prefetch_misses: number
  hit_rate: number
  error?: string
}

export type PprofProfile = 'cpu' | 'heap' | 'goroutine' | 'allocs' | 'mutex' | 'block' | 'threadcreate' | 'trace'

export interface PprofCollectRequest {
  profiles: PprofProfile[]
  cpu_duration_seconds: number
}

export interface SearchIndex {
  storage_id: string
  display_path: string
  status: number  // IndexStatus enum: 0=UNKNOWN 1=PENDING 2=INDEXING 3=READY 4=ERROR
  files_count: number
  index_size_bytes: number
  last_indexed: string
  error_message: string
  progress: number
}

export interface SearchIndexesResponse {
  indexes: SearchIndex[]
}

export interface SearchStatsData {
  total_indexes: number
  total_files_indexed: number
  total_index_size_bytes: number
  searches_total: number
  avg_search_duration_ms: number
  queue_length: number
  active_jobs: number
  jobs_queued: number
  jobs_completed: number
  jobs_failed: number
  jobs_rejected?: number
  uptime: string
}

export interface SearchNode {
  address: string
  indexes: number
  files: number
  searches: number
}

export interface SearchResult {
  repo_id: string
  repo_url: string
  file: string
  line: number
  column: number
  content: string
  score: number
}

export interface SearchResponse {
  results: SearchResult[]
  total: number
  duration_ms: number
}

export interface FileContentRequest {
  storage_id: string
  file_path: string
}

export interface FileContentResponse {
  content: string
  language: string
}

export interface GuardianPartition {
  name: string
  files: number
  size: number
  created_at: string
}

export interface GuardianPartitionsResponse {
  partitions: GuardianPartition[]
}

export interface IngestRequest {
  source: string
  ref?: string
  source_id?: string
  ingestion_type?: string
  fetch_type?: string
  replicate_data?: boolean
}

export interface IngestResponse {
  success: boolean
  message: string
  status?: string
}

export interface RebalanceResponse {
  success: boolean
  message: string
}

export interface RebuildResponse {
  success: boolean
  message: string
}

export interface ToggleWhitelistRequest {
  enabled: boolean
}

export interface AddWhitelistRequest {
  client_id: string
}

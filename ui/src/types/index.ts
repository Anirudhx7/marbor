export interface LoginResponse {
  // No longer sent - the session lives in an httpOnly cookie the server sets
  // directly. Kept optional only for the demo-mode mock login response.
  token?: string;
  role: string;
  username: string;
  must_change_password: boolean;
  expires_at: string;
}

export interface SessionData {
  role: string;
  username: string;
  mustChangePassword: boolean;
}

export interface UserRecord {
  id: number;
  username: string;
  email: string;
  role: 'admin' | 'user';
  status: 'pending' | 'active' | 'suspended';
  api_key_name: string;
  must_change_password: boolean;
  created_at: string;
  approved_at?: string;
  approved_by?: string;
}

export interface PredictiveDecision {
  timestamp: string;
  predicted_model: string;
  trigger_model: string;
  node: string;
  was_already_warm: boolean;
  warmup_triggered: boolean;
  transition_count: number;
  hour: number;
}

export interface GPUNode {
  id: string;
  name: string;
  host: string;
  gpuModel: string;
  port: number;
  vramTotalMB: number;
  vramUsedMB: number;
  // How VRAM figures were obtained, so the UI never presents a guess as a
  // measurement: nvidia = live local nvidia-smi; agent = a remote Node Agent
  // (any vendor - see agentGpuVendor below for which tool); api = summed from
  // the node's own /api/ps (real, total unknown); declared = total from
  // config; none = no data.
  vramSource: 'nvidia' | 'agent' | 'api' | 'declared' | 'none';
  powerDrawW: number;
  temperature: number | null;
  health: 'healthy' | 'degraded' | 'down';
  runtime: string;
  draining: boolean;
  // Why draining was set (e.g. "manual", "thermal", "scheduled") - persisted
  // alongside draining, empty when not draining.
  drainedReason?: string;
  // Live count of in-flight requests on this node. Drain does not kill
  // in-flight requests, only stops routing new ones - this is what lets an
  // operator see when a draining node has actually finished flushing.
  activeConns: number;
  // Live, admin-toggleable, in-memory-only. Never persisted - reverts to
  // false (prewarm enabled) on restart.
  prewarmDisabled?: boolean;
  uptime: string;
  loadedModels: LoadedModel[];
  // Last warmup-ping failure per model (model name -> error string) - only
  // present for models that failed to warm; a model that warmed successfully
  // or was never attempted has no key here. Lets the UI explain why a
  // keep-warm model is stuck instead of leaving it silently "not resident"
  // forever.
  warmupErrors?: Record<string, string>;
  // Last failed scheduled/agent unload per model (model name -> error
  // string) - mirrors warmupErrors for the unload side, so a schedule that
  // dispatched successfully but whose actual unload failed is still
  // diagnosable instead of only ever appearing in the mesh's own logs.
  unloadErrors?: Record<string, string>;
  healthHistory: number[];
  // Real in-flight warmup VRAM reservation (never a separate estimate) from
  // the same accounting used for headroom checks. 0 = nothing pending.
  pendingPrewarmMB?: number;
  coldStarts?: number;
  tokensTotal?: number;
  avgLatencyMs?: number;
  warmHitRatio?: number;
  // Node Agent-derived telemetry (internal/nodeagent). agentPresent is false
  // whenever no agent is configured for this node, or the most recent agent
  // poll failed - the UI must check agentPresent before displaying
  // cpuPercent/fanPercent/ramUsedMB/diskFreeGB/agentVersion as real
  // measurements (R1), rendering '-' instead.
  agentPresent?: boolean;
  agentVersion?: string;
  fanPercent?: number | null;
  cpuPercent?: number;
  ramUsedMB?: number;
  diskFreeGB?: number;
  // Agent self-reported metadata (capabilities/platform/architecture/GPU
  // vendor/detected runtime) - lets the UI gate agent-dependent features on
  // what this node's agent build actually supports, and helps debug a
  // mixed-version fleet. Same agentPresent gating as the fields above.
  agentCapabilities?: string[];
  agentPlatform?: string;
  agentArchitecture?: string;
  agentGpuVendor?: string;
  agentRuntime?: string;
  // agentNodeId is the agent's self-persisted stable identity (survives
  // agent upgrades/hostname changes) - shown for fleet debugging, not yet
  // used to re-identify a node across a URL change.
  agentNodeId?: string;
  // agentGpuCount/agentGpus are the full multi-GPU array from the agent's
  // gpu resource - agentGpus holds one entry per physical device so the UI
  // can render a per-GPU breakdown instead of only the single aggregate
  // vramTotalMB/vramUsedMB/temperature/powerDrawW/fanPercent fields above.
  agentGpuCount?: number;
  agentGpus?: AgentGPUDevice[];
  driverVersion?: string;
  cudaVersion?: string;
  // Host capacity/identity - same agentPresent gating as the fields above.
  ramTotalMB?: number;
  diskTotalGB?: number;
  hostname?: string;
  uptimeSeconds?: number;
  bootTime?: number;
  // The detected runtime's own reported version and live reachability -
  // distinct from agentRuntime (just the runtime name, already above).
  runtimeVersion?: string;
  runtimeStatus?: string;
  // localModels lists models already downloaded on this node (not just
  // currently loaded - loadedModels above covers that), via the agent's
  // "models.list" capability. Fetched separately (getNodeModels), not part
  // of the bulk node list - only present here so demo mode has a static
  // place to put mock data alongside the real per-node fetch.
  localModels?: LocalModel[];
}

export interface LocalModel {
  name: string;
  sizeBytes?: number;
  source: string;
  family?: string;
}

// AgentGPUDevice mirrors internal/nodeagent.GPUInfo - one physical GPU
// device from the agent's multi-GPU array.
export interface AgentGPUDevice {
  index: number;
  vendor?: string;
  corePercent?: number | null;
  temperatureC?: number | null;
  fanPercent?: number | null;
  powerWatts?: number | null;
  vramUsedMB?: number;
  vramTotalMB?: number;
}

export interface LoadedModel {
  name: string;
  sizeVram: number;
}

// BenchmarkRun mirrors internal/store.BenchmarkRun - a persisted result from
// the in-dashboard hardware benchmark (Settings -> Benchmark, hidden route
// /benchmark). All *Ms fields are real measured milliseconds, never
// estimated (R1).
export interface BenchmarkRun {
  id: number;
  node: string;
  model: string;
  n: number;
  cold_p50_ms: number;
  cold_min_ms: number;
  cold_max_ms: number;
  warm_p50_ms: number;
  warm_min_ms: number;
  warm_max_ms: number;
  speedup_x: number;
  created_at: string;
}

export interface LiveRequest {
  id: string;
  apiKey: string;
  model: string;
  routedTo: string;
  status: 'warm' | 'loading';
  latency: number;
  tokens: number;
  tokensPerSec: number;
  timestamp: string | Date;
}

export interface APIKey {
  id: string;
  name: string;
  key: string;
  created: string;
  requestsToday: number;
  requestsThisMonth: number;
  tokensThisMonth: number;
  estimatedCostUsd: number;
  rateLimit: number;
  dailyLimit?: number;
  monthlyLimit?: number;
  dailyUsdCap?: number;
  monthlyUsdCap?: number;
  status: 'active' | 'suspended' | 'rate-limited' | 'revoked' | 'expired';
  allowedModels: string[];
  models?: string[];
  expiresAt: string | null;
}

export interface RoutingRule {
  id: string;
  priority: number;
  condition: string;
  targetNode: string;
  strategy: 'warm-first' | 'round-robin' | 'least-conn';
  enabled: boolean;
}

export interface MetricData {
  timestamp: string;
  value: number;
}

export interface TokenUsageData {
  keyName: string;
  tokens: number;
}

export interface NodeLatencyData {
  nodeName: string;
  latency: number;
}

export interface RequestDistributionData {
  nodeName: string;
  requests: number;
}

export interface Settings {
  proxyPort: number;
  authMode: 'api-key' | 'no-auth';
  liteLLMEnabled: boolean;
  liteLLMEndpoint: string;
  liteLLMApiKey: string;
  pollingInterval: number;
  prometheusEnabled: boolean;
  prometheusPort: number;
  logLevel: 'debug' | 'info' | 'warn' | 'error';
  timezone: string;
  cloudDailyUsdCap: number;
  cloudMonthlyUsdCap: number;
  cloudSoftBudgetPct: number;
  hideDemoBanner?: boolean;
  hideBudgetBanner?: boolean;
  huggingFaceToken?: string;
  allowManagementEndpoints?: boolean;

  // Admin & Security - no config.yaml anymore, so these need a UI home too
  // (2026-07 config.yaml elimination).
  adminBindAddress: string;
  adminCorsOrigin: string;
  proxyAccessLog: boolean;
  proxyTrustProxyHeaders: boolean;

  // Advanced routing knobs.
  routingFallback: string;
  routingUpstreamTimeoutMs: number;
  routingMaxRetries: number;
  routingSessionAffinity: boolean;
  routingSessionAffinityTtl: string;
  routingNvidiaPollIntervalMs: number;
  routingQueueMaxDepth: number;
  routingQueueTimeoutMs: number;
  routingHealthFailureThreshold: number;
  routingHealthSuccessThreshold: number;
  routingOverflowSlaMs: number;
  thermalWatchdogEnabled: boolean;
  thermalWatchdogMaxTempCelsius: number;
  thermalWatchdogConsecutiveBreaches: number;

  // Docker auto-discovery.
  dockerEnabled: boolean;
  dockerSocket: string;
  dockerPollIntervalMs: number;

  // Audit log, webhooks, savings rate.
  auditEnabled: boolean;
  auditRetentionDays: number;
  systemAuditRetentionDays: number;
  webhookEnabled: boolean;
  webhookUrl: string;
  webhookSecret: string;
  savingsReferenceCostPer1k: number;

  // Global warmup (distinct from the per-node toggle on the Warmup page).
  warmupEnabled: boolean;
  warmupIntervalMs: number;
  warmupKeepAlive: string;

  // Model name -> max context window in tokens, for admission-time checks.
  contextWindows: Record<string, number>;
}

export interface SystemAuditEntry {
  time: string;
  username: string;
  action: string;
  target: string;
  details: string;
  source_ip: string;
}

export interface Savings {
  local_requests: number;
  cloud_requests: number;
  // null = requests happened but no real token counts could be parsed yet
  cloud_spent_usd: number | null;
  saved_usd: number | null;
  total_requests: number;
}

export interface ModelNode {
  name: string;
  healthy: boolean;
}

export interface ModelEntry {
  name: string;
  size_vram: number;
  // size_disk is the model's on-disk size in bytes (from Ollama /api/tags or
  // the Node Agent's models.list capability). Absent when neither source has
  // reported it yet for this model.
  size_disk?: number;
  nodes: ModelNode[];
  warm_count: number;
  total_nodes: number;
  family?: string;
}

export interface ModelCatalog {
  models: ModelEntry[];
  total_models: number;
  total_nodes: number;
  healthy_nodes: number;
}

export interface CloudProvider {
  name: string;
  provider: string;
  base_url: string;
  default_model: string;
  cost_per_1k_tokens: number;
  enabled: boolean;
  priority: number;
}

// CloudProviderInput is the add/edit payload - includes api_key (masked as
// "***" when read back from an existing provider; omit/leave masked to keep
// the stored key unchanged).
export interface CloudProviderInput {
  name: string;
  provider: string;
  base_url: string;
  api_key: string;
  default_model: string;
  cost_per_1k_tokens: number;
  enabled: boolean;
  priority: number;
}

export interface RequestEntry {
  id: string;
  time: string;
  key_name: string;
  source_ip?: string;
  model: string;
  node: string;
  status: number;
  latency_ms: number;
  cloud: boolean;
}

export interface HourlyBucket {
  hour: string;
  local: number;
  cloud: number;
  saved_usd: number;
  spent_usd: number;
}

export interface ModelStat {
  model: string;
  local: number;
  cloud: number;
  saved_usd: number;
}

export interface Analytics {
  local_requests: number;
  cloud_requests: number;
  // null = requests happened but no real token counts could be parsed yet
  total_saved_usd: number | null;
  total_spent_usd: number | null;
  hourly: HourlyBucket[];
  by_model: ModelStat[];
}

export type FitStatus = 'green' | 'yellow' | 'red' | 'unknown';

export interface ModelFit {
  name: string;
  size_bytes: number;
  vram_estimate_bytes: number;
  fit: FitStatus;
  loaded: boolean;
}

export interface NodeFit {
  name: string;
  url: string;
  vram_free_bytes: number;
  vram_total_bytes: number;
  vram_source: 'nvidia-smi' | 'rocm-smi' | 'xpu-smi' | 'system_profiler' | 'agent' | 'inferred' | 'unknown' | 'declared';
  models: ModelFit[];
}

export interface ModelFitResponse {
  nodes: NodeFit[];
}

export interface ModelVariant {
  tag: string;
  quantization: string;
  vram_est_mb: number;
  size_mb: number;
  recommended: boolean;
}

export interface CatalogModel {
  name: string;
  display_name: string;
  description: string;
  param_count: string;
  categories: string[];
  variants: ModelVariant[];
  popular: boolean;
  rank: number;
}

// A variant decorated with its per-node fit classification.
export interface CatalogVariantFit extends ModelVariant {
  fit: FitStatus;
}

// A catalog model decorated for a specific node.
export interface CatalogModelFit extends CatalogModel {
  variants: CatalogVariantFit[];
  downloaded: boolean;
}

export interface CatalogNodeEntry {
  name: string;
  url: string;
  runtime?: string;
  vram_free_bytes: number;
  vram_total_bytes: number;
  vram_used_bytes?: number;
  vram_source: 'nvidia-smi' | 'rocm-smi' | 'xpu-smi' | 'system_profiler' | 'agent' | 'inferred' | 'unknown' | 'declared';
  models: CatalogModelFit[];
}

export interface ModelCatalogResponse {
  catalog: CatalogModel[];
  nodes: CatalogNodeEntry[];
}

export interface BudgetEntry {
  name?: string;
  dailySpent: number;
  dailyCap: number;
  dailyPct: number;
  monthlySpent: number;
  monthlyCap: number;
  monthlyPct: number;
}

// ModelConfig is an operator-declared default parameter profile for a
// specific (model, node) pair, applied whenever ollama-mesh routes to that
// model on that node. The same model name can be resident on multiple nodes
// with different runtimes (ollama/vllm/tgi/llamacpp/mlx) or VRAM budgets, so a
// profile is only ever meaningful scoped to one node - `model` and `node`
// are both required. Every other field is optional - unset means "inherit
// the backend's own default", never a fabricated value (R1). Field
// names/JSON keys mirror internal/store/store.go 1:1. Verified 2026-07
// against each runtime's actual current source/API schema (Ollama's
// api/types.go, llama.cpp's server README, vLLM's OpenAI protocol source,
// TGI's live OpenAPI spec) - flash_attention, offload_kv_cache_to_gpu,
// rope_frequency_base/scale, use_mlock, tensor_parallelism, mirostat*, and
// tfs_z were removed: none are real per-request parameters on any of the
// four runtimes (some never existed as request fields at all; others were
// pruned from Ollama/llama.cpp's own current option set).
export interface ModelConfig {
  model: string;
  node: string;

  // Load-time / engine parameters - Ollama only.
  num_ctx?: number;
  num_gpu?: number;
  main_gpu?: number;
  num_batch?: number;
  num_thread?: number;
  use_mmap?: boolean;
  draft_num_predict?: number;
  ttl?: number;

  // Inference-time / sampling parameters.
  temperature?: number;
  top_p?: number;
  top_k?: number;
  min_p?: number;
  typical_p?: number;
  num_keep?: number;
  max_tokens?: number;
  seed?: number;
  stop?: string[];
  repeat_penalty?: number;
  repeat_last_n?: number;
  presence_penalty?: number;
  frequency_penalty?: number;
  mirostat?: number;
  mirostat_tau?: number;
  mirostat_eta?: number;
  logit_bias?: Record<string, number>;
  response_format?: string;

  // llama.cpp-only sampling extras.
  dry_multiplier?: number;
  dry_base?: number;
  dry_allowed_length?: number;
  dry_penalty_last_n?: number;
  xtc_probability?: number;
  xtc_threshold?: number;
  n_probs?: number;
  min_keep?: number;

  // vLLM-only sampling extras. ignore_eos is shared with llama.cpp (same
  // wire name/meaning on both).
  length_penalty?: number;
  stop_token_ids?: number[];
  include_stop_str_in_output?: boolean;
  ignore_eos?: boolean;
  min_tokens?: number;
  skip_special_tokens?: boolean;
  truncate_prompt_tokens?: number;

  // Meta / orchestration.
  system?: string;
  template?: string;
  rpm?: number;
  tpm?: number;
}

export interface CloudBudgetStatus {
  softBudgetPct: number;
  global: BudgetEntry;
  perKey: BudgetEntry[];
}

export interface LoginResponse {
  token: string;
  role: string;
  username: string;
  must_change_password: boolean;
  expires_at: string;
}

export interface SessionData {
  token: string;
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
  gpuModel: string;
  port: number;
  vramTotalMB: number;
  vramUsedMB: number;
  // How VRAM figures were obtained, so the UI never presents a guess as a
  // measurement: nvidia = live local nvidia-smi; api = summed from the node's own
  // /api/ps (real, total unknown); declared = total from config; none = no data.
  vramSource: 'nvidia' | 'api' | 'declared' | 'none';
  powerDrawW: number;
  cpuPercent: number;
  temperature: number | null;
  health: 'healthy' | 'degraded' | 'down';
  runtime: string;
  draining: boolean;
  // Live, admin-toggleable, in-memory-only. Never persisted - reverts to
  // false (prewarm enabled) on restart.
  prewarmDisabled?: boolean;
  uptime: string;
  loadedModels: LoadedModel[];
  healthHistory: number[];
  // Real in-flight warmup VRAM reservation (never a separate estimate) from
  // the same accounting used for headroom checks. 0 = nothing pending.
  pendingPrewarmMB?: number;
}

export interface LoadedModel {
  name: string;
  sizeVram: number;
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
  status: 'active' | 'suspended' | 'rate-limited';
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
  pollingInterval: number;
  prometheusEnabled: boolean;
  prometheusPort: number;
  logLevel: 'debug' | 'info' | 'warn' | 'error';
  timezone: string;
  cloudDailyUsdCap: number;
  cloudMonthlyUsdCap: number;
  cloudSoftBudgetPct: number;
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
  nodes: ModelNode[];
  warm_count: number;
  total_nodes: number;
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
  vram_source: 'nvidia-smi' | 'inferred' | 'unknown' | 'declared';
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
  vram_source: 'nvidia-smi' | 'inferred' | 'unknown' | 'declared';
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

export interface CloudBudgetStatus {
  softBudgetPct: number;
  global: BudgetEntry;
  perKey: BudgetEntry[];
}

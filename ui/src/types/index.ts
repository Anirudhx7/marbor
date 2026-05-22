export interface GPUNode {
  id: string;
  name: string;
  gpuModel: string;
  port: number;
  vramTotalMB: number;
  vramUsedMB: number;
  powerDrawW: number;
  cpuPercent: number;
  temperature: number | null;
  health: 'healthy' | 'degraded' | 'down';
  uptime: string;
  loadedModels: LoadedModel[];
  healthHistory: number[];
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
  timestamp: string | Date;
}

export interface APIKey {
  id: string;
  name: string;
  key: string;
  created: string;
  requestsToday: number;
  requestsThisMonth: number;
  rateLimit: number;
  status: 'active' | 'suspended' | 'rate-limited';
  allowedModels: string[];
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
}

export interface Savings {
  local_requests: number;
  cloud_requests: number;
  cloud_spent_usd: number;
  saved_usd: number;
  total_requests: number;
}

export interface CloudProvider {
  name: string;
  provider: string;
  base_url: string;
  default_model: string;
  cost_per_1k_tokens: number;
  enabled: boolean;
}

import { GPUNode, APIKey, Settings, Savings, CloudProvider, ModelCatalog, RequestEntry, Analytics, ModelCatalogResponse, ModelConfig, BenchmarkRun } from '../types';
import type { SystemInfo } from './api';

const GB = 1024;
const GiB = 1024 * 1024 * 1024;

// mockRuntimeLogLines is the static sample shown by the "View Logs" panel
// in demo mode (P58) - plausible, not a real capture, matching how other
// demo surfaces show static representative data rather than a live call.
export const mockRuntimeLogLines: string[] = [
  'Aug 01 00:12:03 gpu-node-01 systemd[1]: Started Ollama Service.',
  'time=2026-08-01T00:12:03.114Z level=INFO source=routes.go:1288 msg="Listening on 127.0.0.1:11434 (version 0.6.5)"',
  'time=2026-08-01T00:12:03.201Z level=INFO source=gpu.go:217 msg="detected GPU" library=cuda variant=v12 compute=8.9 driver=12.4 name="NVIDIA RTX 4090" total="24.0 GiB"',
  'time=2026-08-01T00:14:41.882Z level=INFO source=server.go:113 msg="model loaded" model=llama3:70b duration=8.9s',
  'time=2026-08-01T00:22:07.005Z level=INFO source=sched.go:406 msg="model unloaded due to idle timeout" model=llama3:70b',
];

export const mockGPUNodes: GPUNode[] = [
  {
    id: 'node-1',
    name: 'gpu-node-01',
    host: '10.0.0.11',
    gpuModel: 'NVIDIA A100 80GB',
    port: 11434,
    runtime: 'ollama',
    vramTotalMB: 80 * GB,
    vramUsedMB: Math.round(67.2 * GB),
    vramSource: 'nvidia',
    powerDrawW: 312,
    cpuPercent: 34,
    temperature: 72,
    health: 'healthy',
    draining: false,
    activeConns: 3,
    prewarmDisabled: false,
    pendingPrewarmMB: 0,
    uptime: '14d 6h',
    loadedModels: [
      { name: 'llama3.2', sizeVram: Math.round(2.2 * GiB) },
      { name: 'qwen2.5', sizeVram: Math.round(4.8 * GiB) },
    ],
    healthHistory: Array(60).fill(0).map(() => 95 + Math.random() * 5),
    coldStarts: 2,
    tokensTotal: 245000,
    avgLatencyMs: 120,
    warmHitRatio: 0.98,
    // Node Agent installed on this node (demo parity - all demo nodes run
    // the agent so the fleet view shows full telemetry everywhere). This is
    // the demo's one multi-GPU node, showing the full Node Agent Protocol
    // v1 envelope (agentGpus array, host capacity/identity, runtime
    // version/status, node_id).
    agentPresent: true,
    agentVersion: '0.17.0',
    fanPercent: 62,
    ramUsedMB: Math.round(41.5 * GB),
    diskFreeGB: 812.4,
    agentCapabilities: ['status', 'models.pull', 'models.list', 'models.delete', 'models.unload', 'runtime.health_check'],
    localModels: [
      { name: 'llama3.2:latest', sizeBytes: Math.round(2.2 * GiB), source: 'ollama-tags' },
      { name: 'qwen2.5:14b', sizeBytes: Math.round(4.8 * GiB), source: 'ollama-tags' },
      { name: 'mistral:7b', sizeBytes: Math.round(4.1 * GiB), source: 'ollama-tags' },
    ],
    agentPlatform: 'linux',
    agentArchitecture: 'amd64',
    agentGpuVendor: 'nvidia',
    agentRuntime: 'ollama',
    agentNodeId: 'a1b2c3d4-5e6f-4a1b-8c2d-000000000001',
    agentGpuCount: 2,
    agentGpus: [
      { index: 0, vendor: 'nvidia', corePercent: 78, temperatureC: 72, fanPercent: 62, powerWatts: 312, vramUsedMB: Math.round(67.2 * GB), vramTotalMB: 80 * GB },
      { index: 1, vendor: 'nvidia', corePercent: 15, temperatureC: 48, fanPercent: 40, powerWatts: 95, vramUsedMB: Math.round(8 * GB), vramTotalMB: 80 * GB },
    ],
    driverVersion: '535.183.01',
    cudaVersion: '12.2',
    ramTotalMB: 128 * GB,
    diskTotalGB: 2000,
    hostname: 'gpu-node-01',
    uptimeSeconds: 14 * 86400 + 6 * 3600,
    runtimeVersion: '0.6.5',
    runtimeStatus: 'up',
    // Demo parity for the "SUPPRESSED" badge (GPUNodes.tsx) and the Warmup
    // page's per-model suppression line - a scheduled unload took this
    // keep-warm model cold, and it stays that way until an explicit warmup
    // re-arms it, distinct from a warmupErrors failure.
    warmupState: [
      { model: 'qwen2.5:7b', state: 'suppressed', reason: 'scheduled_unload', since: new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString() },
    ],
  },
  {
    id: 'node-2',
    name: 'gpu-node-02',
    // Same host as node-1 (10.0.0.11) on purpose - demonstrates the
    // multi-runtime-per-host Node Agent fix: one physical box running both
    // Ollama (:11434, node-1) and vLLM (:8000, here), sharing one agent
    // process/enrollment, both showing agentPresent: true.
    host: '10.0.0.11',
    gpuModel: 'NVIDIA A100 80GB',
    port: 8000,
    runtime: 'vllm',
    vramTotalMB: 80 * GB,
    vramUsedMB: Math.round(52.8 * GB),
    vramSource: 'nvidia',
    powerDrawW: 274,
    cpuPercent: 41,
    temperature: 68,
    health: 'healthy',
    draining: false,
    activeConns: 1,
    prewarmDisabled: false,
    pendingPrewarmMB: 8192,
    uptime: '12d 14h',
    loadedModels: [
      { name: 'meta-llama/Llama-3.3-8B-Instruct', sizeVram: Math.round(16.0 * GiB) },
    ],
    healthHistory: Array(60).fill(0).map(() => 92 + Math.random() * 8),
    coldStarts: 0,
    tokensTotal: 890000,
    avgLatencyMs: 85,
    warmHitRatio: 1.00,
    fanPercent: 55,
    ramUsedMB: Math.round(28.3 * GB),
    diskFreeGB: 1204.7,
    agentPresent: true,
    agentVersion: '0.1.0',
    agentCapabilities: ['status'],
    agentPlatform: 'linux',
    agentArchitecture: 'amd64',
    agentGpuVendor: 'nvidia',
    agentRuntime: 'vllm',
  },
  {
    id: 'node-3',
    name: 'gpu-node-03',
    host: '10.0.0.13',
    gpuModel: 'NVIDIA RTX 4090 24GB',
    port: 8080,
    runtime: 'tgi',
    vramTotalMB: 24 * GB,
    vramUsedMB: Math.round(18.5 * GB),
    vramSource: 'nvidia',
    powerDrawW: 185,
    cpuPercent: 27,
    temperature: 78,
    health: 'healthy',
    draining: false,
    activeConns: 0,
    prewarmDisabled: true,
    pendingPrewarmMB: 0,
    uptime: '8d 2h',
    loadedModels: [
      { name: 'mistralai/Mistral-Small-24B-Instruct-2501', sizeVram: Math.round(14.8 * GiB) },
    ],
    healthHistory: Array(60).fill(0).map(() => 88 + Math.random() * 10),
    coldStarts: 5,
    tokensTotal: 1250000,
    avgLatencyMs: 210,
    warmHitRatio: 0.88,
    fanPercent: 71,
    ramUsedMB: Math.round(16.8 * GB),
    diskFreeGB: 340.2,
    agentPresent: true,
    agentVersion: '0.1.0',
    agentCapabilities: ['status'],
    agentPlatform: 'linux',
    agentArchitecture: 'amd64',
    agentGpuVendor: 'nvidia',
    agentRuntime: 'tgi',
    // Demo parity for the GPU Nodes "WARMUP FAILED"/"UNLOAD FAILED" badges
    // (see NodeCard in GPUNodes.tsx) - a model repeatedly failing its
    // keep-warm ping, and a separate scheduled unload that failed against
    // this node, both diagnosable from the dashboard instead of only ever
    // appearing in the mesh's own logs.
    warmupErrors: { 'mistralai/Mistral-Small-24B-Instruct-2501': 'node gpu-node-03 unhealthy' },
    unloadErrors: { 'codellama:13b': 'agent unreachable: dial tcp: connect: connection refused' },
  },
  {
    id: 'node-4',
    name: 'gpu-node-04',
    host: '10.0.0.14',
    gpuModel: 'NVIDIA RTX 3090 24GB',
    port: 8080,
    runtime: 'llamacpp',
    vramTotalMB: 24 * GB,
    vramUsedMB: Math.round(4.2 * GB),
    vramSource: 'nvidia',
    powerDrawW: 92,
    cpuPercent: 18,
    temperature: 45,
    health: 'degraded',
    draining: true,
    drainedReason: 'manual',
    activeConns: 2,
    prewarmDisabled: false,
    pendingPrewarmMB: 0,
    uptime: '3d 8h',
    loadedModels: [
      { name: 'llama-3.2-3b-instruct.Q4_K_M.gguf', sizeVram: Math.round(4.2 * GiB) },
    ],
    healthHistory: Array(60).fill(0).map(() => 70 + Math.random() * 25),
    coldStarts: 12,
    tokensTotal: 12000,
    avgLatencyMs: 450,
    warmHitRatio: 0.55,
    fanPercent: 89,
    ramUsedMB: Math.round(9.6 * GB),
    diskFreeGB: 78.9,
    agentPresent: true,
    agentVersion: '0.1.0',
    agentCapabilities: ['status'],
    agentPlatform: 'linux',
    agentArchitecture: 'amd64',
    agentGpuVendor: 'nvidia',
    agentRuntime: 'llamacpp',
  },
  {
    id: 'node-5',
    name: 'gpu-node-05',
    host: '10.0.0.15',
    gpuModel: 'Apple M3 Max 128GB',
    port: 8080,
    runtime: 'mlx',
    vramTotalMB: 128 * GB,
    vramUsedMB: Math.round(22.4 * GB),
    vramSource: 'declared',
    powerDrawW: 0,
    cpuPercent: 22,
    temperature: null,
    health: 'healthy',
    draining: false,
    activeConns: 0,
    prewarmDisabled: false,
    pendingPrewarmMB: 0,
    uptime: '5d 19h',
    loadedModels: [
      { name: 'mlx-community/Llama-3.2-3B-Instruct-4bit', sizeVram: Math.round(2.0 * GiB) },
    ],
    healthHistory: Array(60).fill(0).map(() => 96 + Math.random() * 4),
    coldStarts: 1,
    tokensTotal: 58000,
    avgLatencyMs: 96,
    warmHitRatio: 0.97,
    fanPercent: null,
    ramUsedMB: Math.round(38.6 * GB),
    diskFreeGB: 512.5,
    agentPresent: true,
    agentVersion: '0.1.0',
    agentCapabilities: ['status'],
    agentPlatform: 'darwin',
    agentArchitecture: 'arm64',
    agentGpuVendor: 'apple',
    agentRuntime: 'mlx',
  },
];

export const mockAPIKeys: APIKey[] = [
  {
    id: 'key-1',
    name: 'Engineering Team',
    key: 'om-eng-7a3f-b7c2-d4e5-f8g9',
    created: '2026-01-15',
    requestsToday: 3124,
    requestsThisMonth: 142380,
    tokensThisMonth: 284760000,
    estimatedCostUsd: 1423.80,
    rateLimit: 10000,
    dailyUsdCap: 50,
    monthlyUsdCap: 1000,
    status: 'active',
    allowedModels: ['all'],
    expiresAt: null,
  },
  {
    id: 'key-2',
    name: 'Data Platform',
    key: 'om-data-9h2i-j4k5-l6m7-n8o9',
    created: '2026-02-01',
    requestsToday: 1287,
    requestsThisMonth: 89450,
    tokensThisMonth: 178900000,
    estimatedCostUsd: 894.50,
    rateLimit: 5000,
    dailyUsdCap: 40,
    monthlyUsdCap: 750,
    status: 'active',
    allowedModels: ['qwen2.5:14b', 'llama3.3:70b', 'deepseek-r1:7b'],
    expiresAt: null,
  },
  {
    id: 'key-3',
    name: 'Support Bot',
    key: 'om-sup-1p2q-r3s4-t5u6-v7w8',
    created: '2026-02-14',
    requestsToday: 892,
    requestsThisMonth: 62340,
    tokensThisMonth: 62340000,
    estimatedCostUsd: 311.70,
    rateLimit: 3000,
    dailyUsdCap: 15,
    monthlyUsdCap: 350,
    status: 'active',
    allowedModels: ['llama3.3:8b', 'deepseek-r1:7b'],
    expiresAt: null,
  },
  {
    id: 'key-4',
    name: 'CI/CD Pipeline',
    key: 'om-ci-8x9y-z1a2-b3c4-d5e6',
    created: '2026-01-20',
    requestsToday: 412,
    requestsThisMonth: 28160,
    tokensThisMonth: 28160000,
    estimatedCostUsd: 140.80,
    rateLimit: 2000,
    dailyUsdCap: 0,
    monthlyUsdCap: 200,
    status: 'active',
    allowedModels: ['qwen2.5-coder:14b', 'llama3.3:8b'],
    expiresAt: null,
  },
  {
    id: 'key-5',
    name: 'Legal & Compliance',
    key: 'om-leg-4f5g-h6i7-j8k9-l0m1',
    created: '2026-03-01',
    requestsToday: 234,
    requestsThisMonth: 15840,
    tokensThisMonth: 15840000,
    estimatedCostUsd: 79.20,
    rateLimit: 1000,
    dailyUsdCap: 5,
    monthlyUsdCap: 100,
    status: 'active',
    allowedModels: ['qwen2.5:14b', 'llama3.3:8b'],
    expiresAt: null,
  },
  {
    id: 'key-6',
    name: 'External Partner',
    key: 'om-ext-2k3l-m4n5-o6p7-q8r9',
    created: '2026-03-10',
    requestsToday: 0,
    requestsThisMonth: 4397,
    tokensThisMonth: 4397000,
    estimatedCostUsd: 21.99,
    rateLimit: 500,
    dailyUsdCap: 0,
    monthlyUsdCap: 0,
    status: 'suspended',
    allowedModels: ['llama3.3:8b'],
    expiresAt: null,
  },
];

export const defaultSettings: Settings = {
  proxyPort: 11434,
  authMode: 'api-key',
  liteLLMEnabled: false,
  liteLLMEndpoint: 'http://localhost:4000',
  liteLLMApiKey: '',
  pollingInterval: 2000,
  prometheusEnabled: true,
  prometheusPort: 9090,
  logLevel: 'info',
  timezone: 'Local',
  cloudDailyUsdCap: 25,
  cloudMonthlyUsdCap: 500,
  cloudSoftBudgetPct: 0.8,
  hideDemoBanner: false,
  hideBudgetBanner: false,
  huggingFaceToken: '',
  allowManagementEndpoints: false,

  adminBindAddress: ':8080',
  adminCorsOrigin: '',
  proxyAccessLog: true,
  proxyTrustProxyHeaders: false,

  routingFallback: 'least-connections',
  routingUpstreamTimeoutMs: 120000,
  routingMaxRetries: 2,
  routingSessionAffinity: false,
  routingSessionAffinityTtl: '10m',
  routingNvidiaPollIntervalMs: 30000,
  routingQueueMaxDepth: 100,
  routingQueueTimeoutMs: 30000,
  routingHealthFailureThreshold: 3,
  routingHealthSuccessThreshold: 2,
  routingOverflowSlaMs: 0,
  thermalWatchdogEnabled: false,
  thermalWatchdogMaxTempCelsius: 0,
  thermalWatchdogConsecutiveBreaches: 3,

  dockerEnabled: false,
  dockerSocket: '',
  dockerPollIntervalMs: 30000,

  auditEnabled: false,
  auditRetentionDays: 30,
  systemAuditRetentionDays: 0,
  webhookEnabled: false,
  webhookUrl: '',
  webhookSecret: '',
  savingsReferenceCostPer1k: 0.002,

  warmupEnabled: false,
  warmupIntervalMs: 300000,
  warmupKeepAlive: '10m',

  contextWindows: {},

  backupEnabled: true,
  backupIntervalHours: 24,
  backupRetentionCount: 7,
  backupTargetDir: '/backups',
  backupLastAt: new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString(),
  backupLastError: '',
};

export const mockSavings: Savings = {
  local_requests: 328547,
  cloud_requests: 13920,
  cloud_spent_usd: 248.63,
  saved_usd: 4724.18,
  total_requests: 342467,
};

export const mockModelCatalog: ModelCatalog = {
  total_models: 6,
  total_nodes: 5,
  healthy_nodes: 4,
  models: [
    {
      name: 'llama3.3:8b',
      size_vram: Math.round(16.2 * 1024 * 1024 * 1024),
      size_disk: Math.round(4.7 * 1024 * 1024 * 1024),
      warm_count: 2,
      total_nodes: 3,
      // Demo parity for P52's digest-mismatch warning: these two nodes
      // deliberately report different digests for the same model name, as if
      // the tag was re-pulled with different content mid-rollout.
      digest_mismatch: true,
      nodes: [
        { name: 'gpu-node-01', healthy: true, digest: 'sha256:9f8a1c2d47e0b6f3a5d9c8e1b2f4a7d6' },
        { name: 'gpu-node-02', healthy: true, digest: 'sha256:3b7e0a91c4d8f2a6b0e5d7c3f9a1b8e4' },
      ],
    },
    {
      name: 'mistral:7b',
      size_vram: Math.round(14.8 * 1024 * 1024 * 1024),
      size_disk: Math.round(4.1 * 1024 * 1024 * 1024),
      warm_count: 2,
      total_nodes: 3,
      nodes: [
        { name: 'gpu-node-02', healthy: true },
        { name: 'gpu-node-03', healthy: true },
      ],
    },
    {
      name: 'llama3.3:70b',
      size_vram: Math.round(40.2 * 1024 * 1024 * 1024),
      size_disk: Math.round(42.5 * 1024 * 1024 * 1024),
      warm_count: 1,
      total_nodes: 3,
      nodes: [
        { name: 'gpu-node-01', healthy: true },
      ],
    },
    {
      name: 'qwen2.5-coder:14b',
      size_vram: Math.round(26.5 * 1024 * 1024 * 1024),
      size_disk: Math.round(9.0 * 1024 * 1024 * 1024),
      warm_count: 1,
      total_nodes: 3,
      nodes: [
        { name: 'gpu-node-02', healthy: true },
      ],
    },
    {
      name: 'gemma2:9b',
      size_vram: Math.round(14.1 * 1024 * 1024 * 1024),
      size_disk: Math.round(5.4 * 1024 * 1024 * 1024),
      warm_count: 1,
      total_nodes: 3,
      nodes: [
        { name: 'gpu-node-03', healthy: true },
      ],
    },
    {
      name: 'phi3:medium',
      size_vram: Math.round(4.2 * 1024 * 1024 * 1024),
      size_disk: Math.round(7.9 * 1024 * 1024 * 1024),
      warm_count: 0,
      total_nodes: 3,
      nodes: [
        { name: 'gpu-node-04', healthy: false },
      ],
    },
  ],
};

export const mockCloudProviders: CloudProvider[] = [
  {
    name: 'openai-gpt4o',
    provider: 'openai',
    base_url: 'https://api.openai.com/v1',
    default_model: 'gpt-4o',
    cost_per_1k_tokens: 0.005,
    enabled: true,
    priority: 10,
  },
  {
    name: 'anthropic-claude',
    provider: 'anthropic',
    base_url: 'https://api.anthropic.com/v1',
    default_model: 'claude-3-5-sonnet-20241022',
    cost_per_1k_tokens: 0.003,
    enabled: false,
    priority: 5,
  },
];

const now = Date.now();
const mins = (n: number) => new Date(now - n * 60000).toISOString();
const secs = (n: number) => new Date(now - n * 1000).toISOString();

export const mockRequests: RequestEntry[] = [
  { id: 'req-a1b2c3d4e5f6', time: secs(8),   key_name: 'Engineering Team',  model: 'deepseek-r1:7b',  node: 'gpu-node-01', status: 200, latency_ms: 42,   cloud: false },
  { id: 'req-b2c3d4e5f6a1', time: secs(22),  key_name: 'Engineering Team',  model: 'llama3.3:8b',     node: 'gpu-node-02', status: 200, latency_ms: 38,   cloud: false },
  { id: 'req-c3d4e5f6a1b2', time: secs(38),  key_name: 'Data Platform',     model: 'qwen2.5:14b',     node: 'gpu-node-02', status: 200, latency_ms: 74,   cloud: false },
  { id: 'req-d4e5f6a1b2c3', time: secs(51),  key_name: 'Engineering Team',  model: 'gpt-4o',           node: '',            status: 200, latency_ms: 312,  cloud: true  },
  { id: 'req-e5f6a1b2c3d4', time: mins(1),   key_name: 'CI/CD Pipeline',    model: 'qwen2.5-coder:14b',   node: 'gpu-node-01', status: 200, latency_ms: 29,   cloud: false },
  { id: 'req-f6a1b2c3d4e5', time: mins(2),   key_name: 'Engineering Team',  model: 'qwen3:8b',         node: 'gpu-node-03', status: 200, latency_ms: 67,   cloud: false },
  { id: 'req-a7b8c9d0e1f2', time: mins(3),   key_name: 'Data Platform',     model: 'llama3.3:70b',    node: 'gpu-node-01', status: 200, latency_ms: 183,  cloud: false },
  { id: 'req-b8c9d0e1f2a7', time: mins(5),   key_name: 'Support Bot',       model: 'deepseek-r1:7b',  node: 'gpu-node-02', status: 200, latency_ms: 55,   cloud: false },
  { id: 'req-c9d0e1f2a7b8', time: mins(6),   key_name: 'Engineering Team',  model: 'claude-sonnet-4', node: '',            status: 200, latency_ms: 521,  cloud: true  },
  { id: 'req-d0e1f2a7b8c9', time: mins(8),   key_name: 'CI/CD Pipeline',    model: 'qwen2.5-coder:14b',   node: 'gpu-node-01', status: 200, latency_ms: 24,   cloud: false },
  { id: 'req-e1f2a7b8c9d0', time: mins(10),  key_name: 'Data Platform',     model: 'qwen2.5:14b',     node: 'gpu-node-02', status: 200, latency_ms: 81,   cloud: false },
  { id: 'req-f2a7b8c9d0e1', time: mins(11),  key_name: 'Support Bot',       model: 'llama3.3:8b',     node: 'gpu-node-03', status: 200, latency_ms: 44,   cloud: false },
  { id: 'req-a3b8c9d0e1f2', time: mins(13),  key_name: 'Engineering Team',  model: 'qwen3:8b',         node: 'gpu-node-04', status: 429, latency_ms: 3,    cloud: false },
  { id: 'req-b4c9d0e1f2a3', time: mins(15),  key_name: 'Data Platform',     model: 'llama3.3:70b',    node: 'gpu-node-01', status: 500, latency_ms: 1104, cloud: false },
  { id: 'req-c5d0e1f2a3b4', time: mins(17),  key_name: 'Engineering Team',  model: 'gemma4:12b',      node: 'gpu-node-03', status: 200, latency_ms: 91,   cloud: false },
  { id: 'req-d6e1f2a3b4c5', time: mins(19),  key_name: 'Support Bot',       model: 'mistral:7b',      node: 'gpu-node-04', status: 200, latency_ms: 47,   cloud: false },
];

// filterMockRequests simulates the /admin/audit server-side filter params
// against the static demo request list, so the Requests page filter toolbar
// behaves the same in demo mode as it does against the real audit_log.
export interface MockRequestFilters {
  model?: string;
  key?: string;
  node?: string;
  status?: 'success' | 'client_error' | 'server_error';
  cloud?: boolean;
  since?: string; // ISO string
  until?: string; // ISO string
}

function matchesStatusCategory(status: number, category: 'success' | 'client_error' | 'server_error'): boolean {
  if (category === 'success') return status >= 200 && status < 300;
  if (category === 'client_error') return status >= 400 && status < 500;
  return status >= 500 && status < 600;
}

export function filterMockRequests(filters: MockRequestFilters): RequestEntry[] {
  const sinceMs = filters.since ? new Date(filters.since).getTime() : null;
  const untilMs = filters.until ? new Date(filters.until).getTime() : null;
  return mockRequests.filter((e) => {
    if (filters.model && !e.model.toLowerCase().includes(filters.model.toLowerCase())) return false;
    if (filters.key && !(e.key_name ?? '').toLowerCase().includes(filters.key.toLowerCase())) return false;
    if (filters.node && !(e.node ?? '').toLowerCase().includes(filters.node.toLowerCase())) return false;
    if (filters.status && !matchesStatusCategory(e.status, filters.status)) return false;
    if (filters.cloud !== undefined && e.cloud !== filters.cloud) return false;
    if (sinceMs !== null && new Date(e.time).getTime() < sinceMs) return false;
    if (untilMs !== null && new Date(e.time).getTime() >= untilMs) return false;
    return true;
  });
}

function makeHourKey(hoursAgo: number): string {
  const d = new Date(Date.now() - hoursAgo * 3600000);
  const y = d.getUTCFullYear();
  const mo = String(d.getUTCMonth() + 1).padStart(2, '0');
  const day = String(d.getUTCDate()).padStart(2, '0');
  const h = String(d.getUTCHours()).padStart(2, '0');
  return `${y}-${mo}-${day}T${h}`;
}

// Deterministic hourly pattern: 80-person engineering org with 8-model fleet, clear work-day peaks
// 24 entries, index 0 = 23h ago, index 23 = current hour
// Cloud overflow only during peak hours when all 5 local nodes are saturated
const _hourlyLocal = [51, 43, 38, 42, 56, 74, 119, 194, 300, 420, 481, 458, 385, 266, 329, 427, 448, 385, 266, 180, 150, 117, 126, 105];
const _hourlyCloud = [ 1,  0,  0,  1,  0,  1,   2,   6,  10,  16,  19,  17,  12,   8,  11,  18,  21,  13,   8,   6,   4,   2,   4,  2];

export const mockAnalytics: Analytics = {
  local_requests: 5465,
  cloud_requests: 186,
  total_saved_usd: 58.95,
  total_spent_usd: 1.86,
  hourly: Array.from({ length: 24 }, (_, i) => ({
    hour: makeHourKey(23 - i),
    local: _hourlyLocal[i],
    cloud: _hourlyCloud[i],
    saved_usd: parseFloat((_hourlyLocal[i] * 0.01).toFixed(4)),
    spent_usd: parseFloat((_hourlyCloud[i] * 0.01).toFixed(4)),
  })),
  by_model: [
    { model: 'deepseek-r1:7b', local: 1830, cloud: 60, saved_usd: 18.30 },
    { model: 'llama3.3:8b',    local: 1305, cloud: 42, saved_usd: 13.05 },
    { model: 'qwen3:8b',       local:  932, cloud: 32, saved_usd:  9.32 },
    { model: 'qwen2.5:14b',    local:  520, cloud: 18, saved_usd:  5.20 },
    { model: 'llama3.3:70b',   local:  215, cloud:  8, saved_usd:  6.45 },
    { model: 'qwen2.5-coder:14b',  local:  148, cloud:  6, saved_usd:  1.48 },
    { model: 'gemma4:12b',     local:  312, cloud: 12, saved_usd:  3.12 },
    { model: 'mistral:7b',     local:  203, cloud:  8, saved_usd:  2.03 },
  ],
};

// Mock system info: single RTX 4090 node, 64 GB RAM, 24 cores
export const mockSystemInfo: SystemInfo = {
  cpu_cores: 24,
  os: 'linux',
  arch: 'x86_64',
  ram_total_mb: 65536,
  ram_free_mb: 40960,
  gpus: [
    {
      name: 'NVIDIA RTX 4090',
      url: 'http://localhost:11434',
      vram_total_mb: 24576,
      vram_free_mb: 10240,
      vram_source: 'nvidia-smi',
      temperature_c: 58,
      power_draw_w: 210,
      healthy: true,
    },
  ],
};

// Mock ModelCatalogResponse for ModelAdvisor demo mode
// Demo node: NVIDIA RTX 4090 24GB, 10240MB free (14336MB in use - llama3.3:8b + deepseek-r1:7b loaded)
// Fit logic: green = fits free VRAM (<10240MB), yellow = fits total VRAM (needs eviction, <24576MB), red = too large
export const mockModelCatalogResponse: ModelCatalogResponse = {
  catalog: [
    {
      name: 'deepseek-r1:7b',
      display_name: 'DeepSeek R1 7B',
      description: 'DeepSeek\'s open reasoning model with chain-of-thought. Rivals GPT-4o on benchmarks at zero API cost.',
      param_count: '7B',
      categories: ['reasoning', 'chat', 'coding'],
      popular: true,
      rank: 1,
      variants: [
        { tag: 'deepseek-r1:7b', quantization: 'Q4_K_M', vram_est_mb: 5000, size_mb: 4200, recommended: true },
        { tag: 'deepseek-r1:7b-q2_k', quantization: 'Q2_K', vram_est_mb: 3100, size_mb: 2600, recommended: false },
        { tag: 'deepseek-r1:7b-fp16', quantization: 'F16', vram_est_mb: 14400, size_mb: 14000, recommended: false },
      ],
    },
    {
      name: 'llama3.3:8b',
      display_name: 'Llama 3.3 8B',
      description: 'Meta\'s flagship small model. Most-pulled model on Ollama with 116M downloads. Excellent general-purpose workhorse.',
      param_count: '8B',
      categories: ['chat', 'coding', 'general'],
      popular: true,
      rank: 2,
      variants: [
        { tag: 'llama3.3:8b', quantization: 'Q4_K_M', vram_est_mb: 5500, size_mb: 4700, recommended: true },
        { tag: 'llama3.3:8b-q2_k', quantization: 'Q2_K', vram_est_mb: 3400, size_mb: 2900, recommended: false },
        { tag: 'llama3.3:8b-fp16', quantization: 'F16', vram_est_mb: 16000, size_mb: 15500, recommended: false },
      ],
    },
    {
      name: 'qwen3:8b',
      display_name: 'Qwen3 8B',
      description: 'Alibaba\'s latest Qwen3 with hybrid thinking mode. Switch between fast answers and deep reasoning in the same model.',
      param_count: '8B',
      categories: ['chat', 'reasoning', 'coding'],
      popular: true,
      rank: 3,
      variants: [
        { tag: 'qwen3:8b', quantization: 'Q4_K_M', vram_est_mb: 5200, size_mb: 4500, recommended: true },
        { tag: 'qwen3:8b-q2_k', quantization: 'Q2_K', vram_est_mb: 3200, size_mb: 2700, recommended: false },
        { tag: 'qwen3:8b-fp16', quantization: 'F16', vram_est_mb: 16384, size_mb: 15800, recommended: false },
      ],
    },
    {
      name: 'gemma4:12b',
      display_name: 'Gemma 4 12B',
      description: 'Google\'s newest Gemma 4 with vision, tool use, and reasoning. 14.8M pulls and growing fast - released weeks ago.',
      param_count: '12B',
      categories: ['chat', 'vision', 'reasoning'],
      popular: true,
      rank: 4,
      variants: [
        { tag: 'gemma4:12b', quantization: 'Q4_K_M', vram_est_mb: 7800, size_mb: 6800, recommended: true },
        { tag: 'gemma4:12b-fp16', quantization: 'F16', vram_est_mb: 24000, size_mb: 23500, recommended: false },
      ],
    },
    {
      name: 'llama3.2:3b',
      display_name: 'Llama 3.2 3B',
      description: 'Meta\'s compact Llama 3.2. Best choice for edge deployments and low-VRAM GPUs.',
      param_count: '3B',
      categories: ['chat', 'general'],
      popular: true,
      rank: 5,
      variants: [
        { tag: 'llama3.2:3b', quantization: 'Q4_K_M', vram_est_mb: 2200, size_mb: 1800, recommended: true },
        { tag: 'llama3.2:3b-fp16', quantization: 'F16', vram_est_mb: 6400, size_mb: 6200, recommended: false },
      ],
    },
    {
      name: 'qwen2.5:14b',
      display_name: 'Qwen 2.5 14B',
      description: 'Qwen 2.5 14B with 128K context window and strong multilingual support. Popular for enterprise workloads.',
      param_count: '14B',
      categories: ['chat', 'coding'],
      popular: true,
      rank: 6,
      variants: [
        { tag: 'qwen2.5:14b', quantization: 'Q4_K_M', vram_est_mb: 9000, size_mb: 7800, recommended: true },
        { tag: 'qwen2.5:14b-fp16', quantization: 'F16', vram_est_mb: 28000, size_mb: 27500, recommended: false },
      ],
    },
    {
      name: 'mistral:7b',
      display_name: 'Mistral 7B',
      description: 'Fast and capable 7B from Mistral AI. 30M pulls and battle-tested in production.',
      param_count: '7B',
      categories: ['chat', 'coding', 'general'],
      popular: true,
      rank: 7,
      variants: [
        { tag: 'mistral:7b', quantization: 'Q4_K_M', vram_est_mb: 4800, size_mb: 4100, recommended: true },
        { tag: 'mistral:7b-fp16', quantization: 'F16', vram_est_mb: 14400, size_mb: 14000, recommended: false },
      ],
    },
    {
      name: 'gemma2:9b',
      display_name: 'Gemma 2 9B',
      description: 'Google\'s Gemma 2 9B, strong at instruction following and structured output.',
      param_count: '9B',
      categories: ['chat', 'general'],
      popular: false,
      rank: 8,
      variants: [
        { tag: 'gemma2:9b', quantization: 'Q4_K_M', vram_est_mb: 6200, size_mb: 5400, recommended: true },
        { tag: 'gemma2:9b-fp16', quantization: 'F16', vram_est_mb: 18000, size_mb: 17500, recommended: false },
      ],
    },
  ],
  nodes: [
    {
      name: 'gpu-node-01',
      url: 'http://localhost:11434',
      runtime: 'ollama',
      vram_free_bytes: 10 * 1024 * 1024 * 1024,
      vram_total_bytes: 24 * 1024 * 1024 * 1024,
      vram_used_bytes: 14 * 1024 * 1024 * 1024,
      vram_source: 'nvidia-smi',
      disk_free_gb: 420,
      disk_total_gb: 2000,
      disk_known: true,
      models: [],
    },
    {
      name: 'gpu-node-02',
      url: 'http://10.0.1.20:8000',
      runtime: 'vllm',
      vram_free_bytes: 40 * 1024 * 1024 * 1024,
      vram_total_bytes: 80 * 1024 * 1024 * 1024,
      vram_used_bytes: 40 * 1024 * 1024 * 1024,
      vram_source: 'declared',
      disk_free_gb: 1200,
      disk_total_gb: 4000,
      disk_known: true,
      capabilities: ['status', 'models.pull', 'models.list', 'runtime.health_check'],
      models: [],
    },
    {
      name: 'gpu-node-03',
      url: 'http://10.0.1.21:8080',
      runtime: 'tgi',
      vram_free_bytes: 18 * 1024 * 1024 * 1024,
      vram_total_bytes: 24 * 1024 * 1024 * 1024,
      vram_used_bytes: 6 * 1024 * 1024 * 1024,
      vram_source: 'declared',
      disk_free_gb: 15,
      disk_total_gb: 2000,
      disk_known: true,
      models: [],
    },
    {
      name: 'gpu-node-04',
      url: 'http://10.0.1.22:8080',
      runtime: 'llamacpp',
      vram_free_bytes: 20 * 1024 * 1024 * 1024,
      vram_total_bytes: 24 * 1024 * 1024 * 1024,
      vram_used_bytes: 4 * 1024 * 1024 * 1024,
      vram_source: 'declared',
      disk_free_gb: 0,
      disk_total_gb: 0,
      disk_known: false,
      capabilities: ['status', 'models.pull', 'models.list', 'runtime.health_check'],
      models: [],
    },
    {
      name: 'gpu-node-05',
      url: 'http://10.0.1.23:8080',
      runtime: 'mlx',
      vram_free_bytes: 105 * 1024 * 1024 * 1024,
      vram_total_bytes: 128 * 1024 * 1024 * 1024,
      vram_used_bytes: 23 * 1024 * 1024 * 1024,
      vram_source: 'declared',
      disk_free_gb: 900,
      disk_total_gb: 2000,
      disk_known: true,
      models: [],
    },
  ],
};

export const mockFavorites = [
  'bartowski/DeepSeek-R1-Distill-Qwen-8B-GGUF',
  'Qwen/Qwen2.5-Coder-7B-Instruct-GGUF',
];

export const mockHFModels = [
  {
    id: 'bartowski/Llama-3.2-3B-Instruct-GGUF',
    downloads: 124530,
    likes: 843,
    tags: ['text-generation', 'gguf', 'llama-3.2', 'conversational'],
    lastModified: '2026-06-15T18:30:00Z',
    pipeline_tag: 'text-generation'
  },
  {
    id: 'bartowski/Llama-3.2-1B-Instruct-GGUF',
    downloads: 98450,
    likes: 612,
    tags: ['text-generation', 'gguf', 'llama-3.2', 'conversational'],
    lastModified: '2026-06-14T12:00:00Z',
    pipeline_tag: 'text-generation'
  },
  {
    id: 'bartowski/DeepSeek-R1-Distill-Qwen-8B-GGUF',
    downloads: 245000,
    likes: 1845,
    tags: ['text-generation', 'gguf', 'deepseek', 'reasoning'],
    lastModified: '2026-06-18T09:15:00Z',
    pipeline_tag: 'text-generation'
  },
  {
    id: 'google/gemma-2-9b-it-GGUF',
    downloads: 75400,
    likes: 530,
    tags: ['text-generation', 'gguf', 'gemma', 'instruction'],
    lastModified: '2026-06-10T16:45:00Z',
    pipeline_tag: 'text-generation'
  },
  {
    id: 'Qwen/Qwen2.5-Coder-7B-Instruct-GGUF',
    downloads: 145200,
    likes: 1205,
    tags: ['text-generation', 'gguf', 'qwen', 'coding'],
    lastModified: '2026-06-16T10:20:00Z',
    pipeline_tag: 'text-generation'
  },
  {
    id: 'bartowski/Phi-3.5-mini-instruct-GGUF',
    downloads: 54100,
    likes: 412,
    tags: ['text-generation', 'gguf', 'phi-3.5', 'lightweight'],
    lastModified: '2026-06-11T14:50:00Z',
    pipeline_tag: 'text-generation'
  },
  {
    id: 'bartowski/Mistral-7B-Instruct-v0.3-GGUF',
    downloads: 189000,
    likes: 1420,
    tags: ['text-generation', 'gguf', 'mistral', 'general'],
    lastModified: '2026-06-08T08:30:00Z',
    pipeline_tag: 'text-generation'
  },
  {
    id: 'google/gemma-2-2b-it-GGUF',
    downloads: 110400,
    likes: 720,
    tags: ['text-generation', 'gguf', 'gemma', 'edge'],
    lastModified: '2026-06-12T11:15:00Z',
    pipeline_tag: 'text-generation'
  },
  {
    id: 'bartowski/Llama-3.2-11B-Vision-Instruct-GGUF',
    downloads: 67200,
    likes: 498,
    tags: ['image-text-to-text', 'gguf', 'llama-3.2', 'vision'],
    lastModified: '2026-06-17T15:20:00Z',
    pipeline_tag: 'image-text-to-text'
  },
  {
    id: 'bartowski/DeepSeek-R1-Distill-Llama-8B-GGUF',
    downloads: 320000,
    likes: 2140,
    tags: ['text-generation', 'gguf', 'deepseek', 'reasoning'],
    lastModified: '2026-06-19T07:45:00Z',
    pipeline_tag: 'text-generation'
  }
];

export const mockBenchmarkRuns: BenchmarkRun[] = [
  {
    id: 3, node: 'gpu-node-01', model: 'deepseek-r1:8b', n: 10,
    cold_p50_ms: 3840, cold_min_ms: 3510, cold_max_ms: 4220,
    warm_p50_ms: 112, warm_min_ms: 98, warm_max_ms: 141,
    speedup_x: 34.3,
    created_at: '2026-07-18T14:22:00Z',
  },
  {
    id: 2, node: 'gpu-node-03', model: 'qwen2.5-coder:14b', n: 10,
    cold_p50_ms: 5210, cold_min_ms: 4890, cold_max_ms: 5640,
    warm_p50_ms: 158, warm_min_ms: 130, warm_max_ms: 202,
    speedup_x: 33.0,
    created_at: '2026-07-16T09:05:00Z',
  },
  {
    id: 1, node: 'gpu-node-01', model: 'llama3.3:8b', n: 10,
    cold_p50_ms: 3120, cold_min_ms: 2900, cold_max_ms: 3480,
    warm_p50_ms: 96, warm_min_ms: 84, warm_max_ms: 120,
    speedup_x: 32.5,
    created_at: '2026-07-14T11:47:00Z',
  },
];

export const mockHFRepoDetails: Record<string, any> = {
  'bartowski/Llama-3.2-3B-Instruct-GGUF': {
    id: 'bartowski/Llama-3.2-3B-Instruct-GGUF',
    downloads: 124530,
    likes: 843,
    tags: ['text-generation', 'gguf', 'llama-3.2', 'conversational'],
    last_modified: '2026-06-15T18:30:00Z',
    variants: [
      { tag: 'hf.co/bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M', quantization: 'Q4_K_M', vram_est_mb: 2800, size_mb: 2020, recommended: true, fit: 'green', downloaded: false },
      { tag: 'hf.co/bartowski/Llama-3.2-3B-Instruct-GGUF:Q8_0', quantization: 'Q8_0', vram_est_mb: 4200, size_mb: 3200, recommended: false, fit: 'green', downloaded: false },
      { tag: 'hf.co/bartowski/Llama-3.2-3B-Instruct-GGUF:F16', quantization: 'F16', vram_est_mb: 7500, size_mb: 6100, recommended: false, fit: 'green', downloaded: false }
    ]
  },
  'bartowski/Llama-3.2-1B-Instruct-GGUF': {
    id: 'bartowski/Llama-3.2-1B-Instruct-GGUF',
    downloads: 98450,
    likes: 612,
    tags: ['text-generation', 'gguf', 'llama-3.2', 'conversational'],
    last_modified: '2026-06-14T12:00:00Z',
    variants: [
      { tag: 'hf.co/bartowski/Llama-3.2-1B-Instruct-GGUF:Q4_K_M', quantization: 'Q4_K_M', vram_est_mb: 1300, size_mb: 1100, recommended: true, fit: 'green', downloaded: false },
      { tag: 'hf.co/bartowski/Llama-3.2-1B-Instruct-GGUF:Q8_0', quantization: 'Q8_0', vram_est_mb: 2100, size_mb: 1800, recommended: false, fit: 'green', downloaded: false }
    ]
  },
  'bartowski/DeepSeek-R1-Distill-Qwen-8B-GGUF': {
    id: 'bartowski/DeepSeek-R1-Distill-Qwen-8B-GGUF',
    downloads: 245000,
    likes: 1845,
    tags: ['text-generation', 'gguf', 'deepseek', 'reasoning'],
    last_modified: '2026-06-18T09:15:00Z',
    // Deliberately small for demo parity: demonstrates the P48 hard-block
    // "No Disk Space" state on the Q8_0 variant (~8.3GB needed, 6GB free).
    disk_free_gb: 6,
    disk_total_gb: 500,
    disk_known: true,
    variants: [
      { tag: 'hf.co/bartowski/DeepSeek-R1-Distill-Qwen-8B-GGUF:Q4_K_M', quantization: 'Q4_K_M', vram_est_mb: 6200, size_mb: 4900, recommended: true, fit: 'green', downloaded: false },
      { tag: 'hf.co/bartowski/DeepSeek-R1-Distill-Qwen-8B-GGUF:Q8_0', quantization: 'Q8_0', vram_est_mb: 9500, size_mb: 8500, recommended: false, fit: 'yellow', downloaded: false }
    ]
  },
  'google/gemma-2-9b-it-GGUF': {
    id: 'google/gemma-2-9b-it-GGUF',
    downloads: 75400,
    likes: 530,
    tags: ['text-generation', 'gguf', 'gemma', 'instruction'],
    last_modified: '2026-06-10T16:45:00Z',
    variants: [
      { tag: 'hf.co/google/gemma-2-9b-it-GGUF:Q4_K_M', quantization: 'Q4_K_M', vram_est_mb: 7000, size_mb: 5800, recommended: true, fit: 'green', downloaded: false },
      { tag: 'hf.co/google/gemma-2-9b-it-GGUF:Q8_0', quantization: 'Q8_0', vram_est_mb: 11000, size_mb: 9600, recommended: false, fit: 'yellow', downloaded: false }
    ]
  },
  'Qwen/Qwen2.5-Coder-7B-Instruct-GGUF': {
    id: 'Qwen/Qwen2.5-Coder-7B-Instruct-GGUF',
    downloads: 145200,
    likes: 1205,
    tags: ['text-generation', 'gguf', 'qwen', 'coding'],
    last_modified: '2026-06-16T10:20:00Z',
    variants: [
      { tag: 'hf.co/Qwen/Qwen2.5-Coder-7B-Instruct-GGUF:Q4_K_M', quantization: 'Q4_K_M', vram_est_mb: 5200, size_mb: 4700, recommended: true, fit: 'green', downloaded: false },
      { tag: 'hf.co/Qwen/Qwen2.5-Coder-7B-Instruct-GGUF:Q8_0', quantization: 'Q8_0', vram_est_mb: 8800, size_mb: 7900, recommended: false, fit: 'green', downloaded: false }
    ]
  },
  'bartowski/Phi-3.5-mini-instruct-GGUF': {
    id: 'bartowski/Phi-3.5-mini-instruct-GGUF',
    downloads: 54100,
    likes: 412,
    tags: ['text-generation', 'gguf', 'phi-3.5', 'lightweight'],
    last_modified: '2026-06-11T14:50:00Z',
    variants: [
      { tag: 'hf.co/bartowski/Phi-3.5-mini-instruct-GGUF:Q4_K_M', quantization: 'Q4_K_M', vram_est_mb: 2900, size_mb: 2200, recommended: true, fit: 'green', downloaded: false },
      { tag: 'hf.co/bartowski/Phi-3.5-mini-instruct-GGUF:Q8_0', quantization: 'Q8_0', vram_est_mb: 4500, size_mb: 3800, recommended: false, fit: 'green', downloaded: false }
    ]
  },
  'bartowski/Mistral-7B-Instruct-v0.3-GGUF': {
    id: 'bartowski/Mistral-7B-Instruct-v0.3-GGUF',
    downloads: 189000,
    likes: 1420,
    tags: ['text-generation', 'gguf', 'mistral', 'general'],
    last_modified: '2026-06-08T08:30:00Z',
    variants: [
      { tag: 'hf.co/bartowski/Mistral-7B-Instruct-v0.3-GGUF:Q4_K_M', quantization: 'Q4_K_M', vram_est_mb: 5100, size_mb: 4400, recommended: true, fit: 'green', downloaded: false },
      { tag: 'hf.co/bartowski/Mistral-7B-Instruct-v0.3-GGUF:Q8_0', quantization: 'Q8_0', vram_est_mb: 8500, size_mb: 7600, recommended: false, fit: 'green', downloaded: false }
    ]
  },
  'google/gemma-2-2b-it-GGUF': {
    id: 'google/gemma-2-2b-it-GGUF',
    downloads: 110400,
    likes: 720,
    tags: ['text-generation', 'gguf', 'gemma', 'edge'],
    last_modified: '2026-06-12T11:15:00Z',
    variants: [
      { tag: 'hf.co/google/gemma-2-2b-it-GGUF:Q4_K_M', quantization: 'Q4_K_M', vram_est_mb: 1800, size_mb: 1600, recommended: true, fit: 'green', downloaded: false },
      { tag: 'hf.co/google/gemma-2-2b-it-GGUF:Q8_0', quantization: 'Q8_0', vram_est_mb: 3000, size_mb: 2700, recommended: false, fit: 'green', downloaded: false }
    ]
  },
  'bartowski/Llama-3.2-11B-Vision-Instruct-GGUF': {
    id: 'bartowski/Llama-3.2-11B-Vision-Instruct-GGUF',
    downloads: 67200,
    likes: 498,
    tags: ['image-text-to-text', 'gguf', 'llama-3.2', 'vision'],
    last_modified: '2026-06-17T15:20:00Z',
    variants: [
      { tag: 'hf.co/bartowski/Llama-3.2-11B-Vision-Instruct-GGUF:Q4_K_M', quantization: 'Q4_K_M', vram_est_mb: 7800, size_mb: 6900, recommended: true, fit: 'green', downloaded: false },
      { tag: 'hf.co/bartowski/Llama-3.2-11B-Vision-Instruct-GGUF:Q8_0', quantization: 'Q8_0', vram_est_mb: 12500, size_mb: 11400, recommended: false, fit: 'yellow', downloaded: false }
    ]
  },
  'bartowski/DeepSeek-R1-Distill-Llama-8B-GGUF': {
    id: 'bartowski/DeepSeek-R1-Distill-Llama-8B-GGUF',
    downloads: 320000,
    likes: 2140,
    tags: ['text-generation', 'gguf', 'deepseek', 'reasoning'],
    last_modified: '2026-06-19T07:45:00Z',
    variants: [
      { tag: 'hf.co/bartowski/DeepSeek-R1-Distill-Llama-8B-GGUF:Q4_K_M', quantization: 'Q4_K_M', vram_est_mb: 6100, size_mb: 4900, recommended: true, fit: 'green', downloaded: false },
      { tag: 'hf.co/bartowski/DeepSeek-R1-Distill-Llama-8B-GGUF:Q8_0', quantization: 'Q8_0', vram_est_mb: 9800, size_mb: 8500, recommended: false, fit: 'green', downloaded: false }
    ]
  }
};

// --- Model configuration overrides (demo) ---
// Plausible static profiles: a realistic MIX of some fields set, most left
// unconfigured - an all-fields-filled profile would look fabricated (R1).
// Keyed by (model, node) - the same model name can carry a different
// profile per node/runtime it's resident on (see mockGPUNodes/mockModelCatalog
// above: gpu-node-01=ollama, gpu-node-02=vllm, gpu-node-03=tgi, gpu-node-04=llamacpp,
// gpu-node-05=mlx).
const mockModelConfigSeed: ModelConfig[] = [
  {
    model: 'llama3.3:8b',
    node: 'gpu-node-01',
    num_ctx: 4096,
    num_gpu: 999,
    repeat_penalty: 1.15,
    rpm: 120,
    system: 'You are a careful reasoning assistant. Think step by step before answering.',
  },
  // mistral:7b is resident on both a vLLM node and a TGI node - two separate
  // profiles, since load-time/engine params only ever apply on Ollama and
  // the two OpenAI-compatible runtimes support different extra fields.
  {
    model: 'mistral:7b',
    node: 'gpu-node-02',
    temperature: 0.7,
    top_p: 0.9,
    top_k: 40,
    ignore_eos: false,
    rpm: 200,
  },
  {
    model: 'mistral:7b',
    node: 'gpu-node-03',
    temperature: 0.65,
    max_tokens: 2048,
  },
  // phi3:medium is resident on gpu-node-04 (llama.cpp) - exercises the
  // llama.cpp-only sampling extras (mirostat, DRY).
  {
    model: 'phi3:medium',
    node: 'gpu-node-04',
    temperature: 0.7,
    mirostat: 2,
    mirostat_tau: 5,
    dry_multiplier: 0.8,
    dry_base: 1.75,
  },
];

function modelConfigKey(model: string, node: string): string {
  return `${model}@@${node}`;
}

// Mutable in-memory demo store so the config modal behaves realistically
// (save/reset) within a session; resets on reload, same lifecycle as
// api.ts's demoUsers roster.
let demoModelConfigs: Map<string, ModelConfig> | null = null;
function demoModelConfigStore(): Map<string, ModelConfig> {
  if (!demoModelConfigs) {
    demoModelConfigs = new Map(mockModelConfigSeed.map(c => [modelConfigKey(c.model, c.node), c]));
  }
  return demoModelConfigs;
}

export function getMockModelConfig(model: string, node: string): ModelConfig | null {
  return demoModelConfigStore().get(modelConfigKey(model, node)) ?? null;
}

export function setMockModelConfig(cfg: ModelConfig): ModelConfig {
  demoModelConfigStore().set(modelConfigKey(cfg.model, cfg.node), cfg);
  return cfg;
}

export function deleteMockModelConfig(model: string, node: string): void {
  demoModelConfigStore().delete(modelConfigKey(model, node));
}

// --- Model config capabilities (demo) ---
// Mirrors internal/store/model_config_capabilities.go's SupportedFieldsFor
// table so the offline/demo build filters fields the same way the real API
// does. Hardcoded here deliberately - this is the demo/offline path, not the
// real API contract (the live build always calls fetchModelConfigCapabilities).
const OPENAI_COMPAT_BASE_FIELDS = [
  'temperature', 'top_p', 'max_tokens', 'seed', 'stop',
  'presence_penalty', 'frequency_penalty', 'response_format',
];
// mlx_lm.server's documented /v1/chat/completions schema (SERVER.md) has no
// seed or response_format equivalent - excluded from its base set below.
const MLX_BASE_FIELDS = OPENAI_COMPAT_BASE_FIELDS.filter(
  (f) => f !== 'seed' && f !== 'response_format'
);
// Ollama-only, verified against Ollama's current api/types.go
// Options/Runner structs - flash_attention, offload_kv_cache_to_gpu,
// rope_frequency_base/scale, use_mlock, and tensor_parallelism removed:
// none are real per-request params in current Ollama.
const OLLAMA_LOAD_TIME_FIELDS = [
  'num_ctx', 'num_gpu', 'main_gpu', 'num_batch', 'num_thread', 'use_mmap', 'draft_num_predict', 'ttl',
];
// mirostat*/tfs_z removed: not in Ollama's current Options struct (tfs_z was
// removed from llama.cpp itself too, hence its ModelConfig field is gone
// entirely). mirostat* remain valid for llama.cpp only, listed below.
const OLLAMA_INFERENCE_FIELDS = [
  'top_k', 'min_p', 'typical_p', 'num_keep', 'repeat_penalty', 'repeat_last_n',
];

export function getMockModelConfigCapabilities(): Record<string, string[]> {
  // system is supported on every runtime: injectModelDefaults prepends it as
  // a leading system-role message on chat-shaped OpenAI-compatible requests.
  // template stays Ollama-only - it's Ollama's own model-file
  // prompt-templating mechanism, with no OpenAI-compatible equivalent.
  return {
    ollama: [...OPENAI_COMPAT_BASE_FIELDS, ...OLLAMA_LOAD_TIME_FIELDS, ...OLLAMA_INFERENCE_FIELDS, 'system', 'template', 'rpm', 'tpm'],
    vllm: [
      ...OPENAI_COMPAT_BASE_FIELDS,
      'top_k', 'min_p', 'repetition_penalty',
      'length_penalty', 'stop_token_ids', 'include_stop_str_in_output',
      'ignore_eos', 'min_tokens', 'skip_special_tokens', 'truncate_prompt_tokens',
      'system', 'rpm', 'tpm',
    ],
    tgi: [...OPENAI_COMPAT_BASE_FIELDS, 'system', 'rpm', 'tpm'],
    mlx: [
      ...MLX_BASE_FIELDS,
      'top_k', 'min_p', 'repetition_penalty', 'logit_bias',
      'system', 'rpm', 'tpm',
    ],
    llamacpp: [
      ...OPENAI_COMPAT_BASE_FIELDS,
      'repeat_penalty', 'repeat_last_n', 'typical_p', 'mirostat', 'mirostat_tau', 'mirostat_eta',
      'num_keep', 'logit_bias', 'n_probs', 'min_keep',
      'dry_multiplier', 'dry_base', 'dry_allowed_length', 'dry_penalty_last_n',
      'xtc_probability', 'xtc_threshold', 'ignore_eos',
      'system', 'rpm', 'tpm',
    ],
  };
}



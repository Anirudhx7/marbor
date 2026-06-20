import { GPUNode, APIKey, RoutingRule, MetricData, TokenUsageData, RequestDistributionData, Settings, Savings, CloudProvider, ModelCatalog, RequestEntry, Analytics, ModelCatalogResponse } from '../types';
import type { SystemInfo } from './api';

const GB = 1024;
const GiB = 1024 * 1024 * 1024;

export const mockGPUNodes: GPUNode[] = [
  {
    id: 'node-1',
    name: 'gpu-node-01',
    gpuModel: 'NVIDIA A100 80GB',
    port: 11434,
    vramTotalMB: 80 * GB,
    vramUsedMB: Math.round(67.2 * GB),
    vramSource: 'nvidia',
    powerDrawW: 312,
    cpuPercent: 34,
    temperature: 72,
    health: 'healthy',
    draining: false,
    uptime: '14d 6h',
    loadedModels: [
      { name: 'deepseek-r1:32b', sizeVram: Math.round(20.4 * GiB) },
      { name: 'llama3.1:70b', sizeVram: Math.round(40.2 * GiB) },
    ],
    healthHistory: Array(60).fill(0).map(() => 95 + Math.random() * 5),
  },
  {
    id: 'node-2',
    name: 'gpu-node-02',
    gpuModel: 'NVIDIA A100 80GB',
    port: 11435,
    vramTotalMB: 80 * GB,
    vramUsedMB: Math.round(52.8 * GB),
    vramSource: 'nvidia',
    powerDrawW: 274,
    cpuPercent: 28,
    temperature: 68,
    health: 'healthy',
    draining: false,
    uptime: '12d 14h',
    loadedModels: [
      { name: 'llama3.1:8b', sizeVram: Math.round(5.5 * GiB) },
      { name: 'qwen3:8b', sizeVram: Math.round(5.2 * GiB) },
      { name: 'qwen2.5:14b', sizeVram: Math.round(21.3 * GiB) },
    ],
    healthHistory: Array(60).fill(0).map(() => 92 + Math.random() * 8),
  },
  {
    id: 'node-3',
    name: 'gpu-node-03',
    gpuModel: 'NVIDIA RTX 4090 24GB',
    port: 11436,
    vramTotalMB: 24 * GB,
    vramUsedMB: Math.round(18.5 * GB),
    vramSource: 'nvidia',
    powerDrawW: 185,
    cpuPercent: 45,
    temperature: 78,
    health: 'healthy',
    draining: false,
    uptime: '8d 2h',
    loadedModels: [
      { name: 'gemma4:12b', sizeVram: Math.round(7.8 * GiB) },
      { name: 'llama3.2:3b', sizeVram: Math.round(2.2 * GiB) },
    ],
    healthHistory: Array(60).fill(0).map(() => 88 + Math.random() * 10),
  },
  {
    id: 'node-4',
    name: 'gpu-node-04',
    gpuModel: 'NVIDIA RTX 4090 24GB',
    port: 11437,
    vramTotalMB: 24 * GB,
    vramUsedMB: Math.round(4.2 * GB),
    vramSource: 'nvidia',
    powerDrawW: 92,
    cpuPercent: 12,
    temperature: 45,
    health: 'degraded',
    draining: false,
    uptime: '3d 8h',
    loadedModels: [
      { name: 'phi3:medium', sizeVram: Math.round(4.2 * GiB) },
    ],
    healthHistory: Array(60).fill(0).map(() => 70 + Math.random() * 25),
  },
];

export const mockAPIKeys: APIKey[] = [
  {
    id: 'key-1',
    name: 'Production API',
    key: 'sk-prod-7a3f-b7c2-d4e5-f8g9',
    created: '2024-11-15',
    requestsToday: 12543,
    requestsThisMonth: 284756,
    tokensThisMonth: 42713400,
    estimatedCostUsd: 85.43,
    rateLimit: 10000,
    status: 'active',
    allowedModels: ['all'],
    expiresAt: null,
  },
  {
    id: 'key-2',
    name: 'Development API',
    key: 'sk-dev-9h2i-j4k5-l6m7-n8o9',
    created: '2024-12-01',
    requestsToday: 3421,
    requestsThisMonth: 45231,
    tokensThisMonth: 6784650,
    estimatedCostUsd: 13.57,
    rateLimit: 5000,
    status: 'active',
    allowedModels: ['deepseek-r1:7b', 'llama3.1:8b', 'qwen3:8b'],
    expiresAt: null,
  },
  {
    id: 'key-3',
    name: 'CI/CD Pipeline',
    key: 'sk-ci-1p2q-r3s4-t5u6-v7w8',
    created: '2024-12-10',
    requestsToday: 892,
    requestsThisMonth: 12345,
    tokensThisMonth: 1851750,
    estimatedCostUsd: 3.70,
    rateLimit: 2000,
    status: 'active',
    allowedModels: ['llama3.2:3b', 'llama3.1:8b'],
    expiresAt: '2025-03-01',
  },
  {
    id: 'key-4',
    name: 'External Partner',
    key: 'sk-ext-8x9y-z1a2-b3c4-d5e6',
    created: '2024-12-05',
    requestsToday: 0,
    requestsThisMonth: 5678,
    tokensThisMonth: 851700,
    estimatedCostUsd: 1.70,
    rateLimit: 1000,
    status: 'suspended',
    allowedModels: ['llama3.1:8b'],
    expiresAt: null,
  },
  {
    id: 'key-5',
    name: 'Load Testing',
    key: 'sk-load-4f5g-h6i7-j8k9-l0m1',
    created: '2024-12-20',
    requestsToday: 45000,
    requestsThisMonth: 89000,
    tokensThisMonth: 13350000,
    estimatedCostUsd: 26.70,
    rateLimit: 50000,
    status: 'rate-limited',
    allowedModels: ['all'],
    expiresAt: '2025-01-01',
  },
];

export const mockRoutingRules: RoutingRule[] = [
  {
    id: 'rule-1',
    priority: 1,
    condition: 'model =~ "70b"',
    targetNode: 'gpu-node-01',
    strategy: 'warm-first',
    enabled: true,
  },
  {
    id: 'rule-2',
    priority: 2,
    condition: 'model =~ "mixtral"',
    targetNode: 'gpu-node-01',
    strategy: 'warm-first',
    enabled: true,
  },
  {
    id: 'rule-3',
    priority: 3,
    condition: 'model =~ "8b|7b|9b"',
    targetNode: 'gpu-node-02',
    strategy: 'least-conn',
    enabled: true,
  },
  {
    id: 'rule-4',
    priority: 4,
    condition: 'model =~ "3b"',
    targetNode: 'gpu-node-03',
    strategy: 'round-robin',
    enabled: true,
  },
  {
    id: 'rule-5',
    priority: 5,
    condition: 'api_key == "sk-ci-*"',
    targetNode: 'gpu-node-04',
    strategy: 'round-robin',
    enabled: false,
  },
];

// Deterministic so the demo chart is stable across refreshes (no Math.random
// sawtooth - same honesty standard the real product holds itself to). A diurnal
// base plus a fixed harmonic gives believable shape without jumping each render.
export const generateRequestsPerMinuteData = (): MetricData[] => {
  const data: MetricData[] = [];
  const now = new Date();
  for (let i = 1440; i >= 0; i -= 5) {
    const time = new Date(now.getTime() - i * 60000);
    const value = 50 + Math.sin(i / 100) * 30 + Math.sin(i / 13) * 12;
    data.push({
      timestamp: time.toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' }),
      value: Math.max(0, Math.floor(value)),
    });
  }
  return data;
};

export const generateTokenUsageData = (): TokenUsageData[] => [
  { keyName: 'Production API', tokens: 2847563 },
  { keyName: 'Development API', tokens: 452312 },
  { keyName: 'Load Testing', tokens: 890012 },
  { keyName: 'CI/CD Pipeline', tokens: 123456 },
  { keyName: 'External Partner', tokens: 56789 },
  { keyName: 'Internal Tools', tokens: 345678 },
  { keyName: 'Research Team', tokens: 234567 },
  { keyName: 'QA Testing', tokens: 123456 },
  { keyName: 'Staging Env', tokens: 98765 },
  { keyName: 'Demo Account', tokens: 45678 },
];

// Deterministic latency series (see generateRequestsPerMinuteData) - stable
// ~45-70ms shape, no per-render random jitter.
export const generateNodeLatencyData = (): MetricData[] => {
  const data: MetricData[] = [];
  const now = new Date();
  for (let i = 1440; i >= 0; i -= 10) {
    const time = new Date(now.getTime() - i * 60000);
    const value = 52 + Math.sin(i / 60) * 12 + Math.sin(i / 7) * 6;
    data.push({
      timestamp: time.toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' }),
      value: Math.floor(value),
    });
  }
  return data;
};

export const generateRequestDistributionData = (): RequestDistributionData[] => [
  { nodeName: 'gpu-node-01', requests: 45231 },
  { nodeName: 'gpu-node-02', requests: 38456 },
  { nodeName: 'gpu-node-03', requests: 23145 },
  { nodeName: 'gpu-node-04', requests: 9876 },
];

export const defaultSettings: Settings = {
  proxyPort: 11434,
  authMode: 'api-key',
  liteLLMEnabled: false,
  liteLLMEndpoint: 'http://localhost:4000',
  pollingInterval: 2000,
  prometheusEnabled: true,
  prometheusPort: 9090,
  logLevel: 'info',
};

export const mockSavings: Savings = {
  local_requests: 342567,
  cloud_requests: 12453,
  cloud_spent_usd: 48.72,
  saved_usd: 284.15,
  total_requests: 355020,
};

export const mockModelCatalog: ModelCatalog = {
  total_models: 6,
  total_nodes: 4,
  healthy_nodes: 3,
  models: [
    {
      name: 'llama3.1:8b',
      size_vram: Math.round(16.2 * 1024 * 1024 * 1024),
      warm_count: 2,
      total_nodes: 3,
      nodes: [
        { name: 'gpu-node-01', healthy: true },
        { name: 'gpu-node-02', healthy: true },
      ],
    },
    {
      name: 'mistral:7b',
      size_vram: Math.round(14.8 * 1024 * 1024 * 1024),
      warm_count: 2,
      total_nodes: 3,
      nodes: [
        { name: 'gpu-node-02', healthy: true },
        { name: 'gpu-node-03', healthy: true },
      ],
    },
    {
      name: 'llama3.1:70b',
      size_vram: Math.round(40.2 * 1024 * 1024 * 1024),
      warm_count: 1,
      total_nodes: 3,
      nodes: [
        { name: 'gpu-node-01', healthy: true },
      ],
    },
    {
      name: 'codellama:13b',
      size_vram: Math.round(26.5 * 1024 * 1024 * 1024),
      warm_count: 1,
      total_nodes: 3,
      nodes: [
        { name: 'gpu-node-02', healthy: true },
      ],
    },
    {
      name: 'gemma2:9b',
      size_vram: Math.round(14.1 * 1024 * 1024 * 1024),
      warm_count: 1,
      total_nodes: 3,
      nodes: [
        { name: 'gpu-node-03', healthy: true },
      ],
    },
    {
      name: 'phi3:medium',
      size_vram: Math.round(4.2 * 1024 * 1024 * 1024),
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
  },
  {
    name: 'anthropic-claude',
    provider: 'anthropic',
    base_url: 'https://api.anthropic.com/v1',
    default_model: 'claude-3-5-sonnet-20241022',
    cost_per_1k_tokens: 0.003,
    enabled: false,
  },
];

const now = Date.now();
const mins = (n: number) => new Date(now - n * 60000).toISOString();
const secs = (n: number) => new Date(now - n * 1000).toISOString();

export const mockRequests: RequestEntry[] = [
  { id: 'req-a1b2c3d4e5f6', time: secs(8),   key_name: 'Production API',  model: 'deepseek-r1:32b', node: 'gpu-node-01', status: 200, latency_ms: 42,   cloud: false },
  { id: 'req-b2c3d4e5f6a1', time: secs(31),  key_name: 'Development API', model: 'llama3.1:8b',     node: 'gpu-node-02', status: 200, latency_ms: 38,   cloud: false },
  { id: 'req-c3d4e5f6a1b2', time: secs(55),  key_name: 'Production API',  model: 'gpt-4o',           node: '',            status: 200, latency_ms: 312,  cloud: true  },
  { id: 'req-d4e5f6a1b2c3', time: mins(2),   key_name: 'CI/CD Pipeline',  model: 'llama3.2:3b',     node: 'gpu-node-03', status: 200, latency_ms: 21,   cloud: false },
  { id: 'req-e5f6a1b2c3d4', time: mins(3),   key_name: 'Production API',  model: 'qwen3:8b',         node: 'gpu-node-02', status: 200, latency_ms: 67,   cloud: false },
  { id: 'req-f6a1b2c3d4e5', time: mins(5),   key_name: 'Development API', model: 'gemma4:12b',       node: 'gpu-node-03', status: 429, latency_ms: 3,    cloud: false },
  { id: 'req-a7b8c9d0e1f2', time: mins(7),   key_name: 'Production API',  model: 'deepseek-r1:7b',  node: 'gpu-node-02', status: 200, latency_ms: 55,   cloud: false },
  { id: 'req-b8c9d0e1f2a7', time: mins(9),   key_name: 'CI/CD Pipeline',  model: 'claude-3-5-sonnet-20241022', node: '', status: 200, latency_ms: 487, cloud: true },
  { id: 'req-c9d0e1f2a7b8', time: mins(12),  key_name: 'Production API',  model: 'llama3.1:70b',    node: 'gpu-node-01', status: 500, latency_ms: 1204, cloud: false },
  { id: 'req-d0e1f2a7b8c9', time: mins(15),  key_name: 'Development API', model: 'qwen2.5:14b',     node: 'gpu-node-02', status: 200, latency_ms: 33,   cloud: false },
];

function makeHourKey(hoursAgo: number): string {
  const d = new Date(Date.now() - hoursAgo * 3600000);
  const y = d.getUTCFullYear();
  const mo = String(d.getUTCMonth() + 1).padStart(2, '0');
  const day = String(d.getUTCDate()).padStart(2, '0');
  const h = String(d.getUTCHours()).padStart(2, '0');
  return `${y}-${mo}-${day}T${h}`;
}

// Deterministic hourly pattern: base traffic + spikes at hours 9-11 and 14-16 UTC
// 24 entries, index 0 = 23h ago, index 23 = current hour
const _hourlyLocal = [8, 6, 5, 5, 7, 10, 18, 28, 38, 40, 37, 32, 28, 30, 36, 38, 34, 28, 22, 18, 14, 12, 10, 9];
const _hourlyCloud = [0, 0, 0, 0, 0,  0,  1,  1,  2,  2,  1,  1,  1,  1,  2,  2,  1,  1,  1,  0,  0,  0,  0, 0];

export const mockAnalytics: Analytics = {
  local_requests: 831,
  cloud_requests: 16,
  total_saved_usd: 4.1557,
  total_spent_usd: 0.0048,
  hourly: Array.from({ length: 24 }, (_, i) => ({
    hour: makeHourKey(23 - i),
    local: _hourlyLocal[i],
    cloud: _hourlyCloud[i],
    saved_usd: parseFloat((_hourlyLocal[i] * 0.005).toFixed(4)),
    spent_usd: parseFloat((_hourlyCloud[i] * 0.003).toFixed(4)),
  })),
  by_model: [
    { model: 'deepseek-r1:7b', local: 374, cloud: 7,  saved_usd: 1.870 },
    { model: 'llama3.1:8b',    local: 266, cloud: 5,  saved_usd: 1.330 },
    { model: 'qwen3:8b',       local: 191, cloud: 4,  saved_usd: 0.955 },
  ],
};

export const configFileYAML = `# Ollama-Mesh Configuration
# Generated: ${new Date().toISOString()}

proxy:
  port: 11434
  host: 0.0.0.0
  auth:
    mode: api-key
    jwt_secret: \${JWT_SECRET}
    
routing:
  strategy: warm-first
  fallback: least-connections
  health_check_interval: 30s
  
litellm:
  enabled: false
  endpoint: http://localhost:4000
  api_key: \${LITELLM_API_KEY}
  
gpu_nodes:
  - name: gpu-node-01
    host: 10.0.1.10
    port: 11434
    gpu: NVIDIA A100 80GB
    labels:
      tier: production
      
  - name: gpu-node-02
    host: 10.0.1.11
    port: 11435
    gpu: NVIDIA A100 80GB
    labels:
      tier: production
      
  - name: gpu-node-03
    host: 10.0.1.12
    port: 11436
    gpu: NVIDIA RTX 4090 24GB
    labels:
      tier: development
      
  - name: gpu-node-04
    host: 10.0.1.13
    port: 11437
    gpu: NVIDIA RTX 4090 24GB
    labels:
      tier: development

observability:
  prometheus:
    enabled: true
    port: 9090
    path: /metrics
  logging:
    level: info
    format: json
    output: stdout
`;

// Mock system info: single RTX 3070 node, 16 GB RAM, 8 cores
export const mockSystemInfo: SystemInfo = {
  cpu_cores: 8,
  os: 'linux',
  arch: 'x86_64',
  ram_total_mb: 16384,
  ram_free_mb: 8192,
  gpus: [
    {
      name: 'NVIDIA RTX 3070',
      url: 'http://localhost:11434',
      vram_total_mb: 8192,
      vram_free_mb: 3200,
      vram_source: 'nvidia-smi',
      temperature_c: 62,
      power_draw_w: 145,
      healthy: true,
    },
  ],
};

// Mock ModelCatalogResponse for ModelAdvisor demo mode
// Demo node: NVIDIA RTX 3070 8GB, 3200MB free (4992MB in use)
// Fit logic: green = fits free VRAM, yellow = fits total VRAM (needs eviction), red = too large for GPU
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
      name: 'llama3.1:8b',
      display_name: 'Llama 3.1 8B',
      description: 'Meta\'s flagship small model. Most-pulled model on Ollama with 116M downloads. Excellent general-purpose workhorse.',
      param_count: '8B',
      categories: ['chat', 'coding', 'general'],
      popular: true,
      rank: 2,
      variants: [
        { tag: 'llama3.1:8b', quantization: 'Q4_K_M', vram_est_mb: 5500, size_mb: 4700, recommended: true },
        { tag: 'llama3.1:8b-q2_k', quantization: 'Q2_K', vram_est_mb: 3400, size_mb: 2900, recommended: false },
        { tag: 'llama3.1:8b-fp16', quantization: 'F16', vram_est_mb: 16000, size_mb: 15500, recommended: false },
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
      name: 'localhost',
      url: 'http://localhost:11434',
      vram_free_bytes: 3200 * 1024 * 1024,
      vram_total_bytes: 8192 * 1024 * 1024,
      vram_source: 'nvidia-smi',
      models: [
        {
          name: 'deepseek-r1:7b',
          display_name: 'DeepSeek R1 7B',
          description: 'DeepSeek\'s open reasoning model with chain-of-thought. Rivals GPT-4o on benchmarks at zero API cost.',
          param_count: '7B',
          categories: ['reasoning', 'chat', 'coding'],
          popular: true,
          rank: 1,
          downloaded: false,
          variants: [
            { tag: 'deepseek-r1:7b', quantization: 'Q4_K_M', vram_est_mb: 5000, size_mb: 4200, recommended: true, fit: 'yellow' },
            { tag: 'deepseek-r1:7b-q2_k', quantization: 'Q2_K', vram_est_mb: 3100, size_mb: 2600, recommended: false, fit: 'green' },
            { tag: 'deepseek-r1:7b-fp16', quantization: 'F16', vram_est_mb: 14400, size_mb: 14000, recommended: false, fit: 'red' },
          ],
        },
        {
          name: 'llama3.1:8b',
          display_name: 'Llama 3.1 8B',
          description: 'Meta\'s flagship small model. Most-pulled model on Ollama with 116M downloads. Excellent general-purpose workhorse.',
          param_count: '8B',
          categories: ['chat', 'coding', 'general'],
          popular: true,
          rank: 2,
          downloaded: true,
          variants: [
            { tag: 'llama3.1:8b', quantization: 'Q4_K_M', vram_est_mb: 5500, size_mb: 4700, recommended: true, fit: 'yellow' },
            { tag: 'llama3.1:8b-q2_k', quantization: 'Q2_K', vram_est_mb: 3400, size_mb: 2900, recommended: false, fit: 'yellow' },
            { tag: 'llama3.1:8b-fp16', quantization: 'F16', vram_est_mb: 16000, size_mb: 15500, recommended: false, fit: 'red' },
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
          downloaded: false,
          variants: [
            { tag: 'qwen3:8b', quantization: 'Q4_K_M', vram_est_mb: 5200, size_mb: 4500, recommended: true, fit: 'yellow' },
            { tag: 'qwen3:8b-q2_k', quantization: 'Q2_K', vram_est_mb: 3200, size_mb: 2700, recommended: false, fit: 'green' },
            { tag: 'qwen3:8b-fp16', quantization: 'F16', vram_est_mb: 16384, size_mb: 15800, recommended: false, fit: 'red' },
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
          downloaded: false,
          variants: [
            { tag: 'gemma4:12b', quantization: 'Q4_K_M', vram_est_mb: 7800, size_mb: 6800, recommended: true, fit: 'yellow' },
            { tag: 'gemma4:12b-fp16', quantization: 'F16', vram_est_mb: 24000, size_mb: 23500, recommended: false, fit: 'red' },
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
          downloaded: true,
          variants: [
            { tag: 'llama3.2:3b', quantization: 'Q4_K_M', vram_est_mb: 2200, size_mb: 1800, recommended: true, fit: 'green' },
            { tag: 'llama3.2:3b-fp16', quantization: 'F16', vram_est_mb: 6400, size_mb: 6200, recommended: false, fit: 'yellow' },
          ],
        },
        {
          name: 'qwen2.5:14b',
          display_name: 'Qwen 2.5 14B',
          description: 'Qwen 2.5 14B with 128K context window and strong multilingual support.',
          param_count: '14B',
          categories: ['chat', 'coding'],
          popular: true,
          rank: 6,
          downloaded: false,
          variants: [
            { tag: 'qwen2.5:14b', quantization: 'Q4_K_M', vram_est_mb: 9000, size_mb: 7800, recommended: true, fit: 'red' },
            { tag: 'qwen2.5:14b-fp16', quantization: 'F16', vram_est_mb: 28000, size_mb: 27500, recommended: false, fit: 'red' },
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
          downloaded: false,
          variants: [
            { tag: 'mistral:7b', quantization: 'Q4_K_M', vram_est_mb: 4800, size_mb: 4100, recommended: true, fit: 'yellow' },
            { tag: 'mistral:7b-fp16', quantization: 'F16', vram_est_mb: 14400, size_mb: 14000, recommended: false, fit: 'red' },
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
          downloaded: false,
          variants: [
            { tag: 'gemma2:9b', quantization: 'Q4_K_M', vram_est_mb: 6200, size_mb: 5400, recommended: true, fit: 'yellow' },
            { tag: 'gemma2:9b-fp16', quantization: 'F16', vram_est_mb: 18000, size_mb: 17500, recommended: false, fit: 'red' },
          ],
        },
      ],
    },
  ],
};

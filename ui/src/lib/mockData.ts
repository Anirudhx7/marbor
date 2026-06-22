import { GPUNode, APIKey, Settings, Savings, CloudProvider, ModelCatalog, RequestEntry, Analytics, ModelCatalogResponse } from '../types';
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
    name: 'Engineering Team',
    key: 'om-eng-7a3f-b7c2-d4e5-f8g9',
    created: '2026-01-15',
    requestsToday: 3124,
    requestsThisMonth: 142380,
    tokensThisMonth: 284760000,
    estimatedCostUsd: 1423.80,
    rateLimit: 10000,
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
    status: 'active',
    allowedModels: ['qwen2.5:14b', 'llama3.1:70b', 'deepseek-r1:7b'],
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
    status: 'active',
    allowedModels: ['llama3.1:8b', 'deepseek-r1:7b'],
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
    status: 'active',
    allowedModels: ['codellama:13b', 'llama3.1:8b'],
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
    status: 'active',
    allowedModels: ['qwen2.5:14b', 'llama3.1:8b'],
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
    status: 'suspended',
    allowedModels: ['llama3.1:8b'],
    expiresAt: null,
  },
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
  local_requests: 328547,
  cloud_requests: 13920,
  cloud_spent_usd: 248.63,
  saved_usd: 4724.18,
  total_requests: 342467,
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
  { id: 'req-a1b2c3d4e5f6', time: secs(8),   key_name: 'Engineering Team',  model: 'deepseek-r1:7b',  node: 'gpu-node-01', status: 200, latency_ms: 42,   cloud: false },
  { id: 'req-b2c3d4e5f6a1', time: secs(22),  key_name: 'Engineering Team',  model: 'llama3.1:8b',     node: 'gpu-node-02', status: 200, latency_ms: 38,   cloud: false },
  { id: 'req-c3d4e5f6a1b2', time: secs(38),  key_name: 'Data Platform',     model: 'qwen2.5:14b',     node: 'gpu-node-02', status: 200, latency_ms: 74,   cloud: false },
  { id: 'req-d4e5f6a1b2c3', time: secs(51),  key_name: 'Engineering Team',  model: 'gpt-4o',           node: '',            status: 200, latency_ms: 312,  cloud: true  },
  { id: 'req-e5f6a1b2c3d4', time: mins(1),   key_name: 'CI/CD Pipeline',    model: 'codellama:13b',   node: 'gpu-node-01', status: 200, latency_ms: 29,   cloud: false },
  { id: 'req-f6a1b2c3d4e5', time: mins(2),   key_name: 'Engineering Team',  model: 'qwen3:8b',         node: 'gpu-node-03', status: 200, latency_ms: 67,   cloud: false },
  { id: 'req-a7b8c9d0e1f2', time: mins(3),   key_name: 'Data Platform',     model: 'llama3.1:70b',    node: 'gpu-node-01', status: 200, latency_ms: 183,  cloud: false },
  { id: 'req-b8c9d0e1f2a7', time: mins(5),   key_name: 'Support Bot',       model: 'deepseek-r1:7b',  node: 'gpu-node-02', status: 200, latency_ms: 55,   cloud: false },
  { id: 'req-c9d0e1f2a7b8', time: mins(6),   key_name: 'Engineering Team',  model: 'claude-sonnet-4', node: '',            status: 200, latency_ms: 521,  cloud: true  },
  { id: 'req-d0e1f2a7b8c9', time: mins(8),   key_name: 'CI/CD Pipeline',    model: 'codellama:13b',   node: 'gpu-node-01', status: 200, latency_ms: 24,   cloud: false },
  { id: 'req-e1f2a7b8c9d0', time: mins(10),  key_name: 'Data Platform',     model: 'qwen2.5:14b',     node: 'gpu-node-02', status: 200, latency_ms: 81,   cloud: false },
  { id: 'req-f2a7b8c9d0e1', time: mins(11),  key_name: 'Support Bot',       model: 'llama3.1:8b',     node: 'gpu-node-03', status: 200, latency_ms: 44,   cloud: false },
  { id: 'req-a3b8c9d0e1f2', time: mins(13),  key_name: 'Engineering Team',  model: 'qwen3:8b',         node: 'gpu-node-04', status: 429, latency_ms: 3,    cloud: false },
  { id: 'req-b4c9d0e1f2a3', time: mins(15),  key_name: 'Data Platform',     model: 'llama3.1:70b',    node: 'gpu-node-01', status: 500, latency_ms: 1104, cloud: false },
];

function makeHourKey(hoursAgo: number): string {
  const d = new Date(Date.now() - hoursAgo * 3600000);
  const y = d.getUTCFullYear();
  const mo = String(d.getUTCMonth() + 1).padStart(2, '0');
  const day = String(d.getUTCDate()).padStart(2, '0');
  const h = String(d.getUTCHours()).padStart(2, '0');
  return `${y}-${mo}-${day}T${h}`;
}

// Deterministic hourly pattern: 80-person engineering org with 6-model fleet, clear work-day peaks
// 24 entries, index 0 = 23h ago, index 23 = current hour
// Cloud overflow only during peak hours when all 4 local nodes are saturated
const _hourlyLocal = [46, 39, 34, 38, 51, 67, 108, 176, 272, 380, 436, 415, 349, 241, 298, 387, 406, 349, 241, 163, 136, 106, 114, 95];
const _hourlyCloud = [ 1,  0,  0,  1,  0,  1,   2,   5,   9,  14,  17,  15,  11,   7,  10,  16,  19,  12,   7,   5,   4,   2,   4,  2];

export const mockAnalytics: Analytics = {
  local_requests: 4950,
  cloud_requests: 166,
  total_saved_usd: 53.80,
  total_spent_usd: 1.66,
  hourly: Array.from({ length: 24 }, (_, i) => ({
    hour: makeHourKey(23 - i),
    local: _hourlyLocal[i],
    cloud: _hourlyCloud[i],
    saved_usd: parseFloat((_hourlyLocal[i] * 0.01).toFixed(4)),
    spent_usd: parseFloat((_hourlyCloud[i] * 0.01).toFixed(4)),
  })),
  by_model: [
    { model: 'deepseek-r1:7b', local: 1830, cloud: 60, saved_usd: 18.30 },
    { model: 'llama3.1:8b',    local: 1305, cloud: 42, saved_usd: 13.05 },
    { model: 'qwen3:8b',       local:  932, cloud: 32, saved_usd:  9.32 },
    { model: 'qwen2.5:14b',    local:  520, cloud: 18, saved_usd:  5.20 },
    { model: 'llama3.1:70b',   local:  215, cloud:  8, saved_usd:  6.45 },
    { model: 'codellama:13b',  local:  148, cloud:  6, saved_usd:  1.48 },
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
// Demo node: NVIDIA RTX 4090 24GB, 10240MB free (14336MB in use - llama3.1:8b + deepseek-r1:7b loaded)
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
      vram_free_bytes: 10240 * 1024 * 1024,
      vram_total_bytes: 24576 * 1024 * 1024,
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
            { tag: 'deepseek-r1:7b', quantization: 'Q4_K_M', vram_est_mb: 5000, size_mb: 4200, recommended: true, fit: 'green' },
            { tag: 'deepseek-r1:7b-q2_k', quantization: 'Q2_K', vram_est_mb: 3100, size_mb: 2600, recommended: false, fit: 'green' },
            { tag: 'deepseek-r1:7b-fp16', quantization: 'F16', vram_est_mb: 14400, size_mb: 14000, recommended: false, fit: 'yellow' },
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
            { tag: 'llama3.1:8b', quantization: 'Q4_K_M', vram_est_mb: 5500, size_mb: 4700, recommended: true, fit: 'green' },
            { tag: 'llama3.1:8b-q2_k', quantization: 'Q2_K', vram_est_mb: 3400, size_mb: 2900, recommended: false, fit: 'green' },
            { tag: 'llama3.1:8b-fp16', quantization: 'F16', vram_est_mb: 16000, size_mb: 15500, recommended: false, fit: 'yellow' },
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
            { tag: 'qwen3:8b', quantization: 'Q4_K_M', vram_est_mb: 5200, size_mb: 4500, recommended: true, fit: 'green' },
            { tag: 'qwen3:8b-q2_k', quantization: 'Q2_K', vram_est_mb: 3200, size_mb: 2700, recommended: false, fit: 'green' },
            { tag: 'qwen3:8b-fp16', quantization: 'F16', vram_est_mb: 16384, size_mb: 15800, recommended: false, fit: 'yellow' },
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
            { tag: 'gemma4:12b', quantization: 'Q4_K_M', vram_est_mb: 7800, size_mb: 6800, recommended: true, fit: 'green' },
            { tag: 'gemma4:12b-fp16', quantization: 'F16', vram_est_mb: 24000, size_mb: 23500, recommended: false, fit: 'yellow' },
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
            { tag: 'qwen2.5:14b', quantization: 'Q4_K_M', vram_est_mb: 9000, size_mb: 7800, recommended: true, fit: 'green' },
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
            { tag: 'mistral:7b', quantization: 'Q4_K_M', vram_est_mb: 4800, size_mb: 4100, recommended: true, fit: 'green' },
            { tag: 'mistral:7b-fp16', quantization: 'F16', vram_est_mb: 14400, size_mb: 14000, recommended: false, fit: 'yellow' },
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
            { tag: 'gemma2:9b', quantization: 'Q4_K_M', vram_est_mb: 6200, size_mb: 5400, recommended: true, fit: 'green' },
            { tag: 'gemma2:9b-fp16', quantization: 'F16', vram_est_mb: 18000, size_mb: 17500, recommended: false, fit: 'yellow' },
          ],
        },
      ],
    },
  ],
};

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
;


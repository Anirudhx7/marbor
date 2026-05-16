import { GPUNode, APIKey, RoutingRule, MetricData, TokenUsageData, RequestDistributionData, Settings } from '../types';

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
    powerDrawW: 312,
    cpuPercent: 34,
    temperature: 72,
    health: 'healthy',
    uptime: '14d 6h',
    loadedModels: [
      { name: 'llama3.1:70b', sizeVram: Math.round(40.2 * GiB) },
      { name: 'mixtral:8x7b', sizeVram: Math.round(26.5 * GiB) },
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
    powerDrawW: 274,
    cpuPercent: 28,
    temperature: 68,
    health: 'healthy',
    uptime: '12d 14h',
    loadedModels: [
      { name: 'llama3.1:8b', sizeVram: Math.round(16.2 * GiB) },
      { name: 'mistral:7b', sizeVram: Math.round(14.8 * GiB) },
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
    powerDrawW: 185,
    cpuPercent: 45,
    temperature: 78,
    health: 'healthy',
    uptime: '8d 2h',
    loadedModels: [
      { name: 'llama3.2:3b', sizeVram: Math.round(4.2 * GiB) },
      { name: 'gemma2:9b', sizeVram: Math.round(14.1 * GiB) },
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
    powerDrawW: 92,
    cpuPercent: 12,
    temperature: 45,
    health: 'degraded',
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
    rateLimit: 5000,
    status: 'active',
    allowedModels: ['llama3.1:8b', 'mistral:7b', 'llama3.2:3b'],
    expiresAt: null,
  },
  {
    id: 'key-3',
    name: 'CI/CD Pipeline',
    key: 'sk-ci-1p2q-r3s4-t5u6-v7w8',
    created: '2024-12-10',
    requestsToday: 892,
    requestsThisMonth: 12345,
    rateLimit: 2000,
    status: 'active',
    allowedModels: ['llama3.2:3b', 'gemma2:9b'],
    expiresAt: '2025-03-01',
  },
  {
    id: 'key-4',
    name: 'External Partner',
    key: 'sk-ext-8x9y-z1a2-b3c4-d5e6',
    created: '2024-12-05',
    requestsToday: 0,
    requestsThisMonth: 5678,
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

export const generateRequestsPerMinuteData = (): MetricData[] => {
  const data: MetricData[] = [];
  const now = new Date();
  for (let i = 1440; i >= 0; i -= 5) {
    const time = new Date(now.getTime() - i * 60000);
    const baseValue = 50 + Math.sin(i / 100) * 30;
    const randomVariation = Math.random() * 40 - 20;
    data.push({
      timestamp: time.toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' }),
      value: Math.max(0, Math.floor(baseValue + randomVariation)),
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

export const generateNodeLatencyData = (): MetricData[] => {
  const data: MetricData[] = [];
  const now = new Date();
  for (let i = 1440; i >= 0; i -= 10) {
    const time = new Date(now.getTime() - i * 60000);
    data.push({
      timestamp: time.toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' }),
      value: Math.floor(45 + Math.random() * 30),
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

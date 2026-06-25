import { GPUNode, APIKey, LiveRequest, Savings, CloudProvider, ModelCatalog, RequestEntry, Analytics, ModelFitResponse, ModelCatalogResponse } from '../types';

const BASE = '/admin';

export function getSessionToken(): string {
  return localStorage.getItem('sessionToken') ?? localStorage.getItem('adminToken') ?? '';
}

export function setSessionToken(token: string): void {
  localStorage.setItem('sessionToken', token);
}

export function clearSessionToken(): void {
  localStorage.removeItem('sessionToken');
  localStorage.removeItem('adminToken');
}

// Kept for backward compat — resolves to the active session token.
export function getAdminToken(): string { return getSessionToken(); }
export function clearAdminToken(): void { clearSessionToken(); }

export async function login(username: string, password: string): Promise<{ token: string; expires_at: string }> {
  // Demo mode: no backend on GitHub Pages - validate admin/admin locally
  if (import.meta.env.VITE_FORCE_DEMO === 'true') {
    if (username === 'admin' && password === 'admin') {
      return { token: 'demo-session', expires_at: new Date(Date.now() + 30 * 24 * 60 * 60 * 1000).toISOString() };
    }
    throw new Error('Invalid credentials');
  }
  const r = await fetch('/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  });
  if (!r.ok) throw new Error('Invalid credentials');
  return r.json();
}

export async function logout(): Promise<void> {
  try {
    await fetch(`${BASE}/v1/logout`, {
      method: 'POST',
      headers: authHeaders(),
    });
  } finally {
    clearSessionToken();
  }
}

export async function changePassword(currentPassword: string, newUsername: string, newPassword: string): Promise<void> {
  const r = await apiFetch(`${BASE}/v1/change-password`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ current_password: currentPassword, new_username: newUsername, new_password: newPassword }),
  });
  if (!r.ok) {
    const j = await r.json().catch(() => ({}));
    throw new Error((j as any).error || 'Failed to change password');
  }
}

function authHeaders(): { Authorization: string } {
  return { Authorization: `Bearer ${getSessionToken()}` };
}

async function apiFetch(input: string, init?: RequestInit): Promise<Response> {
  const res = await fetch(input, init);
  if (res.status === 401) {
    clearSessionToken();
    window.location.reload();
  }
  return res;
}

export async function fetchNodes(): Promise<GPUNode[]> {
  const res = await apiFetch(`${BASE}/nodes`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch nodes');
  return res.json();
}

export async function fetchKeys(): Promise<APIKey[]> {
  const res = await apiFetch(`${BASE}/keys`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch keys');
  return res.json();
}

export async function fetchLiveRequests(): Promise<LiveRequest[]> {
  const res = await apiFetch(`${BASE}/requests/live`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch requests');
  return res.json();
}

export async function fetchSummary() {
  const res = await apiFetch(`${BASE}/metrics/summary`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch summary');
  const d = await res.json();
  return {
    activeRequests: d.active_requests ?? 0,
    avgLatency: d.avg_latency ?? 0,
    tokensPerMin: d.tokens_per_min ?? 0,
    coldStarts: d.cold_starts ?? 0,
    queueDepth: d.queue_depth ?? 0,
    nodesOnline: d.nodes_online ?? 0,
    nodesDraining: d.nodes_draining ?? 0,
    totalNodes: d.total_nodes ?? 0,
  };
}

export async function createKey(data: { name: string; rate_limit: number; models: string[]; expires_at: string }): Promise<{ key: string }> {
  const res = await apiFetch(`${BASE}/keys`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to create key');
  return res.json();
}

export async function revokeKey(name: string) {
  const res = await apiFetch(`${BASE}/keys/${name}`, {
    method: 'DELETE',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error('Failed to revoke key');
}

export async function addNode(data: Record<string, unknown>) {
  const res = await apiFetch(`${BASE}/nodes`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to add node');
}

export async function removeNode(name: string) {
  const res = await apiFetch(`${BASE}/nodes/${name}`, {
    method: 'DELETE',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error('Failed to remove node');
}

export async function drainNode(name: string) {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/drain`, {
    method: 'POST',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error('Failed to drain node');
}

export async function undrainNode(name: string) {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/drain`, {
    method: 'DELETE',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error('Failed to undrain node');
}

export async function patchNode(name: string, data: { vram_total_mb?: number; gpu_model?: string }) {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}`, {
    method: 'PATCH',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to patch node');
  return res.json() as Promise<import('../types').GPUNode>;
}

export async function patchKey(name: string, data: { rate_limit?: number; daily_limit?: number; monthly_limit?: number; models?: string[] }) {
  const res = await apiFetch(`${BASE}/keys/${encodeURIComponent(name)}`, {
    method: 'PATCH',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to patch key');
  return res.json();
}

export async function fetchRoutingRules() {
  const res = await apiFetch(`${BASE}/routing/rules`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch routing rules');
  return res.json();
}

export async function addRoutingRule(rule: { id: string; priority: number; condition: string; targetNode: string; strategy: string; enabled: boolean }) {
  const res = await apiFetch(`${BASE}/routing/rules`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(rule),
  });
  if (!res.ok) throw new Error('Failed to add routing rule');
}

export async function removeRoutingRule(id: string) {
  const res = await apiFetch(`${BASE}/routing/rules/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error('Failed to remove routing rule');
}

export async function toggleRoutingRule(id: string) {
  const res = await apiFetch(`${BASE}/routing/rules/${encodeURIComponent(id)}/toggle`, {
    method: 'PUT',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error('Failed to toggle routing rule');
}

export async function setRoutingStrategy(strategy: string) {
  const res = await apiFetch(`${BASE}/routing/strategy`, {
    method: 'PUT',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ strategy }),
  });
  if (!res.ok) throw new Error('Failed to set routing strategy');
}

export async function fetchRoutingStrategy(): Promise<string> {
  const res = await apiFetch(`${BASE}/routing/strategy`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch routing strategy');
  const data = await res.json();
  return data.strategy ?? 'warm-first';
}

export async function fetchSettings() {
  const res = await apiFetch(`${BASE}/settings`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch settings');
  return res.json();
}

export async function fetchSavings(): Promise<Savings> {
  const res = await apiFetch(`${BASE}/metrics/savings`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch savings');
  return res.json();
}

export async function fetchCloudProviders(): Promise<CloudProvider[]> {
  const res = await apiFetch(`${BASE}/cloud/providers`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch cloud providers');
  return res.json();
}

export async function reloadConfig(): Promise<{ reloaded: boolean; config_path: string; auth_keys: number; warmup_enabled: boolean }> {
  const res = await apiFetch(`${BASE}/config/reload`, {
    method: 'POST',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error('Config reload failed');
  return res.json();
}

export async function fetchModels(): Promise<ModelCatalog> {
  const res = await apiFetch(`${BASE}/models`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch models');
  return res.json();
}

export async function fetchRequests(): Promise<RequestEntry[]> {
  const res = await apiFetch(`${BASE}/requests`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch requests');
  return res.json();
}

export async function updateSettings(data: Record<string, unknown>) {
  const res = await apiFetch(`${BASE}/settings`, {
    method: 'PUT',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to update settings');
}

export async function fetchAnalytics(): Promise<Analytics> {
  const res = await apiFetch(`${BASE}/analytics`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch analytics');
  return res.json();
}

export async function pullModel(nodeName: string, model: string): Promise<void> {
  const res = await apiFetch(`${BASE}/v1/nodes/${encodeURIComponent(nodeName)}/pull`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ model }),
  });
  if (!res.ok) throw new Error(`Pull failed: ${res.statusText}`);
}

export async function fetchModelFit(): Promise<ModelFitResponse> {
  const res = await apiFetch(`${BASE}/nodes/model-fit`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch model fit data');
  return res.json();
}

export async function fetchModelCatalog(): Promise<ModelCatalogResponse> {
  const res = await apiFetch(`${BASE}/v1/models/catalog`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch model catalog');
  return res.json();
}

export async function fetchHealth(): Promise<{ version: string; proxy_port: number; status: string }> {
  const res = await fetch('/health');
  if (!res.ok) throw new Error('health check failed');
  return res.json();
}

export interface SystemInfo {
  cpu_cores: number;
  os: string;
  arch: string;
  ram_total_mb: number;
  ram_free_mb: number;
  gpus: Array<{
    name: string;
    url: string;
    vram_total_mb: number;
    vram_free_mb: number;
    vram_source: string;
    temperature_c: number | null;
    power_draw_w: number | null;
    healthy: boolean;
  }>;
}

export async function fetchSystemInfo(): Promise<SystemInfo> {
  const res = await apiFetch(`${BASE}/system-info`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch system info');
  return res.json();
}

export interface HFModel {
  id: string;
  downloads: number;
  likes: number;
  tags: string[];
  lastModified: string;
  pipeline_tag: string;
}

export interface ModelVariantFit {
  tag: string;
  quantization: string;
  vram_est_mb: number;
  size_mb: number;
  fit: 'green' | 'yellow' | 'red' | 'unknown';
  downloaded: boolean;
}

export interface HFRepoDetails {
  id: string;
  downloads: number;
  likes: number;
  tags: string[];
  last_modified: string;
  variants: ModelVariantFit[];
}

export async function searchHFModels(query: string): Promise<HFModel[]> {
  const url = `${BASE}/v1/models/search?q=${encodeURIComponent(query)}`;
  const res = await apiFetch(url, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to search Hugging Face models');
  return res.json();
}

export async function getHFRepoDetails(repoId: string, nodeName?: string, ctxLen?: number): Promise<HFRepoDetails> {
  let url = `${BASE}/v1/models/repo?id=${encodeURIComponent(repoId)}`;
  if (nodeName) url += `&node=${encodeURIComponent(nodeName)}`;
  if (ctxLen) url += `&ctx=${ctxLen}`;
  const res = await apiFetch(url, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch Hugging Face repository details');
  return res.json();
}

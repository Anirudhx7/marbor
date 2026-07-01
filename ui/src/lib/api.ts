import { GPUNode, APIKey, LiveRequest, Savings, CloudProvider, ModelCatalog, RequestEntry, Analytics, ModelFitResponse, ModelCatalogResponse, LoginResponse, SessionData, UserRecord } from '../types';

const BASE = '/admin';

// VITE_FORCE_DEMO is set at build time for the GitHub Pages demo (no backend).
// Vite inlines it, so `if (DEMO)` branches below are dead-code-eliminated and
// tree-shaken out of the live/self-hosted build.
const DEMO = import.meta.env.VITE_FORCE_DEMO === 'true';

// In-memory demo user roster so the Users page is populated and interactive on
// the static demo. Mutations (create/approve/suspend/delete) update this array
// so the demo behaves realistically for the session; it resets on reload.
let demoUsers: UserRecord[] | null = null;
function demoUserStore(): UserRecord[] {
  if (!demoUsers) {
    const now = Date.now();
    const iso = (daysAgo: number) => new Date(now - daysAgo * 86_400_000).toISOString();
    demoUsers = [
      { id: 1, username: 'admin',      email: 'admin@acme.io',  role: 'admin', status: 'active',    api_key_name: 'default',    must_change_password: false, created_at: iso(120), approved_at: iso(120), approved_by: 'system' },
      { id: 2, username: 'dana.rao',   email: 'dana@acme.io',   role: 'user',  status: 'active',    api_key_name: 'dana-key',   must_change_password: false, created_at: iso(40),  approved_at: iso(39),  approved_by: 'admin' },
      { id: 3, username: 'sam.lee',    email: 'sam@acme.io',    role: 'user',  status: 'active',    api_key_name: 'sam-key',    must_change_password: false, created_at: iso(22),  approved_at: iso(21),  approved_by: 'admin' },
      { id: 4, username: 'priya.n',    email: 'priya@acme.io',  role: 'user',  status: 'pending',   api_key_name: '',           must_change_password: false, created_at: iso(2) },
      { id: 5, username: 'marco.b',    email: 'marco@acme.io',  role: 'user',  status: 'pending',   api_key_name: '',           must_change_password: false, created_at: iso(1) },
      { id: 6, username: 'legacy.svc', email: '',               role: 'user',  status: 'suspended', api_key_name: 'legacy-key', must_change_password: false, created_at: iso(200), approved_at: iso(199), approved_by: 'admin' },
    ];
  }
  return demoUsers;
}
function demoDelay<T>(v: T): Promise<T> {
  return new Promise(resolve => setTimeout(() => resolve(v), 150));
}
function demoRandomToken(prefix: string): string {
  return prefix + Math.random().toString(36).slice(2, 12);
}

// --- Session management ---

export function getSessionToken(): string {
  return localStorage.getItem('sessionToken') ?? localStorage.getItem('adminToken') ?? '';
}

export function saveSession(data: LoginResponse): void {
  localStorage.setItem('sessionToken', data.token);
  localStorage.setItem('sessionRole', data.role);
  localStorage.setItem('sessionUsername', data.username);
  localStorage.setItem('sessionMustChangePassword', String(data.must_change_password));
}

export function loadSession(): SessionData | null {
  const token = localStorage.getItem('sessionToken') ?? localStorage.getItem('adminToken') ?? '';
  if (!token) return null;
  return {
    token,
    role: localStorage.getItem('sessionRole') ?? 'admin',
    username: localStorage.getItem('sessionUsername') ?? '',
    mustChangePassword: localStorage.getItem('sessionMustChangePassword') === 'true',
  };
}

export function clearSession(): void {
  localStorage.removeItem('sessionToken');
  localStorage.removeItem('adminToken');
  localStorage.removeItem('sessionRole');
  localStorage.removeItem('sessionUsername');
  localStorage.removeItem('sessionMustChangePassword');
}

// Backward compat aliases
export function setSessionToken(token: string): void { localStorage.setItem('sessionToken', token); }
export function clearSessionToken(): void { clearSession(); }
export function getAdminToken(): string { return getSessionToken(); }
export function clearAdminToken(): void { clearSession(); }

// --- Auth ---

export async function login(username: string, password: string): Promise<LoginResponse> {
  if (import.meta.env.VITE_FORCE_DEMO === 'true') {
    if (username === 'admin' && password === 'admin') {
      return {
        token: 'demo-session',
        role: 'admin',
        username: 'admin',
        must_change_password: false,
        expires_at: new Date(Date.now() + 30 * 24 * 60 * 60 * 1000).toISOString(),
      };
    }
    throw new Error('Invalid credentials');
  }
  const r = await fetch('/admin/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  });
  if (!r.ok) {
    const j = await r.json().catch(() => ({}));
    throw new Error((j as any).error || 'Invalid credentials');
  }
  return r.json();
}

export async function userLogin(username: string, password: string): Promise<LoginResponse> {
  const r = await fetch('/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  });
  if (!r.ok) {
    const j = await r.json().catch(() => ({}));
    throw new Error((j as any).error || 'Invalid credentials');
  }
  return r.json();
}

export async function logout(): Promise<void> {
  try {
    await fetch(`${BASE}/v1/logout`, { method: 'POST', headers: authHeaders() });
  } finally {
    clearSession();
  }
}

export async function changePassword(currentPassword: string, newPassword: string): Promise<{ token?: string; expires_at?: string }> {
  const r = await apiFetch(`/change-password`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ current_password: currentPassword, new_password: newPassword }),
  });
  if (!r.ok) {
    const j = await r.json().catch(() => ({}));
    throw new Error((j as any).error || 'Failed to change password');
  }
  return r.json();
}

// --- User management ---

export async function listUsers(): Promise<UserRecord[]> {
  if (DEMO) return demoDelay(demoUserStore().map(u => ({ ...u })));
  const res = await apiFetch(`${BASE}/v1/users`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch users');
  return res.json();
}

export async function createUser(data: { username: string; email?: string; role?: string }): Promise<{ id: number; username: string; initial_password: string }> {
  if (DEMO) {
    const store = demoUserStore();
    const id = store.reduce((max, u) => Math.max(max, u.id), 0) + 1;
    store.push({
      id, username: data.username, email: data.email ?? '',
      role: (data.role as 'admin' | 'user') ?? 'user', status: 'pending',
      api_key_name: '', must_change_password: true, created_at: new Date().toISOString(),
    });
    return demoDelay({ id, username: data.username, initial_password: demoRandomToken('demo-') });
  }
  const res = await apiFetch(`${BASE}/v1/users`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) {
    const j = await res.json().catch(() => ({}));
    throw new Error((j as any).error || 'Failed to create user');
  }
  return res.json();
}

export async function approveUser(id: number, data: {
  api_key_name?: string;
  create_key?: { name: string; rate_limit_per_hour: number; daily_limit: number; monthly_limit: number; models: string[] };
}): Promise<{ user: UserRecord; api_key_value?: string }> {
  if (DEMO) {
    const u = demoUserStore().find(x => x.id === id);
    if (u) {
      u.status = 'active';
      u.api_key_name = data.api_key_name ?? data.create_key?.name ?? u.api_key_name;
      u.approved_by = 'admin';
      u.approved_at = new Date().toISOString();
    }
    return demoDelay({
      user: (u ?? demoUserStore()[0]),
      ...(data.create_key ? { api_key_value: demoRandomToken('sk-demo-') } : {}),
    });
  }
  const res = await apiFetch(`${BASE}/v1/users/${id}/approve`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) {
    const j = await res.json().catch(() => ({}));
    throw new Error((j as any).error || 'Failed to approve user');
  }
  return res.json();
}

export async function suspendUser(id: number): Promise<void> {
  if (DEMO) {
    const u = demoUserStore().find(x => x.id === id);
    if (u) u.status = 'suspended';
    await demoDelay(null);
    return;
  }
  const res = await apiFetch(`${BASE}/v1/users/${id}/suspend`, {
    method: 'POST',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error('Failed to suspend user');
}

export async function deleteUser(id: number): Promise<void> {
  if (DEMO) {
    demoUsers = demoUserStore().filter(x => x.id !== id);
    await demoDelay(null);
    return;
  }
  const res = await apiFetch(`${BASE}/v1/users/${id}`, {
    method: 'DELETE',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error('Failed to delete user');
}

export async function resetUserPassword(id: number): Promise<{ initial_password: string }> {
  if (DEMO) {
    const u = demoUserStore().find(x => x.id === id);
    if (u) u.must_change_password = true;
    return demoDelay({ initial_password: demoRandomToken('demo-') });
  }
  const res = await apiFetch(`${BASE}/v1/users/${id}/reset-password`, {
    method: 'POST',
    headers: authHeaders(),
  });
  if (!res.ok) {
    const j = await res.json().catch(() => ({}));
    throw new Error((j as any).error || 'Failed to reset password');
  }
  return res.json();
}

export async function getPendingUserCount(): Promise<number> {
  if (DEMO) return demoUserStore().filter(u => u.status === 'pending').length;
  const res = await apiFetch(`${BASE}/v1/users/pending-count`, { headers: authHeaders() });
  if (!res.ok) return 0;
  const d = await res.json();
  return (d as any).count ?? 0;
}

// --- Warmup (per-node) & schedules ---

export interface NodeWarmup { enabled: boolean; models: string[] }
export interface Schedule {
  id: string;
  action: 'warmup' | 'drain' | 'undrain';
  node: string;
  models?: string[];
  at: string;      // "HH:MM" 24h, server-local
  days?: number[]; // 0=Sun..6=Sat; empty = every day
  enabled: boolean;
}

// Demo state so the static demo's Warmup page is populated and interactive.
let demoWarmup: Record<string, NodeWarmup> | null = null;
function demoWarmupStore(): Record<string, NodeWarmup> {
  if (!demoWarmup) demoWarmup = { 'gpu-01': { enabled: true, models: ['llama3.1:8b'] } };
  return demoWarmup;
}
let demoSchedules: Schedule[] | null = null;
function demoScheduleStore(): Schedule[] {
  if (!demoSchedules) demoSchedules = [
    { id: 'sched-demo-1', action: 'warmup', node: 'gpu-01', models: ['llama3.1:8b'], at: '08:30', days: [1, 2, 3, 4, 5], enabled: true },
    { id: 'sched-demo-2', action: 'drain', node: 'gpu-02', at: '19:00', days: [1, 2, 3, 4, 5], enabled: true },
  ];
  return demoSchedules;
}

export async function getNodeWarmup(name: string): Promise<NodeWarmup> {
  if (DEMO) return demoDelay(demoWarmupStore()[name] ?? { enabled: false, models: [] });
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/warmup`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch warmup');
  return res.json();
}

export async function setNodeWarmup(name: string, nw: NodeWarmup): Promise<NodeWarmup> {
  if (DEMO) { demoWarmupStore()[name] = nw; return demoDelay(nw); }
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/warmup`, {
    method: 'PUT', headers: { ...authHeaders(), 'Content-Type': 'application/json' }, body: JSON.stringify(nw),
  });
  if (!res.ok) throw new Error('Failed to save warmup');
  return res.json();
}

export async function listSchedules(): Promise<Schedule[]> {
  if (DEMO) return demoDelay(demoScheduleStore().map(s => ({ ...s })));
  const res = await apiFetch(`${BASE}/schedules`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch schedules');
  return res.json();
}

export async function createSchedule(s: Omit<Schedule, 'id'>): Promise<Schedule> {
  if (DEMO) { const ns = { ...s, id: 'sched-' + Math.random().toString(36).slice(2, 10) } as Schedule; demoScheduleStore().push(ns); return demoDelay(ns); }
  const res = await apiFetch(`${BASE}/schedules`, {
    method: 'POST', headers: { ...authHeaders(), 'Content-Type': 'application/json' }, body: JSON.stringify(s),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to create schedule'); }
  return res.json();
}

export async function deleteSchedule(id: string): Promise<void> {
  if (DEMO) { demoSchedules = demoScheduleStore().filter(s => s.id !== id); return; }
  const res = await apiFetch(`${BASE}/schedules/${encodeURIComponent(id)}`, { method: 'DELETE', headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to delete schedule');
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

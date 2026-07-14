import { GPUNode, APIKey, LiveRequest, Savings, CloudProvider, ModelCatalog, RequestEntry, Analytics, ModelFitResponse, ModelCatalogResponse, LoginResponse, SessionData, UserRecord, PredictiveDecision, CloudBudgetStatus, SystemAuditEntry, ModelConfig } from '../types';

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
  action: 'warmup' | 'unload' | 'drain' | 'undrain';
  node: string;
  models?: string[];
  at: string;      // "HH:MM" 24h, server-local
  days?: number[]; // 0=Sun..6=Sat; empty = every day
  enabled: boolean;
}

// Demo state so the static demo's Warmup page is populated and interactive.
let demoWarmup: Record<string, NodeWarmup> | null = null;
function demoWarmupStore(): Record<string, NodeWarmup> {
  if (!demoWarmup) demoWarmup = {
    'gpu-node-01': { enabled: true,  models: ['deepseek-r1:8b', 'qwen2.5:7b'] },
    'gpu-node-02': { enabled: false, models: [] },
    'gpu-node-03': { enabled: true,  models: ['qwen2.5-coder:14b'] },
    'gpu-node-04': { enabled: false, models: [] },
  };
  return demoWarmup;
}
let demoSchedules: Schedule[] | null = null;
function demoScheduleStore(): Schedule[] {
  if (!demoSchedules) demoSchedules = [
    { id: 'sched-demo-1', action: 'warmup', node: 'gpu-node-01', models: ['deepseek-r1:8b', 'qwen2.5:7b'], at: '08:30', days: [1, 2, 3, 4, 5], enabled: true },
    { id: 'sched-demo-2', action: 'drain',  node: 'gpu-node-03', at: '19:00', days: [1, 2, 3, 4, 5], enabled: true },
    { id: 'sched-demo-3', action: 'warmup', node: 'gpu-node-02', models: ['llama3.3:70b'], at: '09:00', days: [1, 2, 3, 4, 5], enabled: false },
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

export async function getPinned(nodeName: string): Promise<string[]> {
  if (DEMO) return demoDelay(['qwen2.5:3b', 'nomic-embed-text']);
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(nodeName)}/pinned`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch pinned models');
  const j = await res.json();
  return j.models ?? [];
}

export async function setPinned(nodeName: string, models: string[]): Promise<void> {
  if (DEMO) return demoDelay(undefined);
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(nodeName)}/pinned`, {
    method: 'PUT',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ models }),
  });
  if (!res.ok) throw new Error('Failed to set pinned models');
}

// unloadModel evicts a single model from a node's VRAM immediately (keep_alive:0).
export async function unloadModel(nodeName: string, model: string): Promise<void> {
  if (DEMO) return demoDelay(undefined);
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(nodeName)}/unload`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ model }),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to unload model'); }
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

export async function updateSchedule(id: string, patch: Partial<Omit<Schedule, 'id'>>): Promise<Schedule> {
  if (DEMO) {
    const s = demoScheduleStore().find(x => x.id === id);
    if (s) Object.assign(s, patch);
    return demoDelay({ ...(s ?? demoScheduleStore()[0]), ...patch });
  }
  const res = await apiFetch(`${BASE}/schedules/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to update schedule'); }
  return res.json();
}

function authHeaders(): { Authorization: string } {
  return { Authorization: `Bearer ${getSessionToken()}` };
}

let isRedirectingToLogin = false;

async function apiFetch(input: string, init?: RequestInit): Promise<Response> {
  const res = await fetch(input, init);
  if (res.status === 401) {
    clearSessionToken();
    if (!isRedirectingToLogin) {
      isRedirectingToLogin = true;
      window.location.reload();
    }
  }
  return res;
}

export async function fetchNodes(): Promise<GPUNode[]> {
  if (DEMO) return demoDelay([
    { id: 'gpu-node-01', name: 'gpu-node-01', gpuModel: 'NVIDIA A100 80GB',     port: 11434, vramTotalMB: 81920, vramUsedMB: 14336, vramSource: 'nvidia', powerDrawW: 280, cpuPercent: 18, temperature: 52, health: 'healthy',  runtime: 'ollama', draining: false, prewarmDisabled: false, pendingPrewarmMB: 0,    uptime: '12d 6h', loadedModels: [{ name: 'deepseek-r1:8b', sizeVram: 8192 }, { name: 'qwen2.5:7b', sizeVram: 6144 }], healthHistory: [1,1,1,1,1,1,1,1,1,1] },
    { id: 'gpu-node-02', name: 'gpu-node-02', gpuModel: 'NVIDIA A100 80GB',     port: 11434, vramTotalMB: 81920, vramUsedMB: 0,     vramSource: 'nvidia', powerDrawW: 210, cpuPercent: 4,  temperature: 44, health: 'healthy',  runtime: 'ollama', draining: false, prewarmDisabled: false, pendingPrewarmMB: 6144, uptime: '12d 6h', loadedModels: [],                                                                                                    healthHistory: [1,1,1,1,1,1,1,1,1,1] },
    { id: 'gpu-node-03', name: 'gpu-node-03', gpuModel: 'NVIDIA RTX 4090 24GB', port: 11434, vramTotalMB: 24576, vramUsedMB: 9216, vramSource: 'nvidia', powerDrawW: 195, cpuPercent: 22, temperature: 61, health: 'healthy',  runtime: 'ollama', draining: false, prewarmDisabled: true,  pendingPrewarmMB: 0,    uptime: '5d 3h',  loadedModels: [{ name: 'qwen2.5-coder:14b', sizeVram: 9216 }],                                                          healthHistory: [1,1,1,1,1,1,0,1,1,1] },
    { id: 'gpu-node-04', name: 'gpu-node-04', gpuModel: 'NVIDIA RTX 3090 24GB', port: 11434, vramTotalMB: 24576, vramUsedMB: 0,    vramSource: 'nvidia', powerDrawW: 0,   cpuPercent: 0,  temperature: null, health: 'down', runtime: 'ollama', draining: false, prewarmDisabled: false, pendingPrewarmMB: 0,    uptime: 'N/A',    loadedModels: [],                                                                                                    healthHistory: [1,1,0,0,0,1,0,0,0,0] },
  ]);
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

export async function createKey(data: { name: string; rate_limit: number; models: string[]; expires_at: string; dailyUsdCap?: number; monthlyUsdCap?: number }): Promise<{ key: string }> {
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

export async function setNodePrewarm(name: string, disabled: boolean) {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/prewarm`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ disabled }),
  });
  if (!res.ok) throw new Error('Failed to toggle node prewarm');
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

export async function patchKey(name: string, data: { rate_limit?: number; daily_limit?: number; monthly_limit?: number; daily_usd_cap?: number; monthly_usd_cap?: number; models?: string[] }) {
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
  if (DEMO) {
    const stored = localStorage.getItem('demo_settings');
    if (stored) {
      try {
        return JSON.parse(stored);
      } catch {}
    }
    return {
      proxy: { port: 11434, log_level: 'info' },
      auth: { enabled: true },
      routing: { poll_interval_ms: 2000, allow_management_endpoints: false },
      metrics: { enabled: true, port: 9090 },
      litellm: { enabled: false, url: '' },
      huggingface: { token: '' },
      timezone: 'UTC',
      cloud_budget: { daily_usd_cap: 100, monthly_usd_cap: 1000, soft_budget_pct: 0.8 },
      hide_demo_banner: false,
      hide_budget_banner: false,
    };
  }
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

export interface AuditLogFilters {
  model?: string;
  key?: string;
  node?: string;
  status?: 'success' | 'client_error' | 'server_error';
  cloud?: boolean;
  since?: string; // RFC3339
  until?: string; // RFC3339
  limit?: number;
}

interface AuditLogEntry {
  time: string;
  request_id: string;
  key_name: string;
  model: string;
  node: string;
  status: string;
  latency_ms: number;
  cloud: boolean;
  cloud_model?: string;
}

// fetchAuditLog queries the server-side filterable /admin/audit endpoint
// (backed by SQLite audit_log, indexed on key_name/model/node/ts) so the
// Requests page can filter without pulling every row and matching client-side.
export async function fetchAuditLog(filters: AuditLogFilters = {}): Promise<RequestEntry[]> {
  const params = new URLSearchParams();
  if (filters.model) params.set('model', filters.model);
  if (filters.key) params.set('key', filters.key);
  if (filters.node) params.set('node', filters.node);
  if (filters.status) params.set('status', filters.status);
  if (filters.cloud !== undefined) params.set('cloud', String(filters.cloud));
  if (filters.since) params.set('since', filters.since);
  if (filters.until) params.set('until', filters.until);
  params.set('limit', String(filters.limit ?? 50));

  const res = await apiFetch(`${BASE}/audit?${params.toString()}`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch audit log');
  const data = await res.json();
  const entries: AuditLogEntry[] = data.entries ?? [];
  return entries.map((e) => ({
    id: e.request_id,
    time: e.time,
    key_name: e.key_name,
    model: e.cloud && e.cloud_model ? e.cloud_model : e.model,
    node: e.node,
    status: Number(e.status) || 0,
    latency_ms: e.latency_ms,
    cloud: e.cloud,
  }));
}

export async function updateSettings(data: Record<string, unknown>) {
  if (DEMO) {
    localStorage.setItem('demo_settings', JSON.stringify(data));
    return;
  }
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

// fetchModelConfig returns the configured default parameter profile for a
// (model, node) pair, or null if none is configured (backend returns 404 in
// that case — R1: the UI must show "not set", never fabricate a value).
// Both model and node are required by the backend.
export async function fetchModelConfig(model: string, node: string): Promise<ModelConfig | null> {
  const res = await apiFetch(`${BASE}/model-config?model=${encodeURIComponent(model)}&node=${encodeURIComponent(node)}`, { headers: authHeaders() });
  if (res.status === 404) return null;
  if (!res.ok) throw new Error(`Failed to fetch model config: ${res.statusText}`);
  return res.json();
}

// saveModelConfig upserts a profile for the (model, node) pair named in the
// body — cfg.model and cfg.node are both required by the backend.
export async function saveModelConfig(cfg: ModelConfig): Promise<ModelConfig> {
  const res = await apiFetch(`${BASE}/model-config`, {
    method: 'PUT',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(cfg),
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || `Failed to save model config: ${res.statusText}`);
  }
  return res.json();
}

// deleteModelConfig resets a single (model, node) pair to backend defaults.
export async function deleteModelConfig(model: string, node: string): Promise<void> {
  const res = await apiFetch(`${BASE}/model-config?model=${encodeURIComponent(model)}&node=${encodeURIComponent(node)}`, {
    method: 'DELETE',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error(`Failed to reset model config: ${res.statusText}`);
}

export async function fetchAllModelConfigs(): Promise<ModelConfig[]> {
  const res = await apiFetch(`${BASE}/model-configs`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch model configs');
  const data = await res.json();
  return data.configs ?? [];
}

// fetchModelConfigCapabilities returns, for each known runtime (ollama, vllm,
// tgi, llamacpp), the exact ModelConfig JSON field names that actually take
// effect when injected for that runtime. This is the single source of truth
// the UI uses to decide which fields to render/enable per node — it must
// never hand-duplicate this list from memory, since that's exactly what
// drifted out of sync with the backend before this endpoint existed.
export async function fetchModelConfigCapabilities(): Promise<Record<string, string[]>> {
  const res = await apiFetch(`${BASE}/model-config/capabilities`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch model config capabilities');
  return res.json();
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

export async function fetchPredictiveDecisions(): Promise<PredictiveDecision[]> {
  if (DEMO) {
    const now = Date.now();
    return demoDelay([
      { timestamp: new Date(now - 2 * 60_000).toISOString(), predicted_model: 'qwen2.5:7b',        trigger_model: 'deepseek-r1:8b', node: 'gpu-node-01', was_already_warm: false, warmup_triggered: true,  transition_count: 14, hour: new Date(now - 2 * 60_000).getHours() },
      { timestamp: new Date(now - 9 * 60_000).toISOString(), predicted_model: 'qwen2.5-coder:14b', trigger_model: 'llama3.3:8b',    node: 'gpu-node-02', was_already_warm: false, warmup_triggered: true,  transition_count: 9,  hour: new Date(now - 9 * 60_000).getHours() },
      { timestamp: new Date(now - 21 * 60_000).toISOString(), predicted_model: 'deepseek-r1:8b',   trigger_model: 'qwen2.5:7b',     node: 'gpu-node-01', was_already_warm: true,  warmup_triggered: false, transition_count: 14, hour: new Date(now - 21 * 60_000).getHours() },
    ]);
  }
  const res = await apiFetch(`${BASE}/predictive/decisions`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch predictive decisions');
  const j = await res.json();
  return j.decisions ?? [];
}

export async function fetchCloudBudgetStatus(): Promise<CloudBudgetStatus> {
  if (DEMO) {
    return demoDelay({
      softBudgetPct: 0.8,
      global: { dailySpent: 4.2, dailyCap: 25, dailyPct: 4.2 / 25, monthlySpent: 61.5, monthlyCap: 500, monthlyPct: 61.5 / 500 },
      perKey: [
        { name: 'team-shared', dailySpent: 3.1, dailyCap: 5, dailyPct: 3.1 / 5, monthlySpent: 42.0, monthlyCap: 50, monthlyPct: 42.0 / 50 },
      ],
    });
  }
  const res = await apiFetch(`${BASE}/cloud-budget-status`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch cloud budget status');
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
  server_time?: string;
  timezone?: string;
}

export async function fetchSystemInfo(): Promise<SystemInfo> {
  if (DEMO) {
    const now = new Date();
    const pad = (n: number) => n.toString().padStart(2, '0');
    const serverTime = `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())} ${pad(now.getHours())}:${pad(now.getMinutes())}:${pad(now.getSeconds())}`;
    const tzMatch = new Intl.DateTimeFormat('en-US', { timeZoneName: 'short' }).formatToParts(now).find(p => p.type === 'timeZoneName');
    return demoDelay({
      cpu_cores: 16,
      os: 'linux',
      arch: 'amd64',
      ram_total_mb: 65536,
      ram_free_mb: 24576,
      gpus: [],
      server_time: serverTime,
      timezone: tzMatch?.value ?? 'UTC',
    });
  }
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

export async function fetchSystemAudit(limit: number = 100): Promise<SystemAuditEntry[]> {
  if (DEMO) {
    const now = Date.now();
    const iso = (minsAgo: number) => new Date(now - minsAgo * 60_000).toISOString();
    return [
      { time: iso(2), username: 'admin', action: 'update_settings', target: 'global', details: 'Timezone: UTC, AuthEnabled: true, DailyCap: 150.00', source_ip: '192.168.1.5' },
      { time: iso(15), username: 'admin', action: 'add_node', target: 'gpu-node-03', details: 'URL: http://192.168.1.103:11434, Runtime: ollama, VRAM: 24576MB', source_ip: '192.168.1.5' },
      { time: iso(45), username: 'admin', action: 'add_routing_rule', target: 'rule-deepseek-r1', details: 'Condition: model == "deepseek-r1", Target: gpu-node-02, Priority: 10, Enabled: true', source_ip: '192.168.1.5' },
      { time: iso(120), username: 'admin', action: 'set_pinned_models', target: 'gpu-node-01', details: 'Models: ["llama3:8b", "mistral:7b"]', source_ip: '192.168.1.5' },
      { time: iso(180), username: 'admin', action: 'add_key', target: 'marketing-team', details: 'RateLimit: 50, DailyLimit: 500, MonthlyLimit: 10000, DailyUsdCap: 50.00, MonthlyUsdCap: 200.00, Models: []', source_ip: '192.168.1.5' },
    ];
  }
  const res = await apiFetch(`${BASE}/system-audit?limit=${limit}`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch system audit logs');
  return res.json();
}

export async function fetchWarmupStatus(): Promise<{ enabled: boolean; interval_ms: number; keep_alive: string; models: any[]; predictive_engine_enabled: boolean }> {
  if (DEMO) {
    return {
      enabled: true,
      interval_ms: 300000,
      keep_alive: "10m",
      models: [],
      predictive_engine_enabled: true,
    };
  }
  const res = await apiFetch(`${BASE}/warmup`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch warmup status');
  return res.json();
}

export async function setPredictiveEngine(enabled: boolean): Promise<{ predictive_engine_enabled: boolean }> {
  if (DEMO) {
    return { predictive_engine_enabled: enabled };
  }
  const res = await apiFetch(`${BASE}/warmup/predictive`, {
    method: 'PUT',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled }),
  });
  if (!res.ok) throw new Error('Failed to set predictive engine status');
  return res.json();
}

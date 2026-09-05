import { GPUNode, APIKey, LiveRequest, Savings, CloudProvider, CloudProviderInput, ModelCatalog, RequestEntry, Analytics, ModelFitResponse, ModelCatalogResponse, LoginResponse, SessionData, UserRecord, PredictiveDecision, CloudBudgetStatus, SystemAuditEntry, ModelConfig, LocalModel, BenchmarkRun, BackupFileInfo, SpillCounterRow, RoutingDecision } from '../types';
import { mockCloudProviders, mockSavings } from './mockData';

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
//
// The session token itself lives only in a server-set httpOnly cookie now -
// never in localStorage, never readable by JS. What's persisted here is
// non-secret UI display state (role/username/must-change-password) so a page
// reload can render the dashboard optimistically instead of flashing the
// login screen; the cookie (sent automatically by the browser) is what
// actually authenticates each request, and apiFetch's 401 handling bounces
// back to Login if it turns out to be missing/expired.

export function saveSession(data: LoginResponse): void {
  localStorage.setItem('sessionRole', data.role);
  localStorage.setItem('sessionUsername', data.username);
  localStorage.setItem('sessionMustChangePassword', String(data.must_change_password));
}

export function loadSession(): SessionData | null {
  const username = localStorage.getItem('sessionUsername');
  if (!username) return null;
  return {
    role: localStorage.getItem('sessionRole') ?? 'admin',
    username,
    mustChangePassword: localStorage.getItem('sessionMustChangePassword') === 'true',
  };
}

export function clearSession(): void {
  localStorage.removeItem('sessionRole');
  localStorage.removeItem('sessionUsername');
  localStorage.removeItem('sessionMustChangePassword');
}

// Called from ForceChangePassword's "Skip for now" - reissues the session
// server-side with must_change_password cleared for THIS session only (the
// server never updates the user's row), so the dashboard's normal API calls
// stop getting rejected by adminAuth/sessionAuth's forced-change gate. The
// next fresh login still forces the prompt again, since the user's own
// must_change_password flag was never touched.
export async function skipPasswordChangeThisSession(): Promise<void> {
  const r = await apiFetch('/skip-password-change', { method: 'POST' });
  if (!r.ok) {
    let message = 'Failed to skip password change';
    try {
      const body = await r.json();
      if (r.status === 403 && body?.error === 'skip_limit_reached') {
        message = 'Skip limit reached - you must set a new password to continue.';
      } else if (body?.error) {
        message = body.error;
      }
    } catch {
      // ignore parse failure, use default message
    }
    throw new Error(message);
  }
  localStorage.setItem('sessionMustChangePassword', 'false');
}

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
    credentials: 'include',
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
    credentials: 'include',
  });
  if (!r.ok) {
    const j = await r.json().catch(() => ({}));
    throw new Error((j as any).error || 'Invalid credentials');
  }
  return r.json();
}

export async function logout(): Promise<void> {
  try {
    await fetch(`${BASE}/v1/logout`, { method: 'POST', headers: authHeaders(), credentials: 'include' });
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

export async function patchUser(id: number, data: { email?: string; role?: 'admin' | 'user' }): Promise<UserRecord> {
  if (DEMO) {
    const u = demoUserStore().find(x => x.id === id);
    if (!u) throw new Error('user not found');
    if (data.email !== undefined) u.email = data.email;
    if (data.role !== undefined) u.role = data.role;
    return demoDelay({ ...u });
  }
  const res = await apiFetch(`${BASE}/v1/users/${id}`, {
    method: 'PATCH',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) {
    const j = await res.json().catch(() => ({}));
    throw new Error((j as any).error || 'Failed to update user');
  }
  return res.json();
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
  last_run_at?: string;  // RFC3339 UTC, absent if never fired since boot
  last_status?: 'ok' | 'error';
  last_error?: string;
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
    { id: 'sched-demo-1', action: 'warmup', node: 'gpu-node-01', models: ['deepseek-r1:8b', 'qwen2.5:7b'], at: '08:30', days: [1, 2, 3, 4, 5], enabled: true, last_run_at: new Date(Date.now() - 16 * 60 * 60 * 1000).toISOString(), last_status: 'ok' },
    { id: 'sched-demo-2', action: 'drain',  node: 'gpu-node-03', at: '19:00', days: [1, 2, 3, 4, 5], enabled: true, last_run_at: new Date(Date.now() - 5 * 60 * 60 * 1000).toISOString(), last_status: 'ok' },
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
  if (DEMO) return demoDelay(['qwen2.5']);
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

// No-op now that the session lives in an httpOnly cookie sent automatically
// by the browser - kept so the many `{ ...authHeaders(), ... }` call sites
// below don't all need touching.
function authHeaders(): Record<string, string> {
  return {};
}

let isRedirectingToLogin = false;

export async function apiFetch(input: string, init?: RequestInit): Promise<Response> {
  const res = await fetch(input, { ...init, credentials: 'include' });
  if (res.status === 401) {
    clearSession();
    if (!isRedirectingToLogin) {
      isRedirectingToLogin = true;
      window.location.reload();
    }
  }
  return res;
}

export async function fetchNodes(): Promise<GPUNode[]> {
  if (DEMO) return demoDelay([
    { id: 'gpu-node-01', name: 'gpu-node-01', host: '10.0.0.11', gpuModel: 'NVIDIA A100 80GB',     port: 11434, vramTotalMB: 81920, vramUsedMB: 14336, vramSource: 'nvidia', powerDrawW: 280, cpuPercent: 18, temperature: 52, health: 'healthy',  runtime: 'ollama', draining: false, activeConns: 2, maxInFlight: 0, prewarmDisabled: false, pendingPrewarmMB: 0,    uptime: '12d 6h', loadedModels: [{ name: 'deepseek-r1:8b', sizeVram: 8192 }, { name: 'qwen2.5:7b', sizeVram: 6144 }], healthHistory: [1,1,1,1,1,1,1,1,1,1] },
    { id: 'gpu-node-02', name: 'gpu-node-02', host: '10.0.0.12', gpuModel: 'NVIDIA A100 80GB',     port: 11434, vramTotalMB: 81920, vramUsedMB: 0,     vramSource: 'nvidia', powerDrawW: 210, cpuPercent: 4,  temperature: 44, health: 'healthy',  runtime: 'ollama', draining: false, activeConns: 0, maxInFlight: 0, prewarmDisabled: false, pendingPrewarmMB: 6144, uptime: '12d 6h', loadedModels: [],                                                                                                    healthHistory: [1,1,1,1,1,1,1,1,1,1] },
    { id: 'gpu-node-03', name: 'gpu-node-03', host: '10.0.0.13', gpuModel: 'NVIDIA RTX 4090 24GB', port: 11434, vramTotalMB: 24576, vramUsedMB: 9216, vramSource: 'nvidia', powerDrawW: 195, cpuPercent: 22, temperature: 61, health: 'healthy',  runtime: 'ollama', draining: false, activeConns: 1, maxInFlight: 4, prewarmDisabled: true,  pendingPrewarmMB: 0,    uptime: '5d 3h',  loadedModels: [{ name: 'qwen2.5-coder:14b', sizeVram: 9216 }],                                                          healthHistory: [1,1,1,1,1,1,0,1,1,1] },
    { id: 'gpu-node-04', name: 'gpu-node-04', host: '10.0.0.14', gpuModel: 'NVIDIA RTX 3090 24GB', port: 11434, vramTotalMB: 24576, vramUsedMB: 0,    vramSource: 'nvidia', powerDrawW: 0,   cpuPercent: 0,  temperature: null, health: 'down', runtime: 'ollama', draining: false, activeConns: 0, maxInFlight: 0, prewarmDisabled: false, pendingPrewarmMB: 0,    uptime: 'N/A',    loadedModels: [],                                                                                                    healthHistory: [1,1,0,0,0,1,0,0,0,0] },
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
    warmHitRatio: d.warm_hit_ratio ?? 0,
  };
}

export async function createKey(data: { name: string; rate_limit: number; models: string[]; expires_at: string; dailyUsdCap?: number; monthlyUsdCap?: number; localOnly?: boolean; allowLocalDegradation?: boolean }): Promise<{ key: string }> {
  const res = await apiFetch(`${BASE}/keys`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to create key');
  return res.json();
}

export async function revokeKey(name: string) {
  const res = await apiFetch(`${BASE}/keys/${encodeURIComponent(name)}`, {
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
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to add node'); }
}

export async function removeNode(name: string) {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}`, {
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

export async function patchNode(name: string, data: { vram_total_mb?: number; gpu_model?: string; runtime?: string; url?: string; gpu_indices?: number[]; max_in_flight?: number; tls_fingerprint?: string | null; parallelism_type?: string | null; parallelism_width?: number | null; vram_overrides?: Record<string, number> }) {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}`, {
    method: 'PATCH',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to patch node'); }
  return res.json() as Promise<import('../types').GPUNode>;
}

export async function patchKey(name: string, data: { rate_limit?: number; daily_limit?: number; monthly_limit?: number; daily_usd_cap?: number; monthly_usd_cap?: number; models?: string[]; expires_at?: string; local_only?: boolean; allow_local_degradation?: boolean }) {
  const res = await apiFetch(`${BASE}/keys/${encodeURIComponent(name)}`, {
    method: 'PATCH',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to patch key');
  return res.json();
}

export async function getSpillCounters(): Promise<SpillCounterRow[]> {
  const res = await apiFetch(`${BASE}/spill`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch spill counters');
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
  if (DEMO) return demoDelay(mockSavings);
  const res = await apiFetch(`${BASE}/metrics/savings`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch savings');
  return res.json();
}

// In-memory demo provider store so Settings > Cloud Providers is fully
// interactive on the static demo (add/edit/delete/reorder); resets on reload.
let demoCloudProviders: CloudProvider[] | null = null;
function demoProviderStore(): CloudProvider[] {
  if (!demoCloudProviders) {
    demoCloudProviders = mockCloudProviders.map(p => ({ ...p }));
  }
  return demoCloudProviders;
}

export async function fetchCloudProviders(): Promise<CloudProvider[]> {
  if (DEMO) return demoDelay(demoProviderStore().map(p => ({ ...p })));
  const res = await apiFetch(`${BASE}/cloud/providers`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch cloud providers');
  return res.json();
}

export async function addCloudProvider(data: CloudProviderInput): Promise<void> {
  if (DEMO) {
    demoProviderStore().push({ ...(data as CloudProvider) });
    return demoDelay(undefined);
  }
  const res = await apiFetch(`${BASE}/cloud/providers`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to add cloud provider'); }
}

export async function updateCloudProvider(name: string, data: CloudProviderInput): Promise<void> {
  if (DEMO) {
    const s = demoProviderStore();
    const i = s.findIndex(p => p.name === name);
    if (i >= 0) s[i] = { ...s[i], name: (data as CloudProvider).name || name, ...(data as Partial<CloudProvider>) };
    return demoDelay(undefined);
  }
  const res = await apiFetch(`${BASE}/cloud/providers/${encodeURIComponent(name)}`, {
    method: 'PUT',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to update cloud provider'); }
}

export async function testCloudProvider(provider: string, base_url: string, api_key: string): Promise<void> {
  if (DEMO) return demoDelay(undefined);
  const res = await apiFetch(`${BASE}/cloud/providers/test`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ provider, base_url, api_key }),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Could not verify API key'); }
}

export async function deleteCloudProvider(name: string): Promise<void> {
  if (DEMO) {
    const s = demoProviderStore();
    const i = s.findIndex(p => p.name === name);
    if (i >= 0) s.splice(i, 1);
    return demoDelay(undefined);
  }
  const res = await apiFetch(`${BASE}/cloud/providers/${encodeURIComponent(name)}`, {
    method: 'DELETE',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error('Failed to delete cloud provider');
}

export async function reorderCloudProviders(order: string[]): Promise<void> {
  if (DEMO) {
    const s = demoProviderStore();
    const byName = new Map(s.map(p => [p.name, p] as const));
    const next = order.map(n => byName.get(n)).filter((p): p is CloudProvider => !!p);
    s.forEach(p => { if (!order.includes(p.name)) next.push(p); });
    demoCloudProviders = next;
    return demoDelay(undefined);
  }
  const res = await apiFetch(`${BASE}/cloud/providers/reorder`, {
    method: 'PUT',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ order }),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to reorder cloud providers'); }
}

// reloadFromStore re-syncs live nodes/API keys/cloud providers from the
// database without restarting (no config.yaml to reload anymore - 2026-07
// elimination). Settings not covered by this (Docker/Webhook wiring,
// listen ports/addresses) take effect on next restart.
export async function reloadFromStore(): Promise<{ reloaded: boolean; auth_keys: number; nodes_added: number; nodes_removed: number; cloud_providers: number }> {
  const res = await apiFetch(`${BASE}/config/reload`, {
    method: 'POST',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error('Reload failed');
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
  routing_reason?: string;
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
    routingReason: e.routing_reason || undefined,
  }));
}

// fetchRequestExplain queries GET /admin/requests/{id}/explain - the
// full routing decision for one request, fetched lazily (not part of the
// audit-log list payload) since it carries the full score breakdown. Throws
// on 404 (no decision recorded for this id, e.g. it predates this feature).
export async function fetchRequestExplain(id: string): Promise<RoutingDecision> {
  const res = await apiFetch(`${BASE}/requests/${encodeURIComponent(id)}/explain`, { headers: authHeaders() });
  if (!res.ok) throw new Error(res.status === 404 ? 'No routing decision recorded for this request' : 'Failed to fetch routing explanation');
  return res.json();
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

export function analyticsExportUrl(type: 'hourly' | 'models'): string {
  return `${BASE}/analytics/export?format=csv&type=${type}`;
}

// triggerBackupNow requests an on-demand marbor.db backup (POST, unlike the GET
// analyticsExportUrl above - server-side handleBackupNow needs a verb that
// isn't cacheable/prefetchable) and pushes the streamed file into the
// browser's normal download flow via a throwaway object URL + <a download>.
export async function triggerBackupNow(): Promise<void> {
  if (DEMO) throw new Error('Backup download is not available in demo mode.');
  const res = await apiFetch(`${BASE}/backup`, { method: 'POST', headers: authHeaders() });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Backup failed'); }
  const blob = await res.blob();
  const disposition = res.headers.get('Content-Disposition') || '';
  const match = /filename="?([^"]+)"?/.exec(disposition);
  const filename = match ? match[1] : 'marbor-backup.db';
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

// fetchBackupList lists scheduled backup files already sitting in the
// configured target directory, so the restore picker doesn't require the
// operator to know the naming scheme or type a path by hand.
export async function fetchBackupList(): Promise<BackupFileInfo[]> {
  if (DEMO) {
    const now = Date.now();
    return demoDelay([
      { name: 'marbor-backup-20260730-030000.db', size_bytes: 4_812_288, modified_at: new Date(now - 9 * 60 * 60 * 1000).toISOString() },
      { name: 'marbor-backup-20260729-030000.db', size_bytes: 4_795_904, modified_at: new Date(now - 33 * 60 * 60 * 1000).toISOString() },
      { name: 'marbor-backup-20260728-030000.db', size_bytes: 4_780_032, modified_at: new Date(now - 57 * 60 * 60 * 1000).toISOString() },
    ]);
  }
  const res = await apiFetch(`${BASE}/backup/list`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to list backups');
  const data = await res.json();
  return data.backups ?? [];
}

// restoreBackup triggers a one-click restore from an already-existing
// scheduled backup file: marbor validates it, swaps marbor.db for it, and
// exits so the process supervisor (systemd/Docker/Kubernetes) restarts it
// with the restored database - see docs/backup.md for the supervisor
// requirement. The connection may drop before a response arrives since the
// marbor shuts down shortly after responding; callers should treat a network
// error here as "restore likely proceeding" rather than a hard failure.
export async function restoreBackup(filename: string): Promise<void> {
  if (DEMO) throw new Error('Restore is not available in demo mode.');
  const res = await apiFetch(`${BASE}/backup/restore`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ filename }),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Restore failed'); }
}

// uploadBackup sends an arbitrary local .db file (picked via a plain
// <input type="file"> - works the same in Chrome/Firefox/Safari/Edge on
// Linux, Windows, and macOS, no OS-specific code needed) to the server,
// which validates it's a genuine SQLite database and saves it into the same
// target directory scheduled/manual backups use, under a fresh
// marbor-backup-<timestamp>.db name. On success it returns that name so the
// caller can select it in the restore picker without a second list fetch.
// If the upload is byte-for-byte identical to a backup already in the pool,
// the server reuses that existing file instead of adding a duplicate -
// `duplicate` tells the caller which happened so it can message it clearly.
export async function uploadBackup(file: File): Promise<{ filename: string; duplicate: boolean }> {
  if (DEMO) throw new Error('Uploading a backup is not available in demo mode.');
  const form = new FormData();
  form.append('file', file);
  const res = await apiFetch(`${BASE}/backup/upload`, {
    method: 'POST',
    headers: authHeaders(),
    body: form,
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Upload failed'); }
  const data = await res.json();
  return { filename: data.filename, duplicate: !!data.duplicate };
}

// normalizePullTag catches the most common way a pull request is malformed
// before it ever reaches the marbor: pasting a bare Hugging Face repo id
// (e.g. "unsloth/gemma-4-26B-A4B-it-GGUF", copied straight off a HF model
// page) into the free-text "Pull Model from Registry" field. Ollama only
// resolves a Hugging Face-hosted GGUF repo when the tag is explicitly
// prefixed "hf.co/" - ModelAdvisor.tsx's own search-and-select flow already
// builds tags this way; this closes the same gap for manual entry. Only
// triggers on the "-gguf" suffix convention (a strong, narrow signal) so a
// legitimate bare "namespace/model" Ollama-library tag is never mangled.
// Exported so pullProgress.ts's startPull() applies the same normalization -
// pulls now go through the async job-tracked path, not this module.
export function normalizePullTag(model: string): string {
  const trimmed = model.trim();
  if (trimmed.startsWith('hf.co/') || trimmed.startsWith('http://') || trimmed.startsWith('https://')) {
    return trimmed;
  }
  if (/^[\w.-]+\/[\w.-]+-gguf(:.+)?$/i.test(trimmed)) {
    return `hf.co/${trimmed}`;
  }
  return trimmed;
}

// fetchModelConfig returns the configured default parameter profile for a
// (model, node) pair, or null if none is configured (backend returns 404 in
// that case - the UI must show "not set", never fabricate a value).
// Both model and node are required by the backend.
export async function fetchModelConfig(model: string, node: string): Promise<ModelConfig | null> {
  const res = await apiFetch(`${BASE}/model-config?model=${encodeURIComponent(model)}&node=${encodeURIComponent(node)}`, { headers: authHeaders() });
  if (res.status === 404) return null;
  if (!res.ok) throw new Error(`Failed to fetch model config: ${res.statusText}`);
  return res.json();
}

// saveModelConfig upserts a profile for the (model, node) pair named in the
// body - cfg.model and cfg.node are both required by the backend.
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
// tgi, llamacpp, mlx), the exact ModelConfig JSON field names that actually take
// effect when injected for that runtime. This is the single source of truth
// the UI uses to decide which fields to render/enable per node - it must
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
  createdAt?: string;
}

// ContextFeasibility is the context-length feasibility advice for one
// model variant at one requested context length. confidence is always
// populated ("derived" or "estimated") so the UI can never present a rough
// linear guess as if it were a real architecture-derived calculation.
export interface ContextFeasibility {
  confidence: 'derived' | 'estimated';
  requested_ctx: number;
  // declared_max_context is the model's own trained maximum context length,
  // Known only when confidence === 'derived' - absent otherwise, never
  // guessed.
  declared_max_context?: number;
  exceeds_declared_max?: boolean;
  kv_cache_est_mb?: number;
  limiting_factor?: 'weights' | 'kv_cache';
  // recommended_ctx is only ever present when confidence === 'derived' - no
  // recommendation is ever built on a rough estimate.
  recommended_ctx?: number;
  runtime_caveat?: string;
}

export interface ModelVariantFit {
  tag: string;
  quantization: string;
  vram_est_mb: number;
  size_mb: number;
  fit: 'green' | 'yellow' | 'red' | 'unknown';
  disk_fit: 'ok' | 'insufficient' | 'unknown';
  downloaded: boolean;
  context_feasibility: ContextFeasibility;
}

export interface HFRepoDetails {
  id: string;
  downloads: number;
  likes: number;
  tags: string[];
  last_modified: string;
  variants: ModelVariantFit[];
  disk_free_gb: number;
  disk_total_gb: number;
  disk_known: boolean;
  // docker_deployed: disk_free_gb/disk_total_gb are always this node's
  // agent's own *host* filesystem reading - when the runtime itself is
  // Docker-controlled, its actual model storage can live on a separate,
  // differently-sized container volume the host reading knows nothing
  // about. The marbor's pull gate already checks the container's real number
  // before actually pulling; this flag lets the UI caveat the figure it's
  // displaying instead of silently showing a number that may not match.
  docker_deployed?: boolean;
}

export interface HFSearchFilters {
  runtime?: string;
  sort?: 'downloads' | 'likes' | 'newest' | 'oldest';
  minDownloads?: number;
  minLikes?: number;
  createdAfter?: string; // YYYY-MM-DD
  createdBefore?: string; // YYYY-MM-DD
}

export async function searchHFModels(query: string, filters?: HFSearchFilters): Promise<HFModel[]> {
  const params = new URLSearchParams();
  if (query) params.set('q', query);
  if (filters?.runtime) params.set('runtime', filters.runtime);
  if (filters?.sort) params.set('sort', filters.sort);
  if (filters?.minDownloads) params.set('min_downloads', String(filters.minDownloads));
  if (filters?.minLikes) params.set('min_likes', String(filters.minLikes));
  if (filters?.createdAfter) params.set('created_after', filters.createdAfter);
  if (filters?.createdBefore) params.set('created_before', filters.createdBefore);
  const url = `${BASE}/v1/models/search?${params.toString()}`;
  const res = await apiFetch(url, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to search Hugging Face models');
  return res.json();
}

export async function getHFRepoDetails(repoId: string, nodeName?: string, ctxLen?: number, runtime?: string): Promise<HFRepoDetails> {
  let url = `${BASE}/v1/models/repo?id=${encodeURIComponent(repoId)}`;
  if (nodeName) url += `&node=${encodeURIComponent(nodeName)}`;
  if (ctxLen) url += `&ctx=${ctxLen}`;
  if (runtime) url += `&runtime=${encodeURIComponent(runtime)}`;
  const res = await apiFetch(url, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch Hugging Face repository details');
  return res.json();
}

export async function fetchFavorites(): Promise<string[]> {
  const res = await apiFetch(`${BASE}/favorites`, { headers: authHeaders() });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to fetch favourites'); }
  const data = await res.json();
  return data.model_ids || [];
}

export async function addFavorite(modelId: string): Promise<void> {
  const res = await apiFetch(`${BASE}/favorites`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ model_id: modelId }),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to add favourite'); }
}

export async function removeFavorite(modelId: string): Promise<void> {
  const encodedModelId = modelId.split('/').map(encodeURIComponent).join('/');
  const res = await apiFetch(`${BASE}/favorites/${encodedModelId}`, {
    method: 'DELETE',
    headers: authHeaders(),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to remove favourite'); }
}

export interface SystemAuditFilter {
  from?: string; // RFC3339
  to?: string;
  before?: string;
  before_id?: number;
  limit?: number;
  kind?: string;
  action?: string;
  user?: string;
  target?: string;
  source_ip?: string;
}

function demoFilterSystemAudit(all: SystemAuditEntry[], f: SystemAuditFilter): SystemAuditEntry[] {
  let filtered = all.slice();
  if (f.from) {
    const fromMs = new Date(f.from).getTime();
    if (!isNaN(fromMs)) filtered = filtered.filter((e) => new Date(e.time).getTime() >= fromMs);
  }
  if (f.to) {
    const toMs = new Date(f.to).getTime();
    if (!isNaN(toMs)) filtered = filtered.filter((e) => new Date(e.time).getTime() <= toMs);
  }
  if (f.before) {
    const beforeMs = new Date(f.before).getTime();
    if (!isNaN(beforeMs)) {
      if (f.before_id != null) {
        filtered = filtered.filter((e) => {
          const t = new Date(e.time).getTime();
          if (t < beforeMs) return true;
          if (t > beforeMs) return false;
          return (e.id ?? 0) < f.before_id!;
        });
      } else {
        filtered = filtered.filter((e) => new Date(e.time).getTime() < beforeMs);
      }
    }
  }
  if (f.action) filtered = filtered.filter((e) => e.action === f.action);
  if (f.user) filtered = filtered.filter((e) => e.username.toLowerCase().includes(f.user!.toLowerCase()));
  if (f.target) filtered = filtered.filter((e) => e.target.toLowerCase().includes(f.target!.toLowerCase()));
  if (f.source_ip) filtered = filtered.filter((e) => (e.source_ip || '').toLowerCase().includes(f.source_ip!.toLowerCase()));
  if (f.kind && f.kind !== 'all') {
    if (f.kind === 'predictive') return [];
    // Mirror ui/src/lib/activityKind.ts toActivityKind for DEMO filtering
    const toKind = (action: string): string => {
      if (['drain_node', 'undrain_node', 'set_node_prewarm'].includes(action)) return 'drain';
      if (['enable_marbor_agent', 'disable_marbor_agent', 'regenerate_marbor_agent_token', 'enroll_marbor_agent'].includes(action)) return 'agent';
      if (['runtime_start', 'runtime_stop', 'runtime_restart', 'accept_node_control', 'clear_node_control'].includes(action)) return 'runtime';
      if (['add_node', 'update_node', 'remove_node', 'patch_node'].includes(action)) return 'node';
      if (['unload_model', 'set_node_warmup', 'set_pinned_models', 'pull_model', 'pull_model_load_failed', 'pull_model_cancel', 'delete_model'].includes(action)) return 'warmup';
      if (['create_schedule', 'patch_schedule', 'delete_schedule'].includes(action) || action.startsWith('scheduled_')) return 'schedule';
      if (action.startsWith('drain_') || action.startsWith('undrain') || action === 'set_node_prewarm') return 'drain';
      if (action.includes('marbor_agent') || action.includes('_agent')) return 'agent';
      if (action.startsWith('runtime_') || action.includes('_control')) return 'runtime';
      if (action.startsWith('add_node') || action.startsWith('remove_node') || action.startsWith('patch_node') || action === 'update_node') return 'node';
      if (action.startsWith('unload') || action.includes('warmup') || action.includes('pinned') || action.startsWith('pull_model') || action === 'delete_model') return 'warmup';
      return 'config';
    };
    filtered = filtered.filter((e) => toKind(e.action) === f.kind);
  }
  filtered.sort((a, b) => {
    const d = new Date(b.time).getTime() - new Date(a.time).getTime();
    if (d !== 0) return d;
    return (b.id ?? 0) - (a.id ?? 0);
  });
  const lim = f.limit && f.limit > 0 && f.limit <= 200 ? f.limit : 100;
  return filtered.slice(0, lim);
}

export async function fetchSystemAudit(limit: number = 100): Promise<SystemAuditEntry[]> {
  return fetchSystemAuditFiltered({ limit });
}

export async function fetchSystemAuditFiltered(f: SystemAuditFilter = {}): Promise<SystemAuditEntry[]> {
  const limit = f.limit && f.limit > 0 && f.limit <= 200 ? f.limit : 100;
  if (DEMO) {
    const now = Date.now();
    const iso = (minsAgo: number) => new Date(now - minsAgo * 60_000).toISOString();
    const isoHours = (hoursAgo: number) => new Date(now - hoursAgo * 3600_000).toISOString();
    const isoDays = (daysAgo: number) => new Date(now - daysAgo * 86400_000).toISOString();
    const all: SystemAuditEntry[] = [
      // Recent minutes
      { time: iso(2), username: 'admin', action: 'drain_node', target: 'gpu-node-04', details: 'Drained: manual', source_ip: '192.168.1.5' },
      { time: iso(5), username: 'dana.rao', action: 'unload_model', target: 'gpu-node-01', details: 'Model: qwen2.5:7b', source_ip: '192.168.1.12' },
      { time: iso(9), username: 'admin', action: 'runtime_restart', target: 'gpu-node-02', details: 'Driver: systemd, Identifier: ollama.service', source_ip: '192.168.1.5' },
      { time: iso(15), username: 'admin', action: 'add_node', target: 'gpu-node-07', details: 'URL: http://10.0.0.17:11434, Runtime: ollama, VRAM: 24576MB', source_ip: '192.168.1.5' },
      { time: iso(18), username: 'admin', action: 'enable_marbor_agent', target: '10.0.0.13', details: 'Port: 9200, Scheme: https', source_ip: '192.168.1.5' },
      { time: iso(22), username: 'sam.lee', action: 'set_node_warmup', target: 'gpu-node-02', details: 'Enabled: true, Models: ["llama3.3:70b"]', source_ip: '192.168.1.8' },
      { time: iso(28), username: 'admin', action: 'undrain_node', target: 'gpu-node-04', details: '', source_ip: '192.168.1.5' },
      { time: iso(35), username: 'admin', action: 'remove_node', target: 'gpu-node-06', details: '', source_ip: '192.168.1.5' },
      { time: iso(45), username: 'admin', action: 'add_routing_rule', target: 'rule-deepseek-r1', details: 'Condition: model == "deepseek-r1", Target: gpu-node-02, Priority: 10, Enabled: true', source_ip: '192.168.1.5' },
      { time: iso(52), username: 'admin', action: 'accept_node_control', target: 'gpu-node-03', details: 'Driver: systemd, Identifier: ollama.service', source_ip: '192.168.1.5' },
      { time: iso(67), username: 'dana.rao', action: 'disable_marbor_agent', target: '10.0.0.14', details: '', source_ip: '192.168.1.12' },
      { time: iso(74), username: 'admin', action: 'set_pinned_models', target: 'gpu-node-01', details: 'Models: ["llama3:8b", "mistral:7b"]', source_ip: '192.168.1.5' },
      { time: iso(90), username: 'admin', action: 'patch_node', target: 'gpu-node-03', details: 'URLChanged: false, VRAMTotalMBChanged: true, RuntimeChanged: false', source_ip: '192.168.1.5' },
      { time: iso(110), username: 'admin', action: 'pull_model', target: 'gpu-node-02', details: 'Model: qwen2.5:14b', source_ip: '192.168.1.5' },
      { time: iso(135), username: 'admin', action: 'regenerate_marbor_agent_token', target: '10.0.0.11', details: '', source_ip: '192.168.1.5' },
      { time: iso(180), username: 'admin', action: 'add_key', target: 'marketing-team', details: 'RateLimit: 50, DailyLimit: 500, MonthlyLimit: 10000, DailyUsdCap: 50.00, MonthlyUsdCap: 200.00, Models: []', source_ip: '192.168.1.5' },
      { time: iso(200), username: 'admin', action: 'create_schedule', target: 'sched-1724170000000000001', details: 'Action: warmup, Node: gpu-node-01, At: 08:30, Models: [llama3.3:8b], Enabled: true', source_ip: '192.168.1.5' },
      // Older hours/days to make date presets meaningful
      { time: isoHours(3), username: 'system', action: 'scheduled_warmup', target: 'gpu-node-01', details: 'Schedule sched-1724170000000000001: action=warmup node=gpu-node-01 models=[llama3.3:8b] status=ok', source_ip: '' },
      { time: isoHours(8), username: 'admin', action: 'patch_schedule', target: 'sched-1724170000000000001', details: 'Action: warmup, Node: gpu-node-01, At: 09:00, Models: [qwen2.5:7b], Enabled: true', source_ip: '192.168.1.5' },
      { time: isoHours(20), username: 'admin', action: 'runtime_stop', target: 'gpu-node-03', details: 'Driver: docker', source_ip: '192.168.1.5' },
      { time: isoDays(2), username: 'dana.rao', action: 'add_node', target: 'gpu-node-05', details: 'URL: http://10.0.0.15:11434, Runtime: mlx, VRAM: 0MB', source_ip: '192.168.1.12' },
      { time: isoDays(3), username: 'system', action: 'scheduled_drain', target: 'gpu-node-03', details: 'Schedule sched-1724100000000000000: action=drain node=gpu-node-03 models=[] status=ok', source_ip: '' },
      { time: isoDays(5), username: 'admin', action: 'update_settings', target: 'global', details: 'Timezone: UTC, AuthEnabled: true, DailyCap: 150.00', source_ip: '192.168.1.5' },
      { time: isoDays(7), username: 'admin', action: 'delete_schedule', target: 'sched-1724170000000000001', details: '', source_ip: '192.168.1.5' },
      { time: isoDays(10), username: 'admin', action: 'add_key', target: 'ci-bot', details: 'RateLimit: 1000', source_ip: '10.0.0.1' },
    ];
    // Assign stable ids for pagination tiebreaker demo - newest first gets highest id, matching autoincrement order.
    all.forEach((e, idx) => { if (e.id == null) (e as any).id = 10000 - idx; });
    return demoFilterSystemAudit(all, { ...f, limit });
  }
  const params = new URLSearchParams();
  if (f.from) params.set('from', f.from);
  if (f.to) params.set('to', f.to);
  if (f.before) params.set('before', f.before);
  if (f.before_id != null) params.set('before_id', String(f.before_id));
  if (f.limit) params.set('limit', String(limit));
  else params.set('limit', '100');
  if (f.kind) params.set('kind', f.kind);
  if (f.action) params.set('action', f.action);
  if (f.user) params.set('user', f.user);
  if (f.target) params.set('target', f.target);
  if (f.source_ip) params.set('source_ip', f.source_ip);
  const qs = params.toString();
  const res = await apiFetch(`${BASE}/system-audit${qs ? `?${qs}` : ''}`, { headers: authHeaders() });
  if (!res.ok) {
    const j = await res.json().catch(() => ({}));
    throw new Error((j as any).error || 'Failed to fetch system audit logs');
  }
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

export async function triggerWarmupPing(): Promise<{ status: string }> {
  if (DEMO) return demoDelay({ status: 'triggered' });
  const res = await apiFetch(`${BASE}/warmup/ping`, { method: 'POST', headers: authHeaders() });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to trigger warmup'); }
  return res.json();
}

// --- marbor agent (per-node optional telemetry agent, internal/marboragent) ---
//
// The token is only ever returned by enable/regenerate - it is never
// retrievable again afterward (matches the API Keys "shown once" pattern).

export interface MarborAgentStatus {
  node: string;
  enabled: boolean;
  port: number;
  // scheme is the Agent's OWN transport scheme ("http" | "https") -
  // independent of this node's runtime URL scheme. Absent/undefined on a
  // disabled agent.
  scheme?: 'http' | 'https';
}

export interface MarborAgentEnableResult {
  node: string;
  enabled: boolean;
  port: number;
  scheme: 'http' | 'https';
  token: string;
  install_command: string; // Linux/macOS one-liner (install.sh, ROLE=agent)
  install_command_windows: string; // Windows PowerShell one-liner (install.ps1, ROLE=agent)
}

export async function getMarborAgent(name: string): Promise<MarborAgentStatus> {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/agent`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch marbor agent status');
  return res.json();
}

export async function enableMarborAgent(name: string, port: number, scheme: 'http' | 'https' = 'http'): Promise<MarborAgentEnableResult> {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/agent`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ port, scheme }),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to enable marbor agent'); }
  return res.json();
}

export async function regenerateMarborAgentToken(name: string): Promise<MarborAgentEnableResult> {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/agent/regenerate`, {
    method: 'POST',
    headers: authHeaders(),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to regenerate marbor agent token'); }
  return res.json();
}

export async function disableMarborAgent(name: string): Promise<void> {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/agent`, {
    method: 'DELETE',
    headers: authHeaders(),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to disable marbor agent'); }
}

// ControlDriver - how the marbor agent starts/stops/restarts the
// inference runtime process on this node. `discovered` is a suggestion
// only, refreshed by the agent's own probe on every poll cycle; `driver`/
// `identifier`/`configured` is the operator-accepted value, which only
// ever changes via acceptNodeControl - never a side effect of a re-scan.
export interface NodeControlStatus {
  node: string;
  configured: boolean;
  driver: string;
  identifier: string;
  start_command?: string;
  discovered: {
    driver: string;
    identifier: string;
    evidence: string[] | null;
  };
}

export async function getNodeControl(name: string): Promise<NodeControlStatus> {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/control`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch node control status');
  return res.json();
}

export async function acceptNodeControl(name: string, driver: string, identifier: string, startCommand?: string): Promise<void> {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/control/accept`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ driver, identifier, ...(startCommand ? { start_command: startCommand } : {}) }),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to accept control driver'); }
}

export async function clearNodeControl(name: string): Promise<void> {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/control`, {
    method: 'DELETE',
    headers: authHeaders(),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to clear control driver'); }
}

// startNodeRuntime/stopNodeRuntime/restartNodeRuntime dispatch the runtime
// control step's runtime.start/runtime.stop/runtime.restart capability - only meaningful
// once a control driver is configured (controlStatus.configured); the
// Admin API returns "Runtime control unavailable: no control driver
// configured" (422) otherwise, surfaced here as a thrown error.
export async function startNodeRuntime(name: string): Promise<void> {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/runtime/start`, {
    method: 'POST',
    headers: authHeaders(),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to start runtime'); }
}

export async function stopNodeRuntime(name: string): Promise<void> {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/runtime/stop`, {
    method: 'POST',
    headers: authHeaders(),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to stop runtime'); }
}

export async function restartNodeRuntime(name: string): Promise<void> {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/runtime/restart`, {
    method: 'POST',
    headers: authHeaders(),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to restart runtime'); }
}

// getNodeRuntimeLogs fetches a point-in-time snapshot of recent log lines
// from a node's runtime process - not a live tail. A node whose
// control driver has no real log source (e.g. a bare PID-file process with
// no supervisor) rejects with that driver's own "not supported" error.
export async function getNodeRuntimeLogs(name: string, lines?: number): Promise<{ lines: string[] }> {
  const qs = lines ? `?lines=${lines}` : '';
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/runtime/logs${qs}`, {
    method: 'POST',
    headers: authHeaders(),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to fetch runtime logs'); }
  return res.json();
}

// getNodeModels lists models already downloaded on a node (not just
// currently loaded - node.loadedModels covers that), via the node's Node
// Agent ("models.list" capability). Callers must check
// node.agentCapabilities?.includes('models.list') before calling - a node
// without the capability returns a 501, surfaced here as a thrown error.
export async function getNodeModels(name: string): Promise<LocalModel[]> {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/models`, { headers: authHeaders() });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to fetch locally available models'); }
  const data = await res.json();
  return data.models || [];
}

// deleteNodeModel removes a locally-downloaded model from a node, via the
// node's marbor agent ("models.delete" capability). Callers must check
// node.agentCapabilities?.includes('models.delete') before calling - a node
// without the capability returns a 501, surfaced here as a thrown error.
// model's "/"-delimited segments are each encodeURIComponent'd independently
// then rejoined with a literal "/" - a name like "org/repo" is meant to land
// on the backend as two path segments (mirroring the agent's own
// "{name...}" wildcard route), but any other character ('#', '?', a space)
// must still be escaped or it gets reinterpreted as a fragment/query
// boundary, truncating the request to a different (shorter) model name.
export async function deleteNodeModel(name: string, model: string): Promise<void> {
  const encodedModel = model.split('/').map(encodeURIComponent).join('/');
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/models/${encodedModel}`, {
    method: 'DELETE',
    headers: authHeaders(),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to delete model'); }
}

export interface NodeHealthCheckResult {
  ok: boolean;
  error?: string;
  latencyMs?: number;
}

// checkNodeHealth triggers an on-demand active liveness probe against a
// node's real runtime right now, via the node's marbor agent
// ("runtime.health_check" capability) - distinct from the node's passive,
// poll-cycle-cached health already shown elsewhere in the dashboard.
// Callers must check node.agentCapabilities?.includes('runtime.health_check')
// before calling - a node without the capability returns a 501, surfaced
// here as a thrown error. A probe that completes but finds the runtime down
// is NOT a thrown error - it resolves normally with { ok: false, error }, the
// real answer to "is it up right now."
export async function checkNodeHealth(name: string): Promise<NodeHealthCheckResult> {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/health-check`, { headers: authHeaders() });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to run health check'); }
  return res.json();
}

// probeNodeTLS retrieves the certificate fingerprint an https:// node
// currently presents, WITHOUT pinning it - callers
// must display the value for the operator to confirm out of band, then
// call patchNode(name, { tls_fingerprint }) only on an explicit "Confirm &
// Pin" click. Never call patchNode automatically from this result.
export async function probeNodeTLS(name: string): Promise<{ fingerprint: string }> {
  const res = await apiFetch(`${BASE}/nodes/${encodeURIComponent(name)}/tls-probe`, {
    method: 'POST',
    headers: authHeaders(),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to probe TLS certificate'); }
  return res.json();
}

// runBenchmark starts an in-dashboard hardware benchmark job (see
// benchmarkProgress.ts for the SSE progress consumer). node+model must
// already be known to the marbor; the marbor auto-provisions and later deletes
// an ephemeral API key server-side, so no key input is required here.
export async function runBenchmark(node: string, model: string, n: number): Promise<{ job_id: string }> {
  const res = await apiFetch(`${BASE}/benchmark/run`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ node, model, n }),
  });
  if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error((j as any).error || 'Failed to start benchmark'); }
  return res.json();
}

export async function cancelBenchmarkJob(jobId: string): Promise<void> {
  await apiFetch(`${BASE}/benchmark/${encodeURIComponent(jobId)}`, {
    method: 'DELETE',
    headers: authHeaders(),
  });
}

export async function fetchBenchmarkRuns(): Promise<BenchmarkRun[]> {
  const res = await apiFetch(`${BASE}/benchmark/runs`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch benchmark history');
  const data = await res.json();
  return data.runs || [];
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

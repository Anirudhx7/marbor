import { GPUNode, APIKey, LiveRequest, Savings, CloudProvider, ModelCatalog, RequestEntry, Analytics, ModelFitResponse, ModelCatalogResponse } from '../types';

const BASE = '/admin';

function getAdminToken(): string {
  let token = localStorage.getItem('adminToken');
  if (!token) {
    token = window.prompt('Enter admin token:') ?? '';
    if (token) localStorage.setItem('adminToken', token);
  }
  return token;
}

function authHeaders(): { Authorization: string } {
  return { Authorization: `Bearer ${getAdminToken()}` };
}

export async function fetchNodes(): Promise<GPUNode[]> {
  const res = await fetch(`${BASE}/nodes`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch nodes');
  return res.json();
}

export async function fetchKeys(): Promise<APIKey[]> {
  const res = await fetch(`${BASE}/keys`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch keys');
  return res.json();
}

export async function fetchLiveRequests(): Promise<LiveRequest[]> {
  const res = await fetch(`${BASE}/requests/live`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch requests');
  return res.json();
}

export async function fetchSummary() {
  const res = await fetch(`${BASE}/metrics/summary`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch summary');
  const d = await res.json();
  return {
    activeRequests: d.active_requests ?? 0,
    avgLatency: d.avg_latency ?? 0,
    tokensPerMin: d.tokens_per_min ?? 0,
    coldStarts: d.cold_starts ?? 0,
  };
}

export async function createKey(data: any) {
  const res = await fetch(`${BASE}/keys`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to create key');
}

export async function revokeKey(name: string) {
  const res = await fetch(`${BASE}/keys/${name}`, {
    method: 'DELETE',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error('Failed to revoke key');
}

export async function addNode(data: any) {
  const res = await fetch(`${BASE}/nodes`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to add node');
}

export async function removeNode(name: string) {
  const res = await fetch(`${BASE}/nodes/${name}`, {
    method: 'DELETE',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error('Failed to remove node');
}

export async function fetchRoutingRules() {
  const res = await fetch(`${BASE}/routing/rules`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch routing rules');
  return res.json();
}

export async function addRoutingRule(data: any) {
  const res = await fetch(`${BASE}/routing/rules`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to add routing rule');
}

export async function removeRoutingRule(id: string) {
  const res = await fetch(`${BASE}/routing/rules/${id}`, {
    method: 'DELETE',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error('Failed to remove routing rule');
}

export async function toggleRoutingRule(id: string) {
  const res = await fetch(`${BASE}/routing/rules/${id}/toggle`, {
    method: 'PUT',
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error('Failed to toggle routing rule');
}

export async function setRoutingStrategy(strategy: string) {
  const res = await fetch(`${BASE}/routing/strategy`, {
    method: 'PUT',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ strategy }),
  });
  if (!res.ok) throw new Error('Failed to set routing strategy');
}

export async function fetchSettings() {
  const res = await fetch(`${BASE}/settings`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch settings');
  return res.json();
}

export async function getSavings(): Promise<Savings> {
  const res = await fetch(`${BASE}/metrics/savings`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch savings');
  return res.json();
}

export async function getCloudProviders(): Promise<CloudProvider[]> {
  const res = await fetch(`${BASE}/cloud/providers`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch cloud providers');
  return res.json();
}

export async function fetchModels(): Promise<ModelCatalog> {
  const res = await fetch(`${BASE}/models`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch models');
  return res.json();
}

export async function getRequests(): Promise<RequestEntry[]> {
  const res = await fetch(`${BASE}/requests`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch requests');
  return res.json();
}

export async function updateSettings(data: any) {
  const res = await fetch(`${BASE}/settings`, {
    method: 'PUT',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to update settings');
}

export async function getAnalytics(): Promise<Analytics> {
  const res = await fetch(`${BASE}/analytics`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch analytics');
  return res.json();
}

export async function pullModel(nodeName: string, model: string): Promise<void> {
  const res = await fetch(`${BASE}/v1/nodes/${encodeURIComponent(nodeName)}/pull`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ model }),
  });
  if (!res.ok) throw new Error(`Pull failed: ${res.statusText}`);
}

export async function fetchModelFit(): Promise<ModelFitResponse> {
  const res = await fetch(`${BASE}/nodes/model-fit`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch model fit data');
  return res.json();
}

export async function fetchModelCatalog(): Promise<ModelCatalogResponse> {
  const res = await fetch(`${BASE}/v1/models/catalog`, { headers: authHeaders() });
  if (!res.ok) throw new Error('Failed to fetch model catalog');
  return res.json();
}


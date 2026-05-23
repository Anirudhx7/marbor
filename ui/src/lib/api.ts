import { GPUNode, APIKey, LiveRequest, Savings, CloudProvider, ModelCatalog } from '../types';

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
  return res.json();
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

export async function updateSettings(data: any) {
  const res = await fetch(`${BASE}/settings`, {
    method: 'PUT',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to update settings');
}


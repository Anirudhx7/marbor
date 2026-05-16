import { GPUNode, APIKey, LiveRequest } from '../types';

const BASE = '/admin';
const token = localStorage.getItem('adminToken') ?? 'admin';
const headers = { Authorization: `Bearer ${token}` };

export async function fetchNodes(): Promise<GPUNode[]> {
  const res = await fetch(`${BASE}/nodes`, { headers });
  if (!res.ok) throw new Error('Failed to fetch nodes');
  return res.json();
}

export async function fetchKeys(): Promise<APIKey[]> {
  const res = await fetch(`${BASE}/keys`, { headers });
  if (!res.ok) throw new Error('Failed to fetch keys');
  return res.json();
}

export async function fetchLiveRequests(): Promise<LiveRequest[]> {
  const res = await fetch(`${BASE}/requests/live`, { headers });
  if (!res.ok) throw new Error('Failed to fetch requests');
  return res.json();
}

export async function fetchSummary() {
  const res = await fetch(`${BASE}/metrics/summary`, { headers });
  if (!res.ok) throw new Error('Failed to fetch summary');
  return res.json();
}

export async function createKey(data: any) {
  const res = await fetch(`${BASE}/keys`, {
    method: 'POST',
    headers: { ...headers, 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to create key');
}

export async function revokeKey(name: string) {
  const res = await fetch(`${BASE}/keys/${name}`, {
    method: 'DELETE',
    headers,
  });
  if (!res.ok) throw new Error('Failed to revoke key');
}

export async function addNode(data: any) {
  const res = await fetch(`${BASE}/nodes`, {
    method: 'POST',
    headers: { ...headers, 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to add node');
}

export async function removeNode(name: string) {
  const res = await fetch(`${BASE}/nodes/${name}`, {
    method: 'DELETE',
    headers,
  });
  if (!res.ok) throw new Error('Failed to remove node');
}

export async function fetchRoutingRules() {
  const res = await fetch(`${BASE}/routing/rules`, { headers });
  if (!res.ok) throw new Error('Failed to fetch routing rules');
  return res.json();
}

export async function addRoutingRule(data: any) {
  const res = await fetch(`${BASE}/routing/rules`, {
    method: 'POST',
    headers: { ...headers, 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to add routing rule');
}

export async function removeRoutingRule(id: string) {
  const res = await fetch(`${BASE}/routing/rules/${id}`, {
    method: 'DELETE',
    headers,
  });
  if (!res.ok) throw new Error('Failed to remove routing rule');
}

export async function toggleRoutingRule(id: string) {
  const res = await fetch(`${BASE}/routing/rules/${id}/toggle`, {
    method: 'PUT',
    headers,
  });
  if (!res.ok) throw new Error('Failed to toggle routing rule');
}

export async function setRoutingStrategy(strategy: string) {
  const res = await fetch(`${BASE}/routing/strategy`, {
    method: 'PUT',
    headers: { ...headers, 'Content-Type': 'application/json' },
    body: JSON.stringify({ strategy }),
  });
  if (!res.ok) throw new Error('Failed to set routing strategy');
}

export async function fetchSettings() {
  const res = await fetch(`${BASE}/settings`, { headers });
  if (!res.ok) throw new Error('Failed to fetch settings');
  return res.json();
}

export async function updateSettings(data: any) {
  const res = await fetch(`${BASE}/settings`, {
    method: 'PUT',
    headers: { ...headers, 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  });
  if (!res.ok) throw new Error('Failed to update settings');
}


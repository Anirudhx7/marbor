// Pure mapping of one /admin/audit row to the row the Requests page renders.
// Kept free of runtime imports so it can be tested directly with node.
import type { RequestEntry } from '../types';

export interface AuditLogEntry {
  id?: number;
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
  queue_wait_ms?: number;
  cloud_ms?: number;
  // Prompt-cache split joined from the request log; absent when unknown.
  cachedTokens?: number;
  inputTokens?: number;
}

// AuditPageEntry is a request row plus the server row id used as the cursor.
export type AuditPageEntry = RequestEntry & { auditId: number };

// mapAuditEntry turns one /admin/audit row into the row the Requests page
// renders. The cache split is carried only when both values are numbers: a
// missing one is unknown and stays absent.
export function mapAuditEntry(e: AuditLogEntry): AuditPageEntry {
  const hasSplit = typeof e.cachedTokens === 'number' && typeof e.inputTokens === 'number';
  return {
    id: e.request_id,
    auditId: e.id ?? 0,
    time: e.time,
    key_name: e.key_name,
    model: e.cloud && e.cloud_model ? e.cloud_model : e.model,
    node: e.node,
    status: Number(e.status) || 0,
    latency_ms: e.latency_ms,
    cloud: e.cloud,
    routingReason: e.routing_reason || undefined,
    queueWaitMs: typeof e.queue_wait_ms === 'number' ? e.queue_wait_ms : undefined,
    cloudMs: typeof e.cloud_ms === 'number' ? e.cloud_ms : undefined,
    cachedTokens: hasSplit ? e.cachedTokens : undefined,
    inputTokens: hasSplit ? e.inputTokens : undefined,
  };
}

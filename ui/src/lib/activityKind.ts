export type ActivityKind = 'drain' | 'agent' | 'runtime' | 'node' | 'warmup' | 'schedule' | 'predictive' | 'access' | 'keys' | 'users' | 'config';

// Access, Keys, Users and Settings ('config') are UI-only splits of the
// server's single `config` kind: the Admin API still reports all four as
// `config`, and the page narrows them on the client. Every other kind lines up
// with the server one to one. Keep drain vs warmup distinct; predictive is
// never derived from system_audit, it comes from its own endpoint and gets its
// own badge/section.
const CONFIG_DERIVED: ReadonlySet<ActivityKind | 'all'> = new Set<ActivityKind | 'all'>(['access', 'keys', 'users', 'config']);

// Every kind. Typed as a full Record so adding a kind to ActivityKind without
// listing it here fails the type check; the Kind filter options are checked
// against this list.
const KIND_PRESENCE: Record<ActivityKind, true> = {
  access: true, keys: true, users: true, config: true, drain: true, agent: true,
  runtime: true, node: true, warmup: true, schedule: true, predictive: true,
};
export const ALL_ACTIVITY_KINDS: readonly ActivityKind[] = Object.keys(KIND_PRESENCE) as ActivityKind[];

// The action the server records for a sign-in attempt that failed.
export const LOGIN_FAILURE = 'login_failure';

// Known actions and their kind. Looked up before any prefix or substring
// fallback so a known action can never be reclassified by a looser rule.
export const EXACT_ACTION_KINDS: Readonly<Record<string, ActivityKind>> = {
  // Sign-ins, including failed attempts
  login_success: 'access',
  [LOGIN_FAILURE]: 'access',
  // API key changes
  add_key: 'keys',
  patch_key: 'keys',
  revoke_key: 'keys',
  rotate_key: 'keys',
  api_key_expiry_malformed: 'keys',
  // Dashboard user changes
  create_user: 'users',
  patch_user: 'users',
  delete_user: 'users',
  suspend_user: 'users',
  approve_user: 'users',
  reset_user_password: 'users',
  // Drain operations: node drain state changes
  drain_node: 'drain',
  undrain_node: 'drain',
  set_node_prewarm: 'drain',
  // Agent lifecycle: enable/disable/regenerate/enroll
  enable_marbor_agent: 'agent',
  disable_marbor_agent: 'agent',
  regenerate_marbor_agent_token: 'agent',
  enroll_marbor_agent: 'agent',
  // Runtime lifecycle: start/stop/restart + control driver accept/clear
  runtime_start: 'runtime',
  runtime_stop: 'runtime',
  runtime_restart: 'runtime',
  accept_node_control: 'runtime',
  clear_node_control: 'runtime',
  // Node membership: join/leave/edit
  add_node: 'node',
  update_node: 'node',
  remove_node: 'node',
  patch_node: 'node',
  // Warmup/model lifecycle: unload, keep-warm pinning, pulls
  unload_model: 'warmup',
  set_node_warmup: 'warmup',
  set_pinned_models: 'warmup',
  pull_model: 'warmup',
  pull_model_load_failed: 'warmup',
  pull_model_cancel: 'warmup',
  delete_model: 'warmup',
  delete_model_replica: 'warmup',
  // Schedule management (scheduled firings are matched by prefix below)
  create_schedule: 'schedule',
  patch_schedule: 'schedule',
  delete_schedule: 'schedule',
};

// classifyAction looks the action up in the exact table first, then the
// scheduled_ prefix, then the looser prefix and substring rules kept for
// forward compatibility with future actions.
export function classifyAction(action: string, exact: Readonly<Record<string, ActivityKind>>): ActivityKind {
  if (!action) return 'config';
  if (Object.prototype.hasOwnProperty.call(exact, action)) return exact[action];
  // Scheduled firings (actor system, distinct from warmup)
  if (action.startsWith('scheduled_')) return 'schedule';
  if (action.startsWith('drain_') || action.startsWith('undrain')) return 'drain';
  if (action.includes('marbor_agent') || action.includes('_agent')) return 'agent';
  if (action.startsWith('runtime_') || action.includes('_control')) return 'runtime';
  if (action.startsWith('add_node') || action.startsWith('remove_node') || action.startsWith('patch_node')) return 'node';
  if (action.startsWith('unload') || action.includes('warmup') || action.includes('pinned') || action.startsWith('pull_model') || action.startsWith('delete_model')) return 'warmup';
  return 'config';
}

// toActivityKind maps a system_audit action string to its kind bucket.
export function toActivityKind(action: string): ActivityKind {
  return classifyAction(action, EXACT_ACTION_KINDS);
}

export function getActivityKindLabel(kind: ActivityKind): string {
  const labels: Record<ActivityKind, string> = {
    drain: 'Drain',
    agent: 'Agent',
    runtime: 'Runtime',
    node: 'Node',
    warmup: 'Warmup',
    schedule: 'Schedule',
    predictive: 'Predictive',
    access: 'Access',
    keys: 'Keys',
    users: 'Users',
    config: 'Settings',
  };
  return labels[kind];
}

export function getActivityKindColor(kind: ActivityKind): string {
  const colors: Record<ActivityKind, string> = {
    drain: 'bg-warning/15 text-warning border-warning/20',
    agent: 'bg-info/15 text-info border-info/20',
    runtime: 'bg-primary/15 text-primary border-primary/20',
    node: 'bg-success/15 text-success border-success/20',
    warmup: 'bg-chart-1/15 text-chart-1 border-chart-1/20',
    schedule: 'bg-chart-2/15 text-chart-2 border-chart-2/20',
    predictive: 'bg-chart-2/15 text-chart-2 border-chart-2/20',
    access: 'bg-info/15 text-info border-info/20',
    keys: 'bg-chart-1/15 text-chart-1 border-chart-1/20',
    users: 'bg-chart-2/15 text-chart-2 border-chart-2/20',
    config: 'bg-muted text-muted-foreground border-border',
  };
  return colors[kind];
}

// serverKindFor is the kind sent to the Admin API: the four config-derived
// kinds are all fetched as `config` and narrowed on the client.
export function serverKindFor(kind: ActivityKind | 'all'): string {
  return CONFIG_DERIVED.has(kind) ? 'config' : kind;
}

export function isFailedLogin(action: string): boolean {
  return action === LOGIN_FAILURE;
}

// matchesKindFilter is the client-side row test for the selected kind. The
// server already filtered every kind except the config-derived ones, so only
// those are narrowed here. Settings means "config bucket minus access, keys
// and users", so actions the server files under config but the UI badges
// differently (for example delete_model_replica) stay visible under it.
export function matchesKindFilter(action: string, kindFilter: ActivityKind | 'all'): boolean {
  if (kindFilter === 'access' || kindFilter === 'keys' || kindFilter === 'users') {
    return toActivityKind(action) === kindFilter;
  }
  if (kindFilter === 'config') {
    const k = toActivityKind(action);
    return k !== 'access' && k !== 'keys' && k !== 'users';
  }
  return true;
}

// distinctOperators lists usernames once each, comparing without regard to
// case and keeping the first-seen casing.
export function distinctOperators(names: readonly (string | null | undefined)[]): string[] {
  const seen = new Map<string, string>();
  for (const n of names) {
    const name = n == null ? '' : n.trim();
    if (name === '') continue;
    const key = name.toLowerCase();
    if (!seen.has(key)) seen.set(key, name);
  }
  return Array.from(seen.values());
}

// isClientNarrowed is true when the visible rows are narrowed on the client
// (a config-derived kind or local search text), so older unloaded events may
// still match. searchText is the raw, undebounced search box value on purpose:
// the narrowing happens immediately on the client, unlike the server filters.
export function isClientNarrowed(kindFilter: ActivityKind | 'all', searchText: string): boolean {
  return CONFIG_DERIVED.has(kindFilter) || searchText.trim() !== '';
}

// olderEventsMayMatch is true when a client-side narrowing (sub-kind or search
// text) is active and more events remain unloaded. Server-side filters alone
// never need it, and predictive decisions are all loaded already.
export function olderEventsMayMatch(kindFilter: ActivityKind | 'all', searchText: string, hasMore: boolean): boolean {
  return hasMore && isClientNarrowed(kindFilter, searchText);
}

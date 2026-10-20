export type ActivityKind = 'drain' | 'agent' | 'runtime' | 'node' | 'warmup' | 'schedule' | 'predictive' | 'access' | 'keys' | 'users' | 'config';

// Access, Keys, Users and Settings ('config') are UI-only splits of the
// server's single `config` kind: the Admin API still reports all four as
// `config`, and the page narrows them on the client. Every other kind lines up
// with the server one to one. Keep drain vs warmup distinct; predictive is
// never derived from system_audit, it comes from its own endpoint and gets its
// own badge/section.
const CONFIG_DERIVED: ReadonlySet<string> = new Set(['access', 'keys', 'users', 'config']);

// toActivityKind maps a system_audit action string to its kind bucket.
export function toActivityKind(action: string): ActivityKind {
  // Sign-ins, including failed attempts
  if (action === 'login_success' || action === 'login_failure') return 'access';
  // API key changes
  if (
    action === 'add_key' ||
    action === 'patch_key' ||
    action === 'revoke_key' ||
    action === 'rotate_key' ||
    action === 'api_key_expiry_malformed'
  ) {
    return 'keys';
  }
  // Dashboard user changes
  if (
    action === 'create_user' ||
    action === 'patch_user' ||
    action === 'delete_user' ||
    action === 'suspend_user' ||
    action === 'approve_user' ||
    action === 'reset_user_password'
  ) {
    return 'users';
  }
  // Drain operations: node drain state changes
  if (action === 'drain_node' || action === 'undrain_node' || action === 'set_node_prewarm') {
    return 'drain';
  }
  // Agent lifecycle: enable/disable/regenerate/enroll
  if (
    action === 'enable_marbor_agent' ||
    action === 'disable_marbor_agent' ||
    action === 'regenerate_marbor_agent_token' ||
    action === 'enroll_marbor_agent'
  ) {
    return 'agent';
  }
  // Runtime lifecycle: start/stop/restart + control driver accept/clear
  if (
    action === 'runtime_start' ||
    action === 'runtime_stop' ||
    action === 'runtime_restart' ||
    action === 'accept_node_control' ||
    action === 'clear_node_control'
  ) {
    return 'runtime';
  }
  // Node membership: join/leave/edit
  if (action === 'add_node' || action === 'update_node' || action === 'remove_node' || action === 'patch_node') {
    return 'node';
  }
  // Warmup/model lifecycle: unload, keep-warm pinning, pulls
  if (
    action === 'unload_model' ||
    action === 'set_node_warmup' ||
    action === 'set_pinned_models' ||
    action === 'pull_model' ||
    action === 'pull_model_load_failed' ||
    action === 'pull_model_cancel' ||
    action === 'delete_model' ||
    action === 'delete_model_replica'
  ) {
    return 'warmup';
  }
  // Schedule management and scheduled firings (actor system, distinct from warmup)
  if (
    action === 'create_schedule' ||
    action === 'patch_schedule' ||
    action === 'delete_schedule' ||
    action.startsWith('scheduled_')
  ) {
    return 'schedule';
  }
  // Fallback prefix checks for forward compat with future actions
  if (action.startsWith('drain_') || action.startsWith('undrain') || action === 'set_node_prewarm') return 'drain';
  if (action.includes('marbor_agent') || action.includes('_agent')) return 'agent';
  if (action.startsWith('runtime_') || action.includes('_control')) return 'runtime';
  if (action.startsWith('add_node') || action.startsWith('remove_node') || action.startsWith('patch_node') || action === 'update_node') return 'node';
  if (action.startsWith('unload') || action.includes('warmup') || action.includes('pinned') || action.startsWith('pull_model') || action.startsWith('delete_model')) return 'warmup';
  return 'config';
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
  return action === 'login_failure';
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
export function distinctOperators(names: readonly string[]): string[] {
  const seen = new Map<string, string>();
  for (const n of names) {
    const key = n.toLowerCase();
    if (!seen.has(key)) seen.set(key, n);
  }
  return Array.from(seen.values());
}

// isClientNarrowed is true when the visible rows are narrowed on the client
// (a config-derived kind or local search text), so older unloaded events may
// still match.
export function isClientNarrowed(kindFilter: ActivityKind | 'all', searchText: string): boolean {
  return CONFIG_DERIVED.has(kindFilter) || searchText.trim() !== '';
}

export type ActivityKind = 'drain' | 'agent' | 'runtime' | 'node' | 'warmup' | 'schedule' | 'predictive' | 'config';

// toActivityKind maps a system_audit action string to its fleet-operations
// kind bucket. Taxonomy is locked: keep drain vs warmup distinct,
// predictive is never derived from system_audit, it comes from its own
// endpoint and gets its own badge/section.
export function toActivityKind(action: string): ActivityKind {
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
    config: 'Config',
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
    config: 'bg-muted text-muted-foreground border-border',
  };
  return colors[kind];
}

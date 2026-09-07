import { useState, useEffect, useRef } from 'react';
import {
  Zap,
  Clock,
  ArrowUpRight,
  Plus,
  Trash2,
  Check,
  Route,
  Shield,
  Server,
  Cloud,
  RefreshCw,
  HardDrive,
  Pencil,
  ChevronUp,
  ChevronDown
} from 'lucide-react';
import { Badge } from '../components/Badge';
import { StatusDot } from '../components/StatusDot';
import { Modal } from '../components/Modal';
import { CustomSelect, CustomCombobox, CustomTagCombobox } from '../components/Select';
import { SavingsCard } from '../components/SavingsCard';
import { mockGPUNodes, mockSavings, mockCloudProviders, mockModelCatalog } from '../lib/mockData';
import {
  fetchRoutingRules,
  addRoutingRule,
  removeRoutingRule,
  toggleRoutingRule,
  setRoutingStrategy,
  fetchRoutingStrategy,
  fetchNodes,
  fetchSavings,
  fetchCloudProviders,
  addCloudProvider,
  updateCloudProvider,
  deleteCloudProvider,
  testCloudProvider,
  reorderCloudProviders,
  fetchModels,
  fetchSettings,
  updateSettings
} from '../lib/api';
import { useDemoMode } from '../hooks/useDemoMode';
import { Savings, CloudProvider, CloudProviderInput } from '../types';

// Known cloud fallback providers. All use plain `Authorization: Bearer <key>`
// auth and an OpenAI-compatible /chat/completions schema, matching this
// marbor's proxy - Azure OpenAI is deliberately excluded (needs an `api-key`
// header + per-deployment URL, which this proxy doesn't support).
// Relocated verbatim from Settings.tsx - cloud provider config lives
// on the Routing page now, since provider priority is a routing decision.
const CLOUD_PROVIDER_PRESETS: Record<string, { label: string; baseUrl: string; defaultModel: string }> = {
  openai: { label: 'OpenAI', baseUrl: 'https://api.openai.com/v1', defaultModel: 'gpt-4o' },
  anthropic: { label: 'Anthropic', baseUrl: 'https://api.anthropic.com/v1', defaultModel: 'claude-sonnet-5' },
  openrouter: { label: 'OpenRouter', baseUrl: 'https://openrouter.ai/api/v1', defaultModel: 'openai/gpt-4o' },
  groq: { label: 'Groq', baseUrl: 'https://api.groq.com/openai/v1', defaultModel: 'llama-3.3-70b-versatile' },
  together: { label: 'Together AI', baseUrl: 'https://api.together.xyz/v1', defaultModel: 'meta-llama/Llama-3.3-70B-Instruct-Turbo' },
  fireworks: { label: 'Fireworks AI', baseUrl: 'https://api.fireworks.ai/inference/v1', defaultModel: 'accounts/fireworks/models/llama-v3p1-70b-instruct' },
  deepseek: { label: 'DeepSeek', baseUrl: 'https://api.deepseek.com/v1', defaultModel: 'deepseek-chat' },
  mistral: { label: 'Mistral AI', baseUrl: 'https://api.mistral.ai/v1', defaultModel: 'mistral-large-latest' },
  xai: { label: 'xAI (Grok)', baseUrl: 'https://api.x.ai/v1', defaultModel: 'grok-2-latest' },
  cerebras: { label: 'Cerebras', baseUrl: 'https://api.cerebras.ai/v1', defaultModel: 'llama-3.3-70b' },
  nvidia: { label: 'NVIDIA NIM', baseUrl: 'https://integrate.api.nvidia.com/v1', defaultModel: 'nvidia/nemotron-3-8b-instruct' },
  custom: { label: 'Custom / Other', baseUrl: '', defaultModel: '' },
};

// Compact toggle switch shared by boolean controls on this page.
// Relocated verbatim from Settings.tsx for the provider Enabled toggle.
function Toggle({ on, onToggle }: { on: boolean; onToggle: () => void }) {
  return (
    <button
      onClick={onToggle}
      className={`relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition-colors ${on ? 'bg-primary' : 'bg-muted-foreground/30'}`}
    >
      <span className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${on ? 'translate-x-6' : 'translate-x-1'}`} />
    </button>
  );
}

const STRATEGIES = [
  { 
    value: 'warm-first', 
    label: 'Warm First', 
    description: 'Prioritize nodes that already have the model loaded in VRAM.',
    icon: <Zap className="w-4 h-4" />
  },
  { 
    value: 'least-connections', 
    label: 'Least Connections', 
    description: 'Route to the node with the fewest active requests.',
    icon: <ArrowUpRight className="w-4 h-4" />
  },
  { 
    value: 'round-robin', 
    label: 'Round Robin', 
    description: 'Cycle through all healthy nodes sequentially.',
    icon: <Route className="w-4 h-4" />
  },
];

interface RoutingRule {
  id: string;
  priority: number;
  condition: string;
  targetNode: string;
  strategy: string;
  enabled: boolean;
}

const MOCK_RULES: RoutingRule[] = [
  {
    id: '1',
    priority: 10,
    condition: 'model =~ "70b"',
    targetNode: 'gpu-node-01',
    strategy: 'warm-first',
    enabled: true,
  },
  {
    id: '2',
    priority: 20,
    condition: 'api_key == "sk-prod-*"',
    targetNode: 'any',
    strategy: 'least-connections',
    enabled: true,
  },
];

export function Routing() {
  const { demoMode } = useDemoMode();
  const [currentStrategy, setCurrentStrategyState] = useState('');
  const [rules, setRules] = useState<RoutingRule[]>(demoMode ? MOCK_RULES : []);
  const [availableNodes, setAvailableNodes] = useState<any[]>([]);
  const [savings, setSavings] = useState<Savings | null>(demoMode ? mockSavings : null);
  const [savingsLoading, setSavingsLoading] = useState(!demoMode);
  const [loading, setLoading] = useState(true);
  const [isCreateModalOpen, setIsCreateModalOpen] = useState(false);
  const [isDeleteModalOpen, setIsDeleteModalOpen] = useState(false);
  const [ruleToDelete, setRuleToDelete] = useState<RoutingRule | null>(null);
  const [ruleToToggle, setRuleToToggle] = useState<RoutingRule | null>(null);
  const [strategyToConfirm, setStrategyToConfirm] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Scoped separately from the shared `error` banner above, which is also
  // set by unrelated failures (rule delete/toggle/create, strategy read in
  // loadData) and never cleared when this modal opens - a stale unrelated
  // error would otherwise appear to belong to the strategy change.
  const [strategyError, setStrategyError] = useState<string | null>(null);

  // Cloud providers CRUD - relocated verbatim from Settings.tsx.
  // Provider priority is a routing decision (fallback order for overflow
  // traffic), not general app config, so it lives on the Routing page now.
  // Same endpoints, same behavior - only the owning page changed.
  const [cloudProviders, setCloudProviders] = useState<CloudProvider[]>(demoMode ? mockCloudProviders : []);
  const [cloudLoading, setCloudLoading] = useState(!demoMode);
  const [cloudModalOpen, setCloudModalOpen] = useState(false);
  const [editingProvider, setEditingProvider] = useState<CloudProviderInput | null>(null);
  const [cloudSaving, setCloudSaving] = useState(false);
  const [cloudTesting, setCloudTesting] = useState(false);
  const [cloudError, setCloudError] = useState<string | null>(null);
  const [providerToDelete, setProviderToDelete] = useState<string | null>(null);

  // Local model fallback chain - relocated verbatim from Settings.tsx.
  // Ordered local alternates tried when no node can serve the requested model,
  // before cloud - only applies to keys with "Allow local degradation" enabled.
  // Persisted immediately via updateSettings (partial routing payload - the
  // backend merges onto current config, so no other field is zeroed).
  const [localDegradationChains, setLocalDegradationChains] = useState<Record<string, string[]>>({});
  const [chainsLoading, setChainsLoading] = useState(!demoMode);
  const [chainsSaving, setChainsSaving] = useState(false);
  const [newDegModel, setNewDegModel] = useState('');
  const [newDegAlts, setNewDegAlts] = useState('');
  const [degChainError, setDegChainError] = useState<string | null>(null);

  // LiteLLM flag only gates the Cloud Providers card subtitle/Add button,
  // exactly as Settings.tsx did. Read-only here, owned by Settings.
  const [liteLLMEnabled, setLiteLLMEnabled] = useState(false);
  // Known model names for the searchable fallback-chain comboboxes -
  // suggestions only, never enforced. Relocated from Settings.tsx;
  // Settings keeps its own copy for the context-windows combobox.
  const [knownModelNames, setKnownModelNames] = useState<string[]>([]);

  // Guards loadData and the action handlers below against a state-set after
  // unmount - loadData and handleStrategyChange/handleToggleRule/
  // handleCreateRule/handleDeleteRule all call setError/setRules post-await
  // with no guard today.
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  const [newRuleForm, setNewRuleForm] = useState({
    priority: '',
    condition: '',
    targetNode: '',
    strategy: 'warm-first',
  });
  const [formErrors, setFormErrors] = useState<string[]>([]);

  const loadData = async () => {
    if (demoMode) {
      if (!mountedRef.current) return;
      setRules(MOCK_RULES);
      setAvailableNodes(mockGPUNodes);
      setCurrentStrategyState('warm-first');
      setSavings(mockSavings);
      setSavingsLoading(false);
      setError(null);
      setLoading(false);
      return;
    }

    try {
      const [rulesData, nodesData] = await Promise.all([
        fetchRoutingRules(),
        fetchNodes(),
      ]);
      if (!mountedRef.current) return;
      setRules(Array.isArray(rulesData) ? rulesData : []);
      setAvailableNodes(nodesData || []);
      setError(null);
      // Fetch strategy separately so failure is visible to the user
      try {
        const strategy = await fetchRoutingStrategy();
        if (mountedRef.current) setCurrentStrategyState(strategy);
      } catch {
        if (mountedRef.current) setCurrentStrategyState('');
      }
    } catch (err: any) {
      if (!mountedRef.current) return;
      setError(err.message || 'Failed to connect to backend');
      setRules([]);
      setAvailableNodes([]);
      setCurrentStrategyState('');
    } finally {
      if (mountedRef.current) setLoading(false);
    }
  };

  useEffect(() => {
    loadData();
  }, [demoMode]);

  // Cloud providers load - relocated from Settings.tsx. Same
  // endpoints, same demo behavior (mock list offline, live fetch otherwise).
  useEffect(() => {
    if (demoMode) {
      if (!mountedRef.current) return;
      setCloudProviders(mockCloudProviders);
      setCloudLoading(false);
      return;
    }
    let active = true;
    if (mountedRef.current) setCloudLoading(true);
    fetchCloudProviders()
      .then(providers => {
        if (!active || !mountedRef.current) return;
        setCloudProviders(providers || []);
      })
      .catch(() => {
        if (active && mountedRef.current) setCloudProviders([]);
      })
      .finally(() => {
        if (active && mountedRef.current) setCloudLoading(false);
      });
    return () => { active = false; };
  }, [demoMode]);

  // Known model names for the fallback-chain comboboxes - relocated from
  // Settings.tsx. Suggestions only, never enforced.
  useEffect(() => {
    if (demoMode) {
      if (mountedRef.current) setKnownModelNames((mockModelCatalog.models || []).map((m) => m.name));
      return;
    }
    let active = true;
    fetchModels().then((data) => {
      if (!active || !mountedRef.current) return;
      setKnownModelNames((data.models || []).map((m) => m.name));
    }).catch(() => {});
    return () => { active = false; };
  }, [demoMode]);

  // Local fallback chains load - relocated from Settings.tsx. Settings
  // read this from fetchSettings().routing.local_degradation_chains; same here.
  // liteLLMEnabled is read-only here (owned by Settings) and only gates the
  // Cloud Providers card subtitle/Add button, exactly as Settings did.
  useEffect(() => {
    if (demoMode) {
      if (mountedRef.current) {
        setLocalDegradationChains({});
        setLiteLLMEnabled(false);
        setChainsLoading(false);
      }
      return;
    }
    let active = true;
    if (mountedRef.current) setChainsLoading(true);
    fetchSettings()
      .then(settingsData => {
        if (!active || !mountedRef.current) return;
        setLocalDegradationChains(settingsData.routing?.local_degradation_chains || {});
        setLiteLLMEnabled(settingsData.litellm?.enabled || false);
      })
      .catch(() => {
        if (active && mountedRef.current) {
          setLocalDegradationChains({});
          setLiteLLMEnabled(false);
        }
      })
      .finally(() => {
        if (active && mountedRef.current) setChainsLoading(false);
      });
    return () => { active = false; };
  }, [demoMode]);

  // Savings poll (5s), mirroring the cadence the card had on the dashboard.
  // Demo mode's fetchSavings already returns static data, but the interval is
  // still skipped there so the page makes zero network calls offline.
  useEffect(() => {
    if (demoMode) return;
    let active = true;
    const loadSavings = async () => {
      if (active) setSavingsLoading(true);
      try {
        const data = await fetchSavings();
        if (active) setSavings(data);
      } catch {
        if (active) setSavings(null);
      } finally {
        if (active) setSavingsLoading(false);
      }
    };
    loadSavings();
    const interval = setInterval(loadSavings, 5000);
    return () => {
      active = false;
      clearInterval(interval);
    };
  }, [demoMode]);

  // Returns whether the change succeeded, so the confirm button below can
  // close the modal only on success - strategyError is rendered inside that
  // same modal, so unconditionally closing it on both outcomes (the modal's
  // own pre-existing behavior) would silently swallow a failure the instant
  // it's set, with no page-level banner to fall back on since this error is
  // deliberately scoped away from the shared one.
  const handleStrategyChange = async (strategy: string): Promise<boolean> => {
    if (demoMode) {
      setCurrentStrategyState(strategy);
      return true;
    }

    try {
      await setRoutingStrategy(strategy);
      if (!mountedRef.current) return true;
      setCurrentStrategyState(strategy);
      setStrategyError(null);
      return true;
    } catch (err: any) {
      if (mountedRef.current) setStrategyError(err.message);
      return false;
    }
  };

  const handleToggleRule = async () => {
    if (!ruleToToggle) return;
    const id = ruleToToggle.id;

    if (demoMode) {
      setRules(rules.map(r => r.id === id ? { ...r, enabled: !r.enabled } : r));
    } else {
      try {
        await toggleRoutingRule(id);
        await loadData();
      } catch (err: any) {
        if (mountedRef.current) setError(err.message);
      }
    }

    if (mountedRef.current) setRuleToToggle(null);
  };

  const handleCreateRule = async () => {
    const errors: string[] = [];
    if (!newRuleForm.priority) errors.push('Priority is required');
    const parsedPriority = parseInt(newRuleForm.priority, 10);
    if (newRuleForm.priority && !(Number.isInteger(parsedPriority) && parsedPriority >= 1)) {
      errors.push('Priority must be a positive integer');
    }
    if (!newRuleForm.condition) errors.push('Condition is required');
    if (!newRuleForm.targetNode) errors.push('Target node is required');

    if (errors.length > 0) {
      setFormErrors(errors);
      return;
    }

    const newRule: RoutingRule = {
      id: `rule-${Date.now()}`,
      priority: parsedPriority,
      condition: newRuleForm.condition,
      targetNode: newRuleForm.targetNode,
      strategy: newRuleForm.strategy,
      enabled: true,
    };

    if (demoMode) {
      setRules([...rules, newRule].sort((a, b) => a.priority - b.priority));
    } else {
      try {
        await addRoutingRule(newRule);
        await loadData();
      } catch (err: any) {
        if (mountedRef.current) setFormErrors([err.message]);
        return;
      }
    }

    if (!mountedRef.current) return;
    setIsCreateModalOpen(false);
    setNewRuleForm({ priority: '', condition: '', targetNode: '', strategy: 'warm-first' });
    setFormErrors([]);
  };

  const handleDeleteRule = async () => {
    if (!ruleToDelete) return;
    
    if (demoMode) {
      setRules(rules.filter(r => r.id !== ruleToDelete.id));
    } else {
      try {
        await removeRoutingRule(ruleToDelete.id);
        await loadData();
      } catch (err: any) {
        if (mountedRef.current) setError(err.message);
      }
    }

    if (!mountedRef.current) return;
    setIsDeleteModalOpen(false);
    setRuleToDelete(null);
  };

  // Cloud provider handlers - relocated verbatim from Settings.tsx,
  // only the unmount guard adapted (Settings used fire-and-forget with a
  // refresh; here guarded by mountedRef like the rest of this page).
  const emptyProvider: CloudProviderInput = { name: '', provider: 'openai', base_url: '', api_key: '', default_model: '', cost_per_1k_tokens: 0, enabled: false, priority: 0 };

  const refreshCloudProviders = async () => {
    try {
      const providers = await fetchCloudProviders();
      if (mountedRef.current) setCloudProviders(providers);
    } catch { /* keep showing the last known list on a transient fetch error */ }
  };

  const openAddCloudProvider = () => {
    setEditingProvider({ ...emptyProvider });
    setCloudError(null);
    setCloudModalOpen(true);
  };

  const openEditCloudProvider = (p: CloudProvider) => {
    setEditingProvider({ name: p.name, provider: p.provider, base_url: p.base_url, api_key: '***', default_model: p.default_model, cost_per_1k_tokens: p.cost_per_1k_tokens, enabled: p.enabled, priority: p.priority });
    setCloudError(null);
    setCloudModalOpen(true);
  };

  const moveCloudProvider = async (index: number, direction: -1 | 1) => {
    const target = index + direction;
    if (target < 0 || target >= cloudProviders.length) return;
    const reordered = [...cloudProviders];
    [reordered[index], reordered[target]] = [reordered[target], reordered[index]];
    setCloudProviders(reordered); // optimistic
    try {
      await reorderCloudProviders(reordered.map(p => p.name));
    } catch (err: any) {
      if (mountedRef.current) setError(err.message || 'Failed to reorder cloud providers');
      await refreshCloudProviders(); // revert to server truth on failure
    }
  };

  const handleSaveCloudProvider = async (isNew: boolean) => {
    if (!editingProvider) return;
    if (!editingProvider.name.trim() || !editingProvider.provider.trim() || !editingProvider.base_url.trim()) {
      setCloudError('Name, provider, and base URL are required');
      return;
    }
    if (isNew && !editingProvider.api_key.trim()) {
      setCloudError('API key is required');
      return;
    }
    setCloudError(null);
    // Only a real, freshly-typed key is testable - '***' means the operator
    // left the existing stored key unchanged, so there's nothing new to verify.
    const hasNewKey = editingProvider.api_key.trim() !== '' && editingProvider.api_key !== '***';
    if (hasNewKey) {
      setCloudTesting(true);
      try {
        await testCloudProvider(editingProvider.provider, editingProvider.base_url, editingProvider.api_key);
      } catch (err: any) {
        if (mountedRef.current) setCloudError(err.message || 'Could not verify API key');
        if (mountedRef.current) setCloudTesting(false);
        return;
      }
      if (mountedRef.current) setCloudTesting(false);
    }
    setCloudSaving(true);
    try {
      if (isNew) {
        await addCloudProvider(editingProvider);
      } else {
        await updateCloudProvider(editingProvider.name, editingProvider);
      }
      if (!mountedRef.current) return;
      setCloudModalOpen(false);
      setEditingProvider(null);
      await refreshCloudProviders();
    } catch (err: any) {
      if (mountedRef.current) setCloudError(err.message || 'Failed to save cloud provider');
    } finally {
      if (mountedRef.current) setCloudSaving(false);
    }
  };

  const handleDeleteCloudProvider = async () => {
    if (!providerToDelete) return;
    try {
      await deleteCloudProvider(providerToDelete);
      if (!mountedRef.current) return;
      setProviderToDelete(null);
      await refreshCloudProviders();
    } catch (err: any) {
      if (!mountedRef.current) return;
      setError(err.message || 'Failed to delete cloud provider');
      setProviderToDelete(null);
    }
  };

  const persistChains = async (next: Record<string, string[]>) => {
    setChainsSaving(true);
    try {
      await updateSettings({ routing: { local_degradation_chains: next } });
      if (!mountedRef.current) return true;
      setLocalDegradationChains(next);
      return true;
    } catch (err: any) {
      if (mountedRef.current) setError(err.message || 'Failed to save fallback chain');
      return false;
    } finally {
      if (mountedRef.current) setChainsSaving(false);
    }
  };

  const handleAddDegChain = async () => {
    const model = newDegModel.trim();
    const alts = newDegAlts.split(',').map((s) => s.trim()).filter(Boolean);
    if (!model || alts.length === 0) return;
    // Mirror config.go Validate()'s local_degradation_chains rules
    // client-side so a nonsensical chain never even makes it into
    // the pending list - previously this only surfaced as a
    // confusing "validation failed" error on Save.
    // (Relocated verbatim from Settings.tsx.)
    if (alts.includes(model)) {
      setDegChainError(`"${model}" lists itself as an alternate`);
      return;
    }
    const seen = new Set<string>();
    for (const alt of alts) {
      if (seen.has(alt)) {
        setDegChainError(`"${model}" lists "${alt}" more than once`);
        return;
      }
      seen.add(alt);
    }
    setDegChainError(null);
    const next = { ...localDegradationChains, [model]: alts };
    if (demoMode) {
      setLocalDegradationChains(next);
      setNewDegModel('');
      setNewDegAlts('');
      return;
    }
    const ok = await persistChains(next);
    if (ok && mountedRef.current) {
      setNewDegModel('');
      setNewDegAlts('');
    }
  };

  const handleDeleteDegChain = async (model: string) => {
    const next = { ...localDegradationChains };
    delete next[model];
    if (demoMode) {
      setLocalDegradationChains(next);
      return;
    }
    await persistChains(next);
  };

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4 border-b border-border pb-6">
        <div>
          <h1 className="text-2xl font-bold text-foreground">Routing Logic</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Configure how requests are balanced across your cluster
          </p>
        </div>
      </div>

      {error && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">
          {error}
        </div>
      )}

      {/* Saved vs Cloud - shared component, also shown on the dashboard next
          to Fleet Capacity. Here it sits next to the strategy that drives the
          local/cloud split (same /admin/metrics/savings endpoint). */}
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
        <SavingsCard savings={savings} loading={savingsLoading} />
      </div>

      {/* Global Strategy Selection */}
      <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
        {STRATEGIES.map((strategy) => (
          <button
            key={strategy.value}
            onClick={() => { if (strategy.value !== currentStrategy) { setStrategyError(null); setStrategyToConfirm(strategy.value); } }}
            className={`flex flex-col p-5 rounded-xl border text-left transition-colors ${
              currentStrategy === strategy.value
                ? 'bg-primary/5 border-primary shadow-sm'
                : 'bg-card border-border hover:border-border/80 shadow-sm'
            }`}
          >
            <div className={`p-2 rounded-lg mb-4 w-fit ${
              currentStrategy === strategy.value ? 'bg-primary text-primary-foreground' : 'bg-secondary text-muted-foreground'
            }`}>
              {strategy.icon}
            </div>
            <h3 className="font-semibold text-foreground mb-1">{strategy.label}</h3>
            <p className="text-sm text-muted-foreground leading-relaxed">
              {strategy.description}
            </p>
            {currentStrategy === strategy.value && (
              <div className="mt-4 flex items-center gap-1.5 text-xs text-primary font-medium">
                <Check className="w-4 h-4" />
                Active Strategy
              </div>
            )}
          </button>
        ))}
      </div>

      {!demoMode && !loading && currentStrategy === '' && (
        <div className="p-3 bg-amber-500/10 border border-amber-500/30 rounded-xl text-amber-600 dark:text-amber-400 text-sm font-medium">
          Could not read strategy from backend - no strategy is currently selected
        </div>
      )}

      {/* Advanced Rules Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4 pt-6">
        <div>
          <h2 className="text-lg font-semibold text-foreground">Override Rules</h2>
          <p className="text-sm text-muted-foreground">Fine-grained control for specific models or API keys</p>
        </div>
        <button
          onClick={() => setIsCreateModalOpen(true)}
          className="flex items-center gap-2 px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm self-start sm:self-auto"
        >
          <Plus className="w-4 h-4" />
          Add Rule
        </button>
      </div>

      {/* Rules Table */}
      <div className="hidden md:block bg-card border border-border shadow-sm rounded-xl overflow-hidden">
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="bg-secondary/30 border-b border-border text-muted-foreground">
                <th className="px-6 py-3 text-left font-medium">Priority</th>
                <th className="px-6 py-3 text-left font-medium">Condition</th>
                <th className="px-6 py-3 text-left font-medium">Target</th>
                <th className="px-6 py-3 text-left font-medium">Strategy</th>
                <th className="px-6 py-3 text-center font-medium">Status</th>
                <th className="px-6 py-3 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {rules.map((rule) => (
                <tr key={rule.id} className={`${rule.enabled ? 'opacity-100' : 'opacity-50'} hover:bg-secondary/30 transition-colors`}>
                  <td className="px-6 py-4">
                    <span className="font-mono font-medium text-muted-foreground">#{rule.priority}</span>
                  </td>
                  <td className="px-6 py-4">
                    <code className="text-xs px-2 py-1 bg-secondary rounded-md border border-border font-mono">
                      {rule.condition}
                    </code>
                  </td>
                  <td className="px-6 py-4">
                    <div className="flex items-center gap-2">
                      <Server className="w-4 h-4 text-muted-foreground" />
                      <span className="font-medium text-foreground">{rule.targetNode}</span>
                    </div>
                  </td>
                  <td className="px-6 py-4">
                    <Badge variant="primary" size="sm">
                      {STRATEGIES.find(s => s.value === rule.strategy)?.label || rule.strategy}
                    </Badge>
                  </td>
                  <td className="px-6 py-4 text-center">
                    <button
                      onClick={() => setRuleToToggle(rule)}
                      className={`inline-flex items-center justify-center w-8 h-8 rounded-lg transition-colors ${
                        rule.enabled
                          ? 'bg-primary/10 text-primary'
                          : 'bg-muted text-muted-foreground'
                      }`}
                    >
                      {rule.enabled ? <Check className="w-4 h-4" /> : <span className="text-xs font-bold">✕</span>}
                    </button>
                  </td>
                  <td className="px-6 py-4 text-right">
                    <button
                      onClick={() => {
                        setRuleToDelete(rule);
                        setIsDeleteModalOpen(true);
                      }}
                      className="p-2 text-muted-foreground hover:text-destructive transition-colors"
                    >
                      <Trash2 className="w-4 h-4" />
                    </button>
                  </td>
                </tr>
              ))}
              {loading && rules.length === 0 && (
                <tr>
                  <td colSpan={6} className="px-6 py-8 text-center text-muted-foreground">
                    Loading...
                  </td>
                </tr>
              )}
              {!loading && rules.length === 0 && (
                <tr>
                  <td colSpan={6} className="px-6 py-8 text-center text-muted-foreground">
                    No override rules configured.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </div>

      {/* Rules Card List (mobile) */}
      <div className="md:hidden space-y-3">
        {loading && rules.length === 0 && (
          <div className="bg-card/50 backdrop-blur-sm border border-border/60 rounded-xl p-4 text-center text-sm text-muted-foreground">
            Loading...
          </div>
        )}
        {!loading && rules.length === 0 && (
          <div className="bg-card/50 backdrop-blur-sm border border-border/60 rounded-xl p-4 text-center text-sm text-muted-foreground">
            No override rules configured.
          </div>
        )}
        {rules.map((rule) => (
          <div
            key={rule.id}
            className={`${rule.enabled ? 'opacity-100' : 'opacity-50'} bg-card/50 backdrop-blur-sm border border-border/60 rounded-xl p-4 space-y-3`}
          >
            <div className="flex items-start justify-between gap-3">
              <div>
                <p className="text-[10px] uppercase tracking-wider text-muted-foreground">Priority</p>
                <span className="text-sm text-foreground font-mono font-medium">#{rule.priority}</span>
              </div>
              <div className="flex items-center gap-1">
                <button
                  onClick={() => setRuleToToggle(rule)}
                  className={`inline-flex items-center justify-center w-8 h-8 rounded-lg transition-colors ${
                    rule.enabled
                      ? 'bg-primary/10 text-primary'
                      : 'bg-muted text-muted-foreground'
                  }`}
                >
                  {rule.enabled ? <Check className="w-4 h-4" /> : <span className="text-xs font-bold">✕</span>}
                </button>
                <button
                  onClick={() => {
                    setRuleToDelete(rule);
                    setIsDeleteModalOpen(true);
                  }}
                  className="p-2 text-muted-foreground hover:text-destructive transition-colors"
                >
                  <Trash2 className="w-4 h-4" />
                </button>
              </div>
            </div>
            <div>
              <p className="text-[10px] uppercase tracking-wider text-muted-foreground mb-1">Condition</p>
              <code className="text-xs px-2 py-1 bg-secondary rounded-md border border-border font-mono block w-fit max-w-full overflow-x-auto">
                {rule.condition}
              </code>
            </div>
            <div className="flex items-center justify-between gap-3">
              <div>
                <p className="text-[10px] uppercase tracking-wider text-muted-foreground mb-1">Target</p>
                <div className="flex items-center gap-2">
                  <Server className="w-4 h-4 text-muted-foreground" />
                  <span className="text-sm text-foreground font-medium">{rule.targetNode}</span>
                </div>
              </div>
              <div>
                <p className="text-[10px] uppercase tracking-wider text-muted-foreground mb-1">Strategy</p>
                <Badge variant="primary" size="sm">
                  {STRATEGIES.find(s => s.value === rule.strategy)?.label || rule.strategy}
                </Badge>
              </div>
            </div>
          </div>
        ))}
      </div>

      {/* Cloud Providers - relocated verbatim from Settings.tsx.
          Fallback cloud endpoints for overflow traffic, tried highest priority
          first. Provider priority is a routing decision, not general app config. */}
      <div className="bg-card border border-border shadow-sm rounded-xl p-6">
        <div className="flex items-center justify-between mb-5">
          <div className="flex items-center gap-3">
            <div className="p-2 bg-sky-500/10 rounded-lg">
              <Cloud className="w-5 h-5 text-sky-600 dark:text-sky-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Cloud Providers</h3>
              <p className="text-xs font-medium text-muted-foreground">
                {liteLLMEnabled
                  ? 'Managed by LiteLLM while enabled - this list is inactive'
                  : 'Fallback cloud endpoints for overflow traffic, tried highest priority first'}
              </p>
            </div>
          </div>
          {!liteLLMEnabled && (
            <button
              onClick={openAddCloudProvider}
              className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium bg-primary hover:bg-primary/90 text-primary-foreground rounded-lg transition-colors"
            >
              <Plus className="w-3.5 h-3.5" />
              Add Provider
            </button>
          )}
        </div>

        <div className={liteLLMEnabled ? 'opacity-40 pointer-events-none' : ''}>
          {cloudLoading ? (
            <div className="space-y-3">
              {[1, 2].map(i => (
                <div key={i} className="h-14 bg-secondary/30 rounded-lg animate-pulse" />
              ))}
            </div>
          ) : cloudProviders.length === 0 ? (
            <div className="py-8 text-center text-sm font-medium text-muted-foreground">
              No cloud providers configured
            </div>
          ) : (
            <div className="space-y-3">
              {cloudProviders.map((provider, index) => (
                <div key={provider.name} className="flex flex-col sm:flex-row sm:items-center justify-between gap-3 p-3 rounded-lg border border-border bg-secondary/30 min-w-0">
                  <div className="flex items-center gap-3 min-w-0 flex-1">
                    <div className="flex flex-col items-center gap-0.5 w-5 text-[10px] font-medium text-muted-foreground shrink-0">
                      <span>{index + 1}</span>
                    </div>
                    <StatusDot status={provider.enabled ? 'online' : 'offline'} size="sm" />
                    <div className="min-w-0 flex-1">
                      <p className="text-sm font-medium text-foreground truncate" title={provider.name}>{provider.name}</p>
                      <p className="text-xs font-medium text-muted-foreground truncate" title={`${provider.default_model} - $${provider.cost_per_1k_tokens.toFixed(4)}/1k tokens`}>{provider.default_model} - ${provider.cost_per_1k_tokens.toFixed(4)}/1k tokens</p>
                    </div>
                  </div>
                  <div className="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                    <Badge variant={provider.enabled ? 'success' : 'muted'} size="sm">
                      {provider.provider}
                    </Badge>
                    <>
                      <button onClick={() => moveCloudProvider(index, -1)} disabled={index === 0} className="p-1.5 text-muted-foreground hover:text-foreground rounded-md hover:bg-secondary transition-colors disabled:opacity-30 disabled:pointer-events-none">
                        <ChevronUp className="w-3.5 h-3.5" />
                      </button>
                      <button onClick={() => moveCloudProvider(index, 1)} disabled={index === cloudProviders.length - 1} className="p-1.5 text-muted-foreground hover:text-foreground rounded-md hover:bg-secondary transition-colors disabled:opacity-30 disabled:pointer-events-none">
                        <ChevronDown className="w-3.5 h-3.5" />
                      </button>
                      <button onClick={() => openEditCloudProvider(provider)} className="p-1.5 text-muted-foreground hover:text-foreground rounded-md hover:bg-secondary transition-colors">
                        <Pencil className="w-3.5 h-3.5" />
                      </button>
                      <button onClick={() => setProviderToDelete(provider.name)} className="p-1.5 text-muted-foreground hover:text-destructive rounded-md hover:bg-secondary transition-colors">
                        <Trash2 className="w-3.5 h-3.5" />
                      </button>
                    </>
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>

      {/* Local Model Fallback Chain - relocated verbatim from Settings.tsx. */}
      <div className="bg-card border border-border shadow-sm rounded-xl p-6">
        <div className="flex items-center gap-3 mb-5">
          <div className="p-2 bg-teal-500/10 rounded-lg">
            <HardDrive className="w-5 h-5 text-teal-600 dark:text-teal-400" />
          </div>
          <div>
            <h3 className="text-sm font-semibold text-foreground">Local Model Fallback Chain</h3>
            <p className="text-xs font-medium text-muted-foreground">Ordered local alternates to try when no node can serve the requested model, before cloud - only applies to keys with "Allow local degradation" enabled (API Keys page)</p>
          </div>
        </div>

        {chainsLoading ? (
          <div className="space-y-2 mb-4">
            {[1, 2].map(i => (
              <div key={i} className="h-14 bg-secondary/30 rounded-lg animate-pulse" />
            ))}
          </div>
        ) : (
        <div className="space-y-2 mb-4">
          {Object.entries(localDegradationChains).length === 0 ? (
            <p className="text-sm text-muted-foreground py-2">No local fallback chains declared</p>
          ) : (
            Object.entries(localDegradationChains).map(([model, alts]) => (
              <div key={model} className="flex items-center justify-between gap-3 p-2.5 rounded-lg border border-border bg-secondary/30 min-w-0">
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium text-foreground truncate" title={model}>{model}</p>
                  <p className="text-xs text-muted-foreground truncate" title={alts.join(' -> ')}>{alts.join(' -> ')}</p>
                </div>
                <button
                  onClick={() => handleDeleteDegChain(model)}
                  disabled={chainsSaving}
                  className="p-1.5 text-muted-foreground hover:text-destructive rounded-md hover:bg-secondary transition-colors disabled:opacity-50"
                >
                  <Trash2 className="w-3.5 h-3.5" />
                </button>
              </div>
            ))
          )}
        </div>
        )}

        <div className="flex flex-col sm:flex-row gap-2">
          <CustomCombobox value={newDegModel} onChange={(v) => { setNewDegModel(v); setDegChainError(null); }} options={knownModelNames} placeholder="llama3.1:70b" className="sm:flex-1" />
          <CustomTagCombobox value={newDegAlts} onChange={(v) => { setNewDegAlts(v); setDegChainError(null); }} options={knownModelNames} placeholder="llama3.1:8b, phi3:mini" className="sm:flex-1" />
          <button
            onClick={handleAddDegChain}
            disabled={chainsSaving}
            className="flex items-center justify-center gap-1.5 px-3 py-2 bg-primary hover:bg-primary/90 text-primary-foreground text-sm font-medium rounded-lg transition-colors disabled:opacity-50"
          >
            <Plus className="w-4 h-4" />
          </button>
        </div>
        {degChainError && <p className="text-sm text-destructive mt-2">{degChainError}</p>}
      </div>

      {/* Create Rule Modal */}
      <Modal
        isOpen={isCreateModalOpen}
        onClose={() => {
          setIsCreateModalOpen(false);
          setFormErrors([]);
          setNewRuleForm({ priority: '', condition: '', targetNode: '', strategy: 'warm-first' });
        }}
        title="Add Routing Rule"
      >
        <div className="space-y-4">
          {formErrors.length > 0 && (
            <div className="p-3 bg-destructive/10 border border-destructive/20 rounded-lg">
              {formErrors.map((error, i) => (
                <p key={i} className="text-sm font-medium text-destructive">{error}</p>
              ))}
            </div>
          )}
          
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Priority <span className="text-destructive">*</span>
              </label>
              <input
                type="number"
                value={newRuleForm.priority}
                onChange={(e) => setNewRuleForm({ ...newRuleForm, priority: e.target.value })}
                placeholder="1"
                min="1"
                className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Target Node <span className="text-destructive">*</span>
              </label>
              <CustomSelect
                value={newRuleForm.targetNode}
                onChange={(val) => setNewRuleForm({ ...newRuleForm, targetNode: val })}
                placeholder="Select node..."
                options={[
                  { value: '', label: 'Select node...' },
                  { value: 'any', label: 'Any Node (Dynamic Balancing)' },
                  ...availableNodes.map((node) => ({ value: node.name, label: node.name })),
                ]}
              />
            </div>
          </div>
          
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Condition <span className="text-destructive">*</span>
            </label>
            <input
              type="text"
              value={newRuleForm.condition}
              onChange={(e) => setNewRuleForm({ ...newRuleForm, condition: e.target.value })}
              placeholder='e.g., model =~ "70b" or api_key == "sk-prod-*"'
              className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
            />
            <p className="text-xs text-muted-foreground mt-1.5">
              Use <code className="text-primary font-medium">=~</code> for regex match, <code className="text-primary font-medium">==</code> for exact match
            </p>
          </div>
          
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Routing Strategy
            </label>
            <div className="space-y-2">
              {STRATEGIES.map((strategy) => (
                <label
                  key={strategy.value}
                  className={`flex items-center gap-3 p-3 rounded-lg border cursor-pointer transition-colors ${
                    newRuleForm.strategy === strategy.value
                      ? 'border-primary/50 bg-primary/5'
                      : 'border-border bg-secondary hover:border-border/80'
                  }`}
                >
                  <input
                    type="radio"
                    name="strategy"
                    value={strategy.value}
                    checked={newRuleForm.strategy === strategy.value}
                    onChange={(e) => setNewRuleForm({ ...newRuleForm, strategy: e.target.value as any })}
                    className="accent-primary"
                  />
                  <div>
                    <p className="text-sm font-medium text-foreground">{strategy.label}</p>
                    <p className="text-xs text-muted-foreground">{strategy.description}</p>
                  </div>
                </label>
              ))}
            </div>
          </div>
          
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setIsCreateModalOpen(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleCreateRule}
              className="px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Add Rule
            </button>
          </div>
        </div>
      </Modal>

      {/* Delete Confirmation Modal */}
      <Modal
        isOpen={isDeleteModalOpen}
        onClose={() => {
          setIsDeleteModalOpen(false);
          setRuleToDelete(null);
        }}
        title="Delete Routing Rule"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Are you sure you want to delete the routing rule with priority{' '}
            <span className="text-foreground font-medium">#{ruleToDelete?.priority}</span>?
          </p>
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setIsDeleteModalOpen(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleDeleteRule}
              className="px-4 py-2 bg-destructive hover:bg-destructive/90 text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Delete Rule
            </button>
          </div>
        </div>
      </Modal>

      {/* Toggle Rule Confirmation Modal */}
      <Modal
        isOpen={ruleToToggle !== null}
        onClose={() => setRuleToToggle(null)}
        title={ruleToToggle?.enabled ? 'Disable Routing Rule' : 'Enable Routing Rule'}
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Are you sure you want to {ruleToToggle?.enabled ? 'disable' : 'enable'} the routing rule with priority{' '}
            <span className="text-foreground font-medium">#{ruleToToggle?.priority}</span>?
          </p>
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setRuleToToggle(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleToggleRule}
              className="px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              {ruleToToggle?.enabled ? 'Disable Rule' : 'Enable Rule'}
            </button>
          </div>
        </div>
      </Modal>

      {/* Strategy Change Confirmation Modal */}
      <Modal
        isOpen={strategyToConfirm !== null}
        onClose={() => setStrategyToConfirm(null)}
        title="Change Routing Strategy"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Are you sure you want to switch the routing strategy from{' '}
            <span className="text-foreground font-semibold">{STRATEGIES.find((s) => s.value === currentStrategy)?.label ?? 'none'}</span> to{' '}
            <span className="text-foreground font-semibold">{STRATEGIES.find((s) => s.value === strategyToConfirm)?.label}</span>?
          </p>
          <p className="text-xs text-muted-foreground">
            This changes how every request across the entire marbor is load-balanced, effective immediately for all live traffic.
          </p>
          {strategyError && (
            <p className="text-sm text-destructive">{strategyError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setStrategyToConfirm(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={async () => {
                if (!strategyToConfirm) return;
                const succeeded = await handleStrategyChange(strategyToConfirm);
                if (succeeded) setStrategyToConfirm(null);
              }}
              className="px-4 py-2 bg-amber-600 hover:bg-amber-600/90 text-white font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Change Strategy
            </button>
          </div>
        </div>
      </Modal>

      {/* Add/Edit Cloud Provider Modal - relocated verbatim from Settings.tsx. */}
      <Modal
        isOpen={cloudModalOpen}
        onClose={() => { setCloudModalOpen(false); setEditingProvider(null); }}
        title={editingProvider && cloudProviders.some(p => p.name === editingProvider.name) ? 'Edit Cloud Provider' : 'Add Cloud Provider'}
        maxWidth="sm"
      >
        {editingProvider && (
          <div className="space-y-4">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Name <span className="text-destructive">*</span>
              </label>
              <input
                type="text"
                value={editingProvider.name}
                disabled={cloudProviders.some(p => p.name === editingProvider.name)}
                onChange={(e) => setEditingProvider({ ...editingProvider, name: e.target.value })}
                placeholder="openai-prod"
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50 disabled:opacity-50"
              />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Provider <span className="text-destructive">*</span>
              </label>
              <CustomSelect
                value={editingProvider.provider}
                onChange={(val) => {
                  const provider = val;
                  const preset = CLOUD_PROVIDER_PRESETS[provider];
                  setEditingProvider({
                    ...editingProvider,
                    provider,
                    base_url: provider === 'custom' ? '' : (preset?.baseUrl || editingProvider.base_url || ''),
                    default_model: provider === 'custom' ? '' : (preset?.defaultModel || editingProvider.default_model || ''),
                  });
                }}
                options={Object.entries(CLOUD_PROVIDER_PRESETS).map(([value, preset]) => ({ value, label: preset.label }))}
              />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Base URL <span className="text-destructive">*</span>
              </label>
              <input type="text" value={editingProvider.base_url} onChange={(e) => setEditingProvider({ ...editingProvider, base_url: e.target.value })} placeholder="https://api.openai.com/v1" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                API Key {!cloudProviders.some(p => p.name === editingProvider.name) && <span className="text-destructive">*</span>}
              </label>
              <input type="password" value={editingProvider.api_key} onChange={(e) => setEditingProvider({ ...editingProvider, api_key: e.target.value })} autoComplete="off" placeholder={cloudProviders.some(p => p.name === editingProvider.name) ? 'Leave unchanged to keep current key' : ''} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
            </div>
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">Default Model</label>
                <input type="text" value={editingProvider.default_model} onChange={(e) => setEditingProvider({ ...editingProvider, default_model: e.target.value })} placeholder="gpt-4o" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
              </div>
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">Cost / 1k tokens</label>
                <input type="number" step="0.0001" value={editingProvider.cost_per_1k_tokens} onChange={(e) => setEditingProvider({ ...editingProvider, cost_per_1k_tokens: parseFloat(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
              </div>
            </div>
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <p className="text-sm font-medium text-foreground">Enabled</p>
              <Toggle on={editingProvider.enabled} onToggle={() => setEditingProvider({ ...editingProvider, enabled: !editingProvider.enabled })} />
            </div>
            {cloudError && <p className="text-sm text-destructive">{cloudError}</p>}
            <div className="flex justify-end gap-3 pt-4 border-t border-border">
              <button onClick={() => { setCloudModalOpen(false); setEditingProvider(null); }} disabled={cloudSaving || cloudTesting} className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors disabled:opacity-50">
                Cancel
              </button>
              <button
                onClick={() => handleSaveCloudProvider(!cloudProviders.some(p => p.name === editingProvider.name))}
                disabled={cloudSaving || cloudTesting}
                className="flex items-center gap-2 px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm disabled:opacity-50"
              >
                {(cloudSaving || cloudTesting) && <RefreshCw className="w-3.5 h-3.5 animate-spin" />}
                {cloudTesting ? 'Testing key...' : cloudSaving ? 'Saving...' : 'Save'}
              </button>
            </div>
          </div>
        )}
      </Modal>

      <Modal
        isOpen={!!providerToDelete}
        onClose={() => setProviderToDelete(null)}
        title="Delete Cloud Provider"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Delete cloud provider <span className="font-medium text-foreground">{providerToDelete}</span>? This cannot be undone.
          </p>
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button onClick={() => setProviderToDelete(null)} className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors">
              Cancel
            </button>
            <button onClick={handleDeleteCloudProvider} className="px-4 py-2 bg-destructive hover:bg-destructive/90 text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm">
              Delete
            </button>
          </div>
        </div>
      </Modal>
    </div>
  );
}

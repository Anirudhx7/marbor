import { useState, useEffect, useRef } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';
import { Save, Check, Terminal, Shield, Activity, MonitorPlay, Cloud, RefreshCw, KeyRound, DollarSign, Sliders, Lock, Container, Webhook, Network, Flame, Ruler, Plus, Pencil, Trash2, ChevronUp, ChevronDown, Gauge, HardDrive, Download } from 'lucide-react';
import { Badge } from '../components/Badge';
import { StatusDot } from '../components/StatusDot';
import { Modal } from '../components/Modal';
import { defaultSettings, mockCloudProviders } from '../lib/mockData';
import { fetchSettings, updateSettings, fetchCloudProviders, addCloudProvider, updateCloudProvider, deleteCloudProvider, testCloudProvider, reorderCloudProviders, reloadFromStore, changePassword, triggerBackupNow, fetchBackupList, restoreBackup, uploadBackup } from '../lib/api';
import type { Settings, CloudProvider, CloudProviderInput, BackupFileInfo } from '../types';
import { useDemoMode, currentAppPath } from '../hooks/useDemoMode';
import { useCurrency, CURRENCY_PRESETS } from '../hooks/useCurrency';
import { CustomSelect } from '../components/Select';

// Known cloud fallback providers. All use plain `Authorization: Bearer <key>`
// auth and an OpenAI-compatible /chat/completions schema, matching this
// mesh's proxy - Azure OpenAI is deliberately excluded (needs an `api-key`
// header + per-deployment URL, which this proxy doesn't support).
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

// Compact toggle switch shared by every boolean setting on this page.
function Toggle({ on, onToggle }: { on: boolean; onToggle: () => void }) {
  return (
    <button
      onClick={onToggle}
      className={`relative inline-flex h-6 w-11 items-center rounded-full transition-colors ${on ? 'bg-primary' : 'bg-muted-foreground/30'}`}
    >
      <span className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${on ? 'translate-x-6' : 'translate-x-1'}`} />
    </button>
  );
}

const getTimezoneOffsetMinutes = (tz: string): number => {
  if (tz === 'Local') return -999999;
  try {
    const formatter = new Intl.DateTimeFormat('en-US', {
      timeZone: tz,
      timeZoneName: 'shortOffset'
    });
    const parts = formatter.formatToParts(new Date());
    const offsetPart = parts.find(p => p.type === 'timeZoneName');
    const offset = offsetPart ? offsetPart.value : '';
    if (offset === 'GMT' || offset === 'UTC') return 0;
    const match = offset.match(/(?:GMT|UTC)([+-])(\d+)(?::(\d+))?/);
    if (match) {
      const sign = match[1] === '+' ? 1 : -1;
      const hours = parseInt(match[2], 10);
      const minutes = parseInt(match[3] || '0', 10);
      return sign * (hours * 60 + minutes);
    }
    return 0;
  } catch {
    return 0;
  }
};

const timezones: string[] = (() => {
  let list = ['Local'];
  try {
    list = ['Local', ...Intl.supportedValuesOf('timeZone')];
  } catch {
    list = [
      'Local', 'UTC', 'America/New_York', 'America/Los_Angeles', 'America/Chicago',
      'Europe/London', 'Europe/Paris', 'Asia/Kolkata', 'Asia/Tokyo', 'Asia/Shanghai',
      'Asia/Singapore', 'Australia/Sydney'
    ];
  }
  return list.sort((a, b) => {
    const offsetA = getTimezoneOffsetMinutes(a);
    const offsetB = getTimezoneOffsetMinutes(b);
    if (offsetA !== offsetB) {
      return offsetA - offsetB;
    }
    return a.localeCompare(b);
  });
})();

const getTimezoneLabel = (tz: string): string => {
  if (tz === 'Local') return 'Local (Server Timezone)';
  try {
    const formatter = new Intl.DateTimeFormat('en-US', {
      timeZone: tz,
      timeZoneName: 'shortOffset'
    });
    const parts = formatter.formatToParts(new Date());
    const offsetPart = parts.find(p => p.type === 'timeZoneName');
    const offset = offsetPart ? offsetPart.value : '';

    let formattedOffset = offset;
    if (offset === 'GMT' || offset === 'UTC') {
      formattedOffset = 'UTC+00:00';
    } else {
      const match = offset.match(/(?:GMT|UTC)([+-])(\d+)(?::(\d+))?/);
      if (match) {
        const sign = match[1];
        const hours = match[2].padStart(2, '0');
        const minutes = match[3] || '00';
        formattedOffset = `UTC${sign}${hours}:${minutes}`;
      }
    }
    const displayOffset = formattedOffset ? `(${formattedOffset}) ` : '';
    return `${displayOffset}${tz.replace(/_/g, ' ')}`;
  } catch {
    return tz.replace(/_/g, ' ');
  }
};

export function SettingsPage() {
  const { demoMode, setDemoMode } = useDemoMode();
  const location = useLocation();
  const navigate = useNavigate();
  const { currency, setCurrency, toDisplay, toUSD } = useCurrency();
  const roundDisplay = (n: number) => Math.round(n * 100) / 100;
  const [settings, setSettings] = useState<Settings>(defaultSettings);
  const [cloudProviders, setCloudProviders] = useState<CloudProvider[]>(demoMode ? mockCloudProviders : []);
  const [cloudLoading, setCloudLoading] = useState(!demoMode);
  const [saved, setSaved] = useState(false);
  const [reloaded, setReloaded] = useState(false);
  const [reloading, setReloading] = useState(false);
  const [reloadConfirmOpen, setReloadConfirmOpen] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Cloud providers CRUD
  const [cloudModalOpen, setCloudModalOpen] = useState(false);
  const [editingProvider, setEditingProvider] = useState<CloudProviderInput | null>(null);
  const [cloudSaving, setCloudSaving] = useState(false);
  const [cloudTesting, setCloudTesting] = useState(false);
  const [cloudError, setCloudError] = useState<string | null>(null);
  const [providerToDelete, setProviderToDelete] = useState<string | null>(null);

  // Model context windows
  const [newCtxModel, setNewCtxModel] = useState('');
  const [newCtxTokens, setNewCtxTokens] = useState('');

  // Admin credentials change
  const [credCurrentPw, setCredCurrentPw] = useState('');
  const [credNewPw, setCredNewPw] = useState('');
  const [credConfirmPw, setCredConfirmPw] = useState('');
  const [credSaving, setCredSaving] = useState(false);
  const [credError, setCredError] = useState<string | null>(null);
  const [credSaved, setCredSaved] = useState(false);

  // Backup & Restore
  const [backupDownloading, setBackupDownloading] = useState(false);
  const [backupDownloadError, setBackupDownloadError] = useState<string | null>(null);

  const handleBackupNow = async () => {
    setBackupDownloading(true);
    setBackupDownloadError(null);
    try {
      await triggerBackupNow();
    } catch (err: any) {
      setBackupDownloadError(err.message || 'Backup failed');
    } finally {
      setBackupDownloading(false);
    }
  };

  // Restore-from-backup picker
  const [backupList, setBackupList] = useState<BackupFileInfo[]>([]);
  const [backupListLoading, setBackupListLoading] = useState(false);
  const [selectedBackupName, setSelectedBackupName] = useState<string>('');
  const [restoreTarget, setRestoreTarget] = useState<BackupFileInfo | null>(null);
  const [restoring, setRestoring] = useState(false);
  const [restoreError, setRestoreError] = useState<string | null>(null);
  const [restoreInitiated, setRestoreInitiated] = useState(false);

  // Guards against the mount-time fetch and a later upload's own refetch
  // resolving out of order - only the result of the most recently issued
  // fetchBackupList() call is ever applied to state.
  const backupListRequestId = useRef(0);

  useEffect(() => {
    if (currentAppPath() !== '/settings') return;
    let active = true;
    const requestId = ++backupListRequestId.current;
    setBackupListLoading(true);
    fetchBackupList()
      .then(list => {
        if (!active || requestId !== backupListRequestId.current) return;
        setBackupList(list);
        setSelectedBackupName(prev => (prev && list.some(b => b.name === prev)) ? prev : (list[0]?.name || ''));
      })
      .catch(() => { if (active && requestId === backupListRequestId.current) setBackupList([]); })
      .finally(() => { if (active) setBackupListLoading(false); });
    return () => { active = false; };
  }, [demoMode, location.pathname]);

  const selectedBackup = backupList.find(b => b.name === selectedBackupName) || null;

  const [uploadingBackup, setUploadingBackup] = useState(false);
  const [uploadError, setUploadError] = useState<string | null>(null);

  const handleBackupFileChosen = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    e.target.value = ''; // allow re-selecting the same file later
    if (!file) return;
    setUploadingBackup(true);
    setUploadError(null);
    const requestId = ++backupListRequestId.current;
    try {
      const filename = await uploadBackup(file);
      const list = await fetchBackupList();
      if (requestId === backupListRequestId.current) {
        setBackupList(list);
        setSelectedBackupName(list.some(b => b.name === filename) ? filename : (list[0]?.name || ''));
      }
    } catch (err: any) {
      setUploadError(err.message || 'Upload failed');
    } finally {
      setUploadingBackup(false);
    }
  };

  const handleRestoreConfirm = async () => {
    if (!restoreTarget) return;
    setRestoring(true);
    setRestoreError(null);
    try {
      await restoreBackup(restoreTarget.name);
      setRestoreInitiated(true);
      setRestoreTarget(null);
    } catch (err: any) {
      setRestoreError(err.message || 'Restore failed');
    } finally {
      setRestoring(false);
    }
  };

  const formatBackupSize = (bytes: number): string => {
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  };

  useEffect(() => {
    if (currentAppPath() !== '/settings') return;
    let active = true;
    if (active && currentAppPath() === '/settings') {
      setCloudLoading(true);
    }
    Promise.all([fetchSettings(), fetchCloudProviders().catch(() => mockCloudProviders)])
      .then(([settingsData, providersData]) => {
        if (!active || currentAppPath() !== '/settings') return;
        setSettings({
          proxyPort: settingsData.proxy?.port || 11434,
          authMode: settingsData.auth?.enabled ? 'api-key' : 'no-auth',
          liteLLMEnabled: settingsData.litellm?.enabled || false,
          liteLLMEndpoint: settingsData.litellm?.url || '',
          liteLLMApiKey: settingsData.litellm?.api_key || '',
          pollingInterval: settingsData.routing?.poll_interval_ms || 2000,
          prometheusEnabled: settingsData.metrics?.enabled || false,
          prometheusPort: settingsData.metrics?.port || 9090,
          logLevel: settingsData.proxy?.log_level || 'info',
          timezone: settingsData.timezone || 'Local',
          cloudDailyUsdCap: settingsData.cloud_budget?.daily_usd_cap || 0,
          cloudMonthlyUsdCap: settingsData.cloud_budget?.monthly_usd_cap || 0,
          cloudSoftBudgetPct: settingsData.cloud_budget?.soft_budget_pct || 0,
          hideDemoBanner: settingsData.hide_demo_banner || false,
          hideBudgetBanner: settingsData.hide_budget_banner || false,
          huggingFaceToken: settingsData.huggingface?.token || '',
          allowManagementEndpoints: settingsData.routing?.allow_management_endpoints || false,

          adminBindAddress: settingsData.admin?.bind_address || ':8080',
          adminCorsOrigin: settingsData.admin?.cors_origin || '',
          proxyAccessLog: settingsData.proxy?.access_log !== false,
          proxyTrustProxyHeaders: settingsData.proxy?.trust_proxy_headers || false,

          routingFallback: settingsData.routing?.fallback || 'least-connections',
          routingUpstreamTimeoutMs: settingsData.routing?.upstream_timeout_ms || 120000,
          routingMaxRetries: settingsData.routing?.max_retries ?? 2,
          routingSessionAffinity: settingsData.routing?.session_affinity || false,
          routingSessionAffinityTtl: settingsData.routing?.session_affinity_ttl || '10m',
          routingNvidiaPollIntervalMs: settingsData.routing?.nvidia_poll_interval_ms || 30000,
          routingQueueMaxDepth: settingsData.routing?.queue_max_depth ?? 100,
          routingQueueTimeoutMs: settingsData.routing?.queue_timeout_ms || 30000,
          routingHealthFailureThreshold: settingsData.routing?.health_failure_threshold ?? 3,
          routingHealthSuccessThreshold: settingsData.routing?.health_success_threshold ?? 2,
          routingOverflowSlaMs: settingsData.routing?.overflow_sla_ms ?? 0,
          thermalWatchdogEnabled: settingsData.routing?.thermal_watchdog?.enabled || false,
          thermalWatchdogMaxTempCelsius: settingsData.routing?.thermal_watchdog?.max_temp_celsius || 0,
          thermalWatchdogConsecutiveBreaches: settingsData.routing?.thermal_watchdog?.consecutive_breaches ?? 3,

          dockerEnabled: settingsData.docker?.enabled || false,
          dockerSocket: settingsData.docker?.socket || '',
          dockerPollIntervalMs: settingsData.docker?.poll_interval_ms ?? 30000,

          auditEnabled: settingsData.audit?.enabled || false,
          auditRetentionDays: settingsData.audit?.retention_days ?? 30,
          systemAuditRetentionDays: settingsData.audit?.system_audit_retention_days ?? 0,
          webhookEnabled: settingsData.webhook?.enabled || false,
          webhookUrl: settingsData.webhook?.url || '',
          webhookSecret: settingsData.webhook?.secret || '',
          savingsReferenceCostPer1k: settingsData.savings?.reference_cost_per_1k ?? 0.002,

          warmupEnabled: settingsData.warmup?.enabled || false,
          warmupIntervalMs: settingsData.warmup?.interval_ms ?? 300000,
          warmupKeepAlive: settingsData.warmup?.keep_alive || '10m',

          contextWindows: settingsData.context_windows || {},

          backupEnabled: settingsData.backup?.enabled || false,
          backupIntervalHours: settingsData.backup?.interval_hours ?? 24,
          backupRetentionCount: settingsData.backup?.retention_count ?? 7,
          backupTargetDir: settingsData.backup?.target_dir || '',
          backupLastAt: settingsData.backup?.last_backup_at || undefined,
          backupLastError: settingsData.backup?.last_backup_error || '',
        });
        setCloudProviders(demoMode ? mockCloudProviders : (providersData || []));
        setError(null);
      })
      .catch(err => {
        if (!active || currentAppPath() !== '/settings') return;
        setError(err.message || 'Failed to load settings');
        setCloudProviders([]);
      })
      .finally(() => {
        if (active && currentAppPath() === '/settings') {
          setCloudLoading(false);
        }
      });
    return () => {
      active = false;
    };
  }, [demoMode, location.pathname]);

  const handleSave = async () => {
    try {
      // Map UI settings to backend config format (also used in demo mode → localStorage)
      const payload = {
        timezone: settings.timezone,
        proxy: { port: settings.proxyPort, log_level: settings.logLevel, access_log: settings.proxyAccessLog, trust_proxy_headers: settings.proxyTrustProxyHeaders },
        admin: { bind_address: settings.adminBindAddress, cors_origin: settings.adminCorsOrigin },
        auth: { enabled: settings.authMode === 'api-key' },
        routing: {
          poll_interval_ms: settings.pollingInterval,
          allow_management_endpoints: settings.allowManagementEndpoints || false,
          fallback: settings.routingFallback,
          upstream_timeout_ms: settings.routingUpstreamTimeoutMs,
          max_retries: settings.routingMaxRetries,
          session_affinity: settings.routingSessionAffinity,
          session_affinity_ttl: settings.routingSessionAffinityTtl,
          nvidia_poll_interval_ms: settings.routingNvidiaPollIntervalMs,
          queue_max_depth: settings.routingQueueMaxDepth,
          queue_timeout_ms: settings.routingQueueTimeoutMs,
          health_failure_threshold: settings.routingHealthFailureThreshold,
          health_success_threshold: settings.routingHealthSuccessThreshold,
          overflow_sla_ms: settings.routingOverflowSlaMs,
          thermal_watchdog: {
            enabled: settings.thermalWatchdogEnabled,
            max_temp_celsius: settings.thermalWatchdogMaxTempCelsius,
            consecutive_breaches: settings.thermalWatchdogConsecutiveBreaches,
          },
        },
        metrics: { enabled: settings.prometheusEnabled, port: settings.prometheusPort },
        litellm: { enabled: settings.liteLLMEnabled, url: settings.liteLLMEndpoint, api_key: settings.liteLLMApiKey },
        huggingface: { token: settings.huggingFaceToken || '' },
        cloud_budget: { daily_usd_cap: settings.cloudDailyUsdCap, monthly_usd_cap: settings.cloudMonthlyUsdCap, soft_budget_pct: settings.cloudSoftBudgetPct },
        hide_demo_banner: settings.hideDemoBanner || false,
        hide_budget_banner: settings.hideBudgetBanner || false,
        docker: { enabled: settings.dockerEnabled, socket: settings.dockerSocket, poll_interval_ms: settings.dockerPollIntervalMs },
        audit: {
          enabled: settings.auditEnabled,
          retention_days: settings.auditRetentionDays,
          system_audit_retention_days: settings.systemAuditRetentionDays,
        },
        webhook: { enabled: settings.webhookEnabled, url: settings.webhookUrl, secret: settings.webhookSecret },
        savings: { reference_cost_per_1k: settings.savingsReferenceCostPer1k },
        warmup: { enabled: settings.warmupEnabled, interval_ms: settings.warmupIntervalMs, keep_alive: settings.warmupKeepAlive },
        context_windows: settings.contextWindows,
        backup: {
          enabled: settings.backupEnabled,
          interval_hours: settings.backupIntervalHours,
          retention_count: settings.backupRetentionCount,
          target_dir: settings.backupTargetDir,
        },
      };

      await updateSettings(payload);
      window.dispatchEvent(new Event('ollama-mesh-settings-change'));
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
      setError(null);
    } catch (err: any) {
      setError(err instanceof TypeError
        ? 'Could not reach the mesh backend. Check that the process is running and reachable from this browser.'
        : (err.message || 'Failed to save settings'));
    }
  };

  const handleReload = async () => {
    if (demoMode) {
      setReloaded(true);
      setReloadConfirmOpen(false);
      setTimeout(() => setReloaded(false), 2000);
      return;
    }
    setReloading(true);
    try {
      await reloadFromStore();
      setReloaded(true);
      setError(null);
      setTimeout(() => setReloaded(false), 2000);
    } catch (err: any) {
      setError(err instanceof TypeError
        ? 'Could not reach the mesh backend. Check that the process is running and reachable from this browser.'
        : (err.message || 'Reload failed'));
    } finally {
      setReloading(false);
      setReloadConfirmOpen(false);
    }
  };

  const handleChangeCredentials = async () => {
    if (credNewPw && credNewPw !== credConfirmPw) {
      setCredError('New passwords do not match');
      return;
    }
    if (!credCurrentPw) {
      setCredError('Current password is required');
      return;
    }
    setCredSaving(true);
    setCredError(null);
    try {
      await changePassword(credCurrentPw, credNewPw || '');
      setCredSaved(true);
      setCredCurrentPw('');
      setCredNewPw('');
      setCredConfirmPw('');
      setTimeout(() => setCredSaved(false), 3000);
    } catch (err: any) {
      setCredError(err.message || 'Failed to update credentials');
    } finally {
      setCredSaving(false);
    }
  };

  const emptyProvider: CloudProviderInput = { name: '', provider: 'openai', base_url: '', api_key: '', default_model: '', cost_per_1k_tokens: 0, enabled: false, priority: 0 };

  const refreshCloudProviders = async () => {
    try {
      const providers = await fetchCloudProviders();
      setCloudProviders(providers);
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
      setError(err.message || 'Failed to reorder cloud providers');
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
        setCloudError(err.message || 'Could not verify API key');
        setCloudTesting(false);
        return;
      }
      setCloudTesting(false);
    }
    setCloudSaving(true);
    try {
      if (isNew) {
        await addCloudProvider(editingProvider);
      } else {
        await updateCloudProvider(editingProvider.name, editingProvider);
      }
      setCloudModalOpen(false);
      setEditingProvider(null);
      await refreshCloudProviders();
    } catch (err: any) {
      setCloudError(err.message || 'Failed to save cloud provider');
    } finally {
      setCloudSaving(false);
    }
  };

  const handleDeleteCloudProvider = async () => {
    if (!providerToDelete) return;
    try {
      await deleteCloudProvider(providerToDelete);
      setProviderToDelete(null);
      await refreshCloudProviders();
    } catch (err: any) {
      setError(err.message || 'Failed to delete cloud provider');
      setProviderToDelete(null);
    }
  };

  return (
    <>
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4 border-b border-border pb-6">
        <div>
          <h1 className="text-2xl font-bold text-foreground">Settings</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Configure proxy settings, authentication, and integrations
          </p>
        </div>
        <div className="flex items-center gap-2">
          <button
            onClick={() => setReloadConfirmOpen(true)}
            disabled={reloading}
            title="Reload config from disk without restarting (equivalent to SIGHUP)"
            className="flex items-center gap-2 px-4 py-2 bg-secondary hover:bg-secondary/80 disabled:opacity-50 text-foreground font-medium rounded-lg transition-colors border border-border"
          >
            {reloaded ? (
              <>
                <Check className="w-4 h-4 text-primary" />
                Reloaded
              </>
            ) : (
              <>
                <RefreshCw className={`w-4 h-4 ${reloading ? 'animate-spin' : ''}`} />
                Reload Config
              </>
            )}
          </button>
          <button
            onClick={handleSave}
            className="flex items-center gap-2 px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground font-medium rounded-lg transition-colors shadow-sm"
          >
            {saved ? (
              <>
                <Check className="w-4 h-4" />
                Saved
              </>
            ) : (
              <>
                <Save className="w-4 h-4" />
                Save Changes
              </>
            )}
          </button>
        </div>
      </div>

      {error && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">
          {error}
        </div>
      )}

      {/* Demo Mode Toggle */}
      <div className="bg-card border border-border shadow-sm rounded-xl p-6">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-3">
            <div className="p-2 bg-amber-500/10 rounded-lg">
              <MonitorPlay className="w-5 h-5 text-amber-600 dark:text-amber-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Demo Mode</h3>
              <p className="text-xs font-medium text-muted-foreground">Use mock data for testing UI without a real backend</p>
            </div>
          </div>
          <button
            onClick={() => setDemoMode(!demoMode)}
            className={`relative inline-flex h-6 w-11 items-center rounded-full transition-colors ${
              demoMode ? 'bg-amber-500' : 'bg-muted-foreground/30'
            }`}
          >
            <span
              className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${
                demoMode ? 'translate-x-6' : 'translate-x-1'
              }`}
            />
          </button>
        </div>
      </div>

      {/* Hardware Benchmark - hidden diagnostic page, no Sidebar entry */}
      <div className="bg-card border border-border shadow-sm rounded-xl p-6">
        <div className="flex items-center justify-between gap-4">
          <div className="flex items-center gap-3">
            <div className="p-2 bg-primary/10 rounded-lg">
              <Gauge className="w-5 h-5 text-primary" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Hardware Benchmark</h3>
              <p className="text-xs font-medium text-muted-foreground">Measure real cold-vs-warm TTFT on your own hardware, through this mesh</p>
            </div>
          </div>
          <button
            onClick={() => navigate('/benchmark')}
            className="px-4 py-2 bg-secondary hover:bg-secondary/80 text-foreground text-sm font-medium rounded-lg transition-colors border border-border shrink-0"
          >
            Open
          </button>
        </div>
      </div>

      <div className="space-y-8 pt-2">
        <section>
          <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-3">General</h2>
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6 items-start">
        {/* Proxy Settings */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-primary/10 rounded-lg">
              <Terminal className="w-5 h-5 text-primary" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Proxy Configuration</h3>
              <p className="text-xs font-medium text-muted-foreground">Core proxy server settings</p>
            </div>
          </div>

          <div className="space-y-4">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Proxy Port
              </label>
              <input
                type="number"
                value={settings.proxyPort}
                onChange={(e) => setSettings({ ...settings, proxyPort: parseInt(e.target.value) || settings.proxyPort })}
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
              />
              <p className="text-[10px] text-amber-500/80 mt-1">
                Requires a mesh restart to take effect - saving here only stores the new port.
              </p>
            </div>

            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Timezone
              </label>
              <CustomSelect
                value={settings.timezone}
                onChange={(val) => setSettings({ ...settings, timezone: val })}
                options={timezones.map(tz => ({ value: tz, label: getTimezoneLabel(tz) }))}
              />
              <p className="text-[10px] text-muted-foreground mt-1">
                Scheduler and prediction cycles will evaluate relative to this timezone.
              </p>
            </div>

            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-2">
                Authentication Mode
              </label>
              <div className="space-y-2">
                <label className="flex items-center gap-3 p-3 rounded-lg border border-border bg-secondary/30 cursor-pointer hover:border-primary/40 transition-colors">
                  <input
                    type="radio"
                    name="authMode"
                    checked={settings.authMode === 'api-key'}
                    onChange={() => setSettings({ ...settings, authMode: 'api-key' })}
                    className="accent-primary"
                  />
                  <div>
                    <p className="text-sm font-medium text-foreground">API Key Authentication</p>
                    <p className="text-xs text-muted-foreground">Require valid API key for all requests</p>
                  </div>
                </label>
                <label className="flex items-center gap-3 p-3 rounded-lg border border-border bg-secondary/30 cursor-pointer hover:border-primary/40 transition-colors">
                  <input
                    type="radio"
                    name="authMode"
                    checked={settings.authMode === 'no-auth'}
                    onChange={() => setSettings({ ...settings, authMode: 'no-auth' })}
                    className="accent-primary"
                  />
                  <div>
                    <p className="text-sm font-medium text-foreground">No Authentication</p>
                    <p className="text-xs text-muted-foreground">Allow all requests (development only)</p>
                  </div>
                </label>
              </div>
            </div>
          </div>
        </div>

        {/* Dashboard Preferences */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-indigo-500/10 rounded-lg">
              <Sliders className="w-5 h-5 text-indigo-600 dark:text-indigo-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Dashboard Preferences</h3>
              <p className="text-xs font-medium text-muted-foreground">Customize UI warning banners visibility</p>
            </div>
          </div>

          <div className="space-y-4">
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Hide Demo Banner</p>
                <p className="text-xs text-muted-foreground">Do not show warning banner in demo mode</p>
              </div>
              <button
                onClick={() => setSettings({ ...settings, hideDemoBanner: !settings.hideDemoBanner })}
                className={`relative inline-flex h-6 w-11 items-center rounded-full transition-colors ${
                  settings.hideDemoBanner ? 'bg-primary' : 'bg-muted-foreground/30'
                }`}
              >
                <span
                  className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${
                    settings.hideDemoBanner ? 'translate-x-6' : 'translate-x-1'
                  }`}
                />
              </button>
            </div>

            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Hide Budget Banner</p>
                <p className="text-xs text-muted-foreground">Do not show cloud spend warning banners</p>
              </div>
              <button
                onClick={() => setSettings({ ...settings, hideBudgetBanner: !settings.hideBudgetBanner })}
                className={`relative inline-flex h-6 w-11 items-center rounded-full transition-colors ${
                  settings.hideBudgetBanner ? 'bg-primary' : 'bg-muted-foreground/30'
                }`}
              >
                <span
                  className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${
                    settings.hideBudgetBanner ? 'translate-x-6' : 'translate-x-1'
                  }`}
                />
              </button>
            </div>
          </div>
        </div>

          </div>
        </section>

        <section>
          <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-3">Security & Access</h2>
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6 items-start">
        {/* Admin & Security */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-rose-500/10 rounded-lg">
              <Lock className="w-5 h-5 text-rose-600 dark:text-rose-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Admin & Security</h3>
              <p className="text-xs font-medium text-muted-foreground">Dashboard listen address and CORS - takes effect on next restart</p>
            </div>
          </div>
          <div className="space-y-4">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Admin Bind Address</label>
              <input type="text" value={settings.adminBindAddress} onChange={(e) => setSettings({ ...settings, adminBindAddress: e.target.value })} placeholder=":8080" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
              <p className="text-[10px] text-muted-foreground mt-1">
                Use 127.0.0.1:8080 to restrict the dashboard to localhost.
              </p>
              <p className="text-[10px] text-amber-500/80 mt-1">
                Requires a mesh restart to take effect - changing this can lock you out until you reach it via the new address.
              </p>
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">CORS Origin</label>
              <input type="text" value={settings.adminCorsOrigin} onChange={(e) => setSettings({ ...settings, adminCorsOrigin: e.target.value })} placeholder="https://your-frontend.example.com" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
              <p className="text-[10px] text-muted-foreground mt-1">Leave blank for same-origin only. Must be one concrete origin, not "*".</p>
            </div>
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Proxy Access Log</p>
                <p className="text-xs text-muted-foreground">Structured JSON access-log line per request on stdout</p>
                <p className="text-[10px] text-amber-500/80 mt-1">Requires a mesh restart to take effect.</p>
              </div>
              <Toggle on={settings.proxyAccessLog} onToggle={() => setSettings({ ...settings, proxyAccessLog: !settings.proxyAccessLog })} />
            </div>
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Trust Proxy Headers</p>
                <p className="text-xs text-muted-foreground">Trust X-Forwarded-For/X-Real-IP for the logged client IP. Only enable if the mesh sits behind a trusted reverse proxy - otherwise these headers are forgeable by any direct client.</p>
              </div>
              <Toggle on={settings.proxyTrustProxyHeaders} onToggle={() => setSettings({ ...settings, proxyTrustProxyHeaders: !settings.proxyTrustProxyHeaders })} />
            </div>
          </div>
        </div>

        {/* Developer Integrations & Security */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-rose-500/10 rounded-lg">
              <Lock className="w-5 h-5 text-rose-600 dark:text-rose-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Developer Integrations & Security</h3>
              <p className="text-xs font-medium text-muted-foreground">API tokens and proxy access controls</p>
            </div>
          </div>

          <div className="space-y-4">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Hugging Face API Token
              </label>
              <input
                type="password"
                value={settings.huggingFaceToken || ''}
                onChange={(e) => setSettings({ ...settings, huggingFaceToken: e.target.value })}
                placeholder="hf_..."
                autoComplete="off"
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
              <p className="text-[10px] text-muted-foreground mt-1">
                Used by Model Advisor to query the Hugging Face API for model recommendations. Encrypted at rest; masked here once saved.
              </p>
            </div>

            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Allow Management Endpoints</p>
                <p className="text-xs text-muted-foreground">Let client API keys reach model create/delete/pull APIs, not just inference. Only enable if every client is trusted.</p>
              </div>
              <button
                onClick={() => setSettings({ ...settings, allowManagementEndpoints: !settings.allowManagementEndpoints })}
                className={`relative inline-flex h-6 w-11 items-center rounded-full transition-colors ${
                  settings.allowManagementEndpoints ? 'bg-primary' : 'bg-muted-foreground/30'
                }`}
              >
                <span
                  className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${
                    settings.allowManagementEndpoints ? 'translate-x-6' : 'translate-x-1'
                  }`}
                />
              </button>
            </div>
          </div>
        </div>

        {/* Admin Credentials - hidden in demo mode */}
        {!demoMode && (
          <div className="bg-card border border-border shadow-sm rounded-xl p-6 lg:col-span-2">
            <div className="flex items-center gap-3 mb-5">
              <div className="p-2 bg-rose-500/10 rounded-lg">
                <KeyRound className="w-5 h-5 text-rose-600 dark:text-rose-400" />
              </div>
              <div>
                <h3 className="text-sm font-semibold text-foreground">Admin Credentials</h3>
                <p className="text-xs font-medium text-muted-foreground">Change your dashboard login password</p>
              </div>
            </div>

            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">Current Password</label>
                <input
                  type="password"
                  value={credCurrentPw}
                  onChange={(e) => setCredCurrentPw(e.target.value)}
                  placeholder="Required to make changes"
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
                />
              </div>
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">New Password <span className="text-muted-foreground/60">(optional)</span></label>
                <input
                  type="password"
                  value={credNewPw}
                  onChange={(e) => setCredNewPw(e.target.value)}
                  placeholder="Leave blank to keep current"
                  autoComplete="new-password"
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
                />
              </div>
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">Confirm New Password</label>
                <input
                  type="password"
                  value={credConfirmPw}
                  onChange={(e) => setCredConfirmPw(e.target.value)}
                  placeholder="Repeat new password"
                  autoComplete="new-password"
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
                />
              </div>
            </div>

            {credError && (
              <p className="mt-3 text-sm text-destructive">{credError}</p>
            )}
            {credSaved && (
              <p className="mt-3 text-sm text-green-600 dark:text-green-400">Credentials updated. Re-login required on other sessions.</p>
            )}

            <div className="mt-4 flex justify-end">
              <button
                onClick={handleChangeCredentials}
                disabled={credSaving || !credCurrentPw}
                className="flex items-center gap-2 px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground text-sm font-medium rounded-lg transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
              >
                {credSaving ? (
                  'Saving...'
                ) : credSaved ? (
                  <><Check className="w-4 h-4" /> Saved</>
                ) : (
                  <><Save className="w-4 h-4" /> Update Credentials</>
                )}
              </button>
            </div>
          </div>
        )}
          </div>
        </section>

        <section>
          <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-3">Integrations & Cost</h2>
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6 items-start">
        {/* LiteLLM Integration */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-blue-500/10 rounded-lg">
              <Shield className="w-5 h-5 text-blue-600 dark:text-blue-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">LiteLLM Integration</h3>
              <p className="text-xs font-medium text-muted-foreground">Middleware layer configuration</p>
            </div>
          </div>

          <div className="space-y-4">
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Enable LiteLLM</p>
                <p className="text-xs text-muted-foreground">Route requests through LiteLLM proxy</p>
              </div>
              <button
                onClick={() => setSettings({ ...settings, liteLLMEnabled: !settings.liteLLMEnabled })}
                className={`relative inline-flex h-6 w-11 items-center rounded-full transition-colors ${
                  settings.liteLLMEnabled ? 'bg-primary' : 'bg-muted-foreground/30'
                }`}
              >
                <span
                  className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${
                    settings.liteLLMEnabled ? 'translate-x-6' : 'translate-x-1'
                  }`}
                />
              </button>
            </div>

            {settings.liteLLMEnabled && (
              <div className="animate-fade-in">
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                  LiteLLM Endpoint URL
                </label>
                <input
                  type="text"
                  value={settings.liteLLMEndpoint}
                  onChange={(e) => setSettings({ ...settings, liteLLMEndpoint: e.target.value })}
                  placeholder="http://localhost:4000"
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
                />
                <label className="block text-sm font-medium text-muted-foreground mb-1.5 mt-3">
                  API Key
                </label>
                <input
                  type="password"
                  value={settings.liteLLMApiKey}
                  onChange={(e) => setSettings({ ...settings, liteLLMApiKey: e.target.value })}
                  autoComplete="off"
                  placeholder="sk-litellm-..."
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
                />
                <p className="text-[10px] text-muted-foreground mt-1">
                  Sent as Authorization: Bearer &lt;key&gt; to your LiteLLM proxy.
                </p>
              </div>
            )}

            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                /api/ps Polling Interval
              </label>
              <div className="flex items-center gap-3">
                <input
                  type="range"
                  min="1000"
                  max="10000"
                  step="500"
                  value={settings.pollingInterval}
                  onChange={(e) => setSettings({ ...settings, pollingInterval: parseInt(e.target.value) || 1000 })}
                  className="flex-1 accent-primary"
                />
                <code className="font-mono text-sm font-medium text-primary min-w-[80px]">
                  {settings.pollingInterval}ms
                </code>
              </div>
            </div>
          </div>
        </div>

        {/* Cloud Spend Cap */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-emerald-500/10 rounded-lg">
              <DollarSign className="w-5 h-5 text-emerald-600 dark:text-emerald-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Cloud Spend Cap</h3>
              <p className="text-xs font-medium text-muted-foreground">Block cloud fallback once spend hits these limits</p>
            </div>
          </div>

          <div className="space-y-4">
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                  Display Currency
                </label>
                <CustomSelect
                  value={currency.code}
                  onChange={(val) => {
                    const preset = CURRENCY_PRESETS.find(c => c.code === val);
                    setCurrency({ ...currency, code: val, symbol: preset?.symbol || currency.symbol });
                  }}
                  options={CURRENCY_PRESETS.map(c => ({ value: c.code, label: c.code }))}
                />
              </div>
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                  FX Rate (1 USD =)
                </label>
                <input
                  type="number"
                  min={0.0001}
                  step="0.0001"
                  value={currency.fxRate}
                  onChange={(e) => setCurrency({ ...currency, fxRate: parseFloat(e.target.value) || 1 })}
                  disabled={currency.code === 'USD'}
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50 disabled:opacity-50"
                />
              </div>
            </div>

            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Daily Cap ({currency.code})
              </label>
              <input
                type="number"
                min={0}
                step="0.01"
                value={roundDisplay(toDisplay(settings.cloudDailyUsdCap))}
                onChange={(e) => setSettings({ ...settings, cloudDailyUsdCap: toUSD(parseFloat(e.target.value) || 0) })}
                placeholder="0 = disabled"
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Monthly Cap ({currency.code})
              </label>
              <input
                type="number"
                min={0}
                step="0.01"
                value={roundDisplay(toDisplay(settings.cloudMonthlyUsdCap))}
                onChange={(e) => setSettings({ ...settings, cloudMonthlyUsdCap: toUSD(parseFloat(e.target.value) || 0) })}
                placeholder="0 = disabled"
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Warn at (% of cap)
              </label>
              <input
                type="number"
                min={0}
                max={100}
                step="1"
                value={Math.round(settings.cloudSoftBudgetPct * 100)}
                onChange={(e) => setSettings({ ...settings, cloudSoftBudgetPct: (parseFloat(e.target.value) || 0) / 100 })}
                placeholder="0 = disabled"
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>

            <p className="text-[10px] text-muted-foreground">
              Checked against real cumulative cloud spend (UTC day/month). 0 disables the check.
              Amounts stored and enforced in USD - currency above is display-only, converted at the manual FX rate you set.
            </p>
          </div>
        </div>

        {/* Cloud Providers */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6 lg:col-span-2">
          <div className="flex items-center justify-between mb-5">
            <div className="flex items-center gap-3">
              <div className="p-2 bg-sky-500/10 rounded-lg">
                <Cloud className="w-5 h-5 text-sky-600 dark:text-sky-400" />
              </div>
              <div>
                <h3 className="text-sm font-semibold text-foreground">Cloud Providers</h3>
                <p className="text-xs font-medium text-muted-foreground">
                  {settings.liteLLMEnabled
                    ? 'Managed by LiteLLM while enabled - this list is inactive'
                    : 'Fallback cloud endpoints for overflow traffic, tried highest priority first'}
                </p>
              </div>
            </div>
            {!demoMode && !settings.liteLLMEnabled && (
              <button
                onClick={openAddCloudProvider}
                className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium bg-primary hover:bg-primary/90 text-primary-foreground rounded-lg transition-colors"
              >
                <Plus className="w-3.5 h-3.5" />
                Add Provider
              </button>
            )}
          </div>

          <div className={settings.liteLLMEnabled ? 'opacity-40 pointer-events-none' : ''}>
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
                  <div key={provider.name} className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
                    <div className="flex items-center gap-3">
                      <div className="flex flex-col items-center gap-0.5 w-5 text-[10px] font-medium text-muted-foreground">
                        <span>{index + 1}</span>
                      </div>
                      <StatusDot status={provider.enabled ? 'online' : 'offline'} size="sm" />
                      <div>
                        <p className="text-sm font-medium text-foreground">{provider.name}</p>
                        <p className="text-xs font-medium text-muted-foreground">{provider.default_model} - ${provider.cost_per_1k_tokens.toFixed(4)}/1k tokens</p>
                      </div>
                    </div>
                    <div className="flex items-center gap-2">
                      <Badge variant={provider.enabled ? 'success' : 'muted'} size="sm">
                        {provider.provider}
                      </Badge>
                      {!demoMode && (
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
                      )}
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>
        </div>

          </div>
        </section>

        <section>
          <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-3">Routing & Reliability</h2>
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6 items-start">
        {/* Advanced Routing */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6 lg:col-span-2">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-cyan-500/10 rounded-lg">
              <Network className="w-5 h-5 text-cyan-600 dark:text-cyan-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Advanced Routing</h3>
              <p className="text-xs font-medium text-muted-foreground">Timeouts, retries, session affinity, and queueing - takes effect on next restart</p>
            </div>
          </div>

          <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Fallback Strategy</label>
              <CustomSelect
                value={settings.routingFallback}
                onChange={(val) => setSettings({ ...settings, routingFallback: val })}
                options={[
                  { value: 'least-connections', label: 'Least Connections' },
                  { value: 'round-robin', label: 'Round Robin' },
                ]}
              />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Upstream Timeout (ms)</label>
              <input type="number" value={settings.routingUpstreamTimeoutMs} onChange={(e) => setSettings({ ...settings, routingUpstreamTimeoutMs: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Max Retries</label>
              <input type="number" value={settings.routingMaxRetries} onChange={(e) => setSettings({ ...settings, routingMaxRetries: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">nvidia-smi Poll Interval (ms)</label>
              <input type="number" value={settings.routingNvidiaPollIntervalMs} onChange={(e) => setSettings({ ...settings, routingNvidiaPollIntervalMs: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Queue Max Depth</label>
              <input type="number" value={settings.routingQueueMaxDepth} onChange={(e) => setSettings({ ...settings, routingQueueMaxDepth: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Queue Timeout (ms)</label>
              <input type="number" value={settings.routingQueueTimeoutMs} onChange={(e) => setSettings({ ...settings, routingQueueTimeoutMs: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Health Failure Threshold</label>
              <input type="number" value={settings.routingHealthFailureThreshold} onChange={(e) => setSettings({ ...settings, routingHealthFailureThreshold: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Health Success Threshold</label>
              <input type="number" value={settings.routingHealthSuccessThreshold} onChange={(e) => setSettings({ ...settings, routingHealthSuccessThreshold: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Cloud Overflow SLA (ms, 0 = disabled)</label>
              <input type="number" value={settings.routingOverflowSlaMs} onChange={(e) => setSettings({ ...settings, routingOverflowSlaMs: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
          </div>

          <div className="mt-4 flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
            <div>
              <p className="text-sm font-medium text-foreground">Session Affinity</p>
              <p className="text-xs text-muted-foreground">Route requests sharing an X-Session-ID to the same node (KV-cache reuse)</p>
            </div>
            <Toggle on={settings.routingSessionAffinity} onToggle={() => setSettings({ ...settings, routingSessionAffinity: !settings.routingSessionAffinity })} />
          </div>
          {settings.routingSessionAffinity && (
            <div className="mt-3">
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Session Affinity TTL</label>
              <input type="text" value={settings.routingSessionAffinityTtl} onChange={(e) => setSettings({ ...settings, routingSessionAffinityTtl: e.target.value })} placeholder="10m" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
          )}

          <div className="mt-4 pt-4 border-t border-border">
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div className="flex items-center gap-2">
                <Flame className="w-4 h-4 text-orange-500" />
                <div>
                  <p className="text-sm font-medium text-foreground">Thermal Watchdog</p>
                  <p className="text-xs text-muted-foreground">Auto-drain a node after sustained overheat (recovery requires manual undrain)</p>
                </div>
              </div>
              <Toggle on={settings.thermalWatchdogEnabled} onToggle={() => setSettings({ ...settings, thermalWatchdogEnabled: !settings.thermalWatchdogEnabled })} />
            </div>
            {settings.thermalWatchdogEnabled && (
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 mt-3">
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">Max Temp (°C)</label>
                  <input type="number" value={settings.thermalWatchdogMaxTempCelsius} onChange={(e) => setSettings({ ...settings, thermalWatchdogMaxTempCelsius: parseFloat(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
                </div>
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">Consecutive Breaches</label>
                  <input type="number" value={settings.thermalWatchdogConsecutiveBreaches} onChange={(e) => setSettings({ ...settings, thermalWatchdogConsecutiveBreaches: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
                </div>
              </div>
            )}
          </div>
        </div>

        {/* Docker Auto-Discovery */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-blue-500/10 rounded-lg">
              <Container className="w-5 h-5 text-blue-600 dark:text-blue-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Docker Auto-Discovery</h3>
              <p className="text-xs font-medium text-muted-foreground">Auto-register Ollama containers - takes effect on next restart</p>
            </div>
          </div>
          <div className="space-y-4">
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Enable Docker Discovery</p>
                <p className="text-xs text-muted-foreground">Poll the Docker socket for Ollama containers</p>
              </div>
              <Toggle on={settings.dockerEnabled} onToggle={() => setSettings({ ...settings, dockerEnabled: !settings.dockerEnabled })} />
            </div>
            {settings.dockerEnabled && (
              <>
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">Docker Socket</label>
                  <input type="text" value={settings.dockerSocket} onChange={(e) => setSettings({ ...settings, dockerSocket: e.target.value })} placeholder="/var/run/docker.sock" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
                </div>
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">Poll Interval (ms)</label>
                  <input type="number" value={settings.dockerPollIntervalMs} onChange={(e) => setSettings({ ...settings, dockerPollIntervalMs: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
                </div>
              </>
            )}
          </div>
        </div>

        {/* Webhooks */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-violet-500/10 rounded-lg">
              <Webhook className="w-5 h-5 text-violet-600 dark:text-violet-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Webhooks</h3>
              <p className="text-xs font-medium text-muted-foreground">HMAC-signed node health-transition notifications</p>
            </div>
          </div>
          <div className="space-y-4">
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Enable Webhooks</p>
              </div>
              <Toggle on={settings.webhookEnabled} onToggle={() => setSettings({ ...settings, webhookEnabled: !settings.webhookEnabled })} />
            </div>
            {settings.webhookEnabled && (
              <>
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">Webhook URL</label>
                  <input type="text" value={settings.webhookUrl} onChange={(e) => setSettings({ ...settings, webhookUrl: e.target.value })} placeholder="https://hooks.example.com/ollama-mesh" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
                </div>
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">Signing Secret</label>
                  <input type="password" value={settings.webhookSecret} onChange={(e) => setSettings({ ...settings, webhookSecret: e.target.value })} autoComplete="off" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
                  <p className="text-[10px] text-muted-foreground mt-1">Stored server-side; masked here once saved.</p>
                </div>
              </>
            )}
          </div>
        </div>

          </div>
        </section>

        <section>
          <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-3">Observability & Limits</h2>
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6 items-start">
        {/* Observability */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-purple-500/10 rounded-lg">
              <Activity className="w-5 h-5 text-purple-600 dark:text-purple-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Observability</h3>
              <p className="text-xs font-medium text-muted-foreground">Metrics and logging configuration</p>
            </div>
          </div>

          <div className="space-y-4">
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Prometheus Metrics</p>
                <p className="text-xs text-muted-foreground">Export metrics in Prometheus format</p>
              </div>
              <button
                onClick={() => setSettings({ ...settings, prometheusEnabled: !settings.prometheusEnabled })}
                className={`relative inline-flex h-6 w-11 items-center rounded-full transition-colors ${
                  settings.prometheusEnabled ? 'bg-primary' : 'bg-muted-foreground/30'
                }`}
              >
                <span
                  className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${
                    settings.prometheusEnabled ? 'translate-x-6' : 'translate-x-1'
                  }`}
                />
              </button>
            </div>

            {settings.prometheusEnabled && (
              <div className="animate-fade-in">
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                  Prometheus Port
                </label>
                <input
                  type="number"
                  value={settings.prometheusPort}
                  onChange={(e) => setSettings({ ...settings, prometheusPort: parseInt(e.target.value) || settings.prometheusPort })}
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
                />
              </div>
            )}

            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Log Level
              </label>
              <CustomSelect
                value={settings.logLevel}
                onChange={(val) => setSettings({ ...settings, logLevel: val as any })}
                options={[
                  { value: 'debug', label: 'Debug' },
                  { value: 'info', label: 'Info' },
                  { value: 'warn', label: 'Warning' },
                  { value: 'error', label: 'Error' },
                ]}
              />
            </div>
          </div>
        </div>

        {/* Model Context Windows */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-lime-500/10 rounded-lg">
              <Ruler className="w-5 h-5 text-lime-600 dark:text-lime-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Model Context Windows</h3>
              <p className="text-xs font-medium text-muted-foreground">Operator-declared max tokens per model, for admission-time checks</p>
            </div>
          </div>

          <div className="space-y-2 mb-4">
            {Object.entries(settings.contextWindows).length === 0 ? (
              <p className="text-sm text-muted-foreground py-2">No context windows declared</p>
            ) : (
              Object.entries(settings.contextWindows).map(([model, tokens]) => (
                <div key={model} className="flex items-center justify-between p-2.5 rounded-lg border border-border bg-secondary/30">
                  <div>
                    <p className="text-sm font-medium text-foreground">{model}</p>
                    <p className="text-xs text-muted-foreground">{tokens.toLocaleString()} tokens</p>
                  </div>
                  <button
                    onClick={() => {
                      const next = { ...settings.contextWindows };
                      delete next[model];
                      setSettings({ ...settings, contextWindows: next });
                    }}
                    className="p-1.5 text-muted-foreground hover:text-destructive rounded-md hover:bg-secondary transition-colors"
                  >
                    <Trash2 className="w-3.5 h-3.5" />
                  </button>
                </div>
              ))
            )}
          </div>

          <div className="flex gap-2">
            <input type="text" value={newCtxModel} onChange={(e) => setNewCtxModel(e.target.value)} placeholder="llama3.2:8b" className="flex-1 px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
            <input type="number" value={newCtxTokens} onChange={(e) => setNewCtxTokens(e.target.value)} placeholder="8192" className="w-28 px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
            <button
              onClick={() => {
                const tokens = parseInt(newCtxTokens, 10);
                if (!newCtxModel.trim() || !tokens) return;
                setSettings({ ...settings, contextWindows: { ...settings.contextWindows, [newCtxModel.trim()]: tokens } });
                setNewCtxModel('');
                setNewCtxTokens('');
              }}
              className="flex items-center gap-1.5 px-3 py-2 bg-primary hover:bg-primary/90 text-primary-foreground text-sm font-medium rounded-lg transition-colors"
            >
              <Plus className="w-4 h-4" />
            </button>
          </div>
        </div>

        {/* Global Warmup & Audit */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6 lg:col-span-2">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-amber-500/10 rounded-lg">
              <Ruler className="w-5 h-5 text-amber-600 dark:text-amber-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Global Warmup & Audit</h3>
              <p className="text-xs font-medium text-muted-foreground">Distinct from per-node warmup toggles on the Warmup page</p>
            </div>
          </div>
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
            <div className="space-y-4">
              <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
                <div>
                  <p className="text-sm font-medium text-foreground">Enable Global Warmup</p>
                </div>
                <Toggle on={settings.warmupEnabled} onToggle={() => setSettings({ ...settings, warmupEnabled: !settings.warmupEnabled })} />
              </div>
              {settings.warmupEnabled && (
                <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                  <div>
                    <label className="block text-sm font-medium text-muted-foreground mb-1.5">Interval (ms)</label>
                    <input type="number" value={settings.warmupIntervalMs} onChange={(e) => setSettings({ ...settings, warmupIntervalMs: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
                  </div>
                  <div>
                    <label className="block text-sm font-medium text-muted-foreground mb-1.5">Keep Alive</label>
                    <input type="text" value={settings.warmupKeepAlive} onChange={(e) => setSettings({ ...settings, warmupKeepAlive: e.target.value })} placeholder="10m" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
                  </div>
                </div>
              )}
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">Local Token Value ($/1k tokens)</label>
                <input type="number" step="0.001" value={settings.savingsReferenceCostPer1k} onChange={(e) => setSettings({ ...settings, savingsReferenceCostPer1k: parseFloat(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
                <p className="text-[10px] text-muted-foreground mt-1">Cloud rate used to value locally-served tokens in the dashboard's savings calculation.</p>
              </div>
            </div>
            <div className="space-y-4">
              <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
                <div>
                  <p className="text-sm font-medium text-foreground">Audit Log</p>
                  <p className="text-xs text-muted-foreground">Append-only request audit trail</p>
                </div>
                <Toggle on={settings.auditEnabled} onToggle={() => setSettings({ ...settings, auditEnabled: !settings.auditEnabled })} />
              </div>
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">Audit Log Retention (days)</label>
                <input
                  type="number"
                  min={0}
                  step={1}
                  value={settings.auditRetentionDays}
                  onChange={(e) => setSettings({ ...settings, auditRetentionDays: Math.max(0, parseInt(e.target.value, 10) || 0) })}
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
                />
                <p className="text-[10px] text-muted-foreground mt-1">
                  Requests older than this are pruned automatically (checked every 12h). Set to 0 to keep audit log entries forever.
                </p>
              </div>
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">System Audit Retention (days)</label>
                <input
                  type="number"
                  min={0}
                  step={1}
                  value={settings.systemAuditRetentionDays}
                  onChange={(e) => setSettings({ ...settings, systemAuditRetentionDays: Math.max(0, parseInt(e.target.value, 10) || 0) })}
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
                />
                <p className="text-[10px] text-muted-foreground mt-1">
                  Applies to the admin action trail (System Audit page), separate from request logs above. Defaults to 0 - keep forever - since this log is low-volume and security-sensitive.
                </p>
              </div>
            </div>
          </div>
        </div>

        {/* Backup & Restore */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6 lg:col-span-2">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-emerald-500/10 rounded-lg">
              <HardDrive className="w-5 h-5 text-emerald-600 dark:text-emerald-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Backup & Restore</h3>
              <p className="text-xs font-medium text-muted-foreground">
                Full docs:{' '}
                <a
                  href="https://anirudh.social/ollama-mesh/docs/backup.html"
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-primary hover:underline"
                >
                  docs/backup.md
                </a>
              </p>
            </div>
          </div>
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
            <div className="space-y-4">
              <div>
                <button
                  onClick={handleBackupNow}
                  disabled={backupDownloading}
                  className="flex items-center gap-1.5 px-3 py-2 bg-primary hover:bg-primary/90 disabled:opacity-50 text-primary-foreground text-sm font-medium rounded-lg transition-colors"
                >
                  <Download className="w-4 h-4" />
                  {backupDownloading ? 'Preparing download...' : 'Download Backup Now'}
                </button>
                {backupDownloadError && (
                  <p className="text-[10px] text-red-500 mt-1.5">{backupDownloadError}</p>
                )}
                <p className="text-[10px] text-muted-foreground mt-1.5">
                  Downloads a consistent point-in-time copy of mesh.db to your browser, taken while the mesh keeps running.
                </p>
              </div>
              {settings.backupLastAt && (
                <p className="text-xs text-muted-foreground">
                  Last scheduled backup: {new Date(settings.backupLastAt).toLocaleString()}
                </p>
              )}
              {settings.backupLastError && (
                <p className="text-xs text-red-500">Last scheduled backup failed: {settings.backupLastError}</p>
              )}
            </div>
            <div className="space-y-4">
              <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
                <div>
                  <p className="text-sm font-medium text-foreground">Scheduled Backup</p>
                  <p className="text-xs text-muted-foreground">Automatic VACUUM INTO backups on a recurring interval</p>
                </div>
                <Toggle on={settings.backupEnabled} onToggle={() => setSettings({ ...settings, backupEnabled: !settings.backupEnabled })} />
              </div>
              {settings.backupEnabled && (
                <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                  <div>
                    <label className="block text-sm font-medium text-muted-foreground mb-1.5">Interval (hours)</label>
                    <input
                      type="number"
                      min={1}
                      step={1}
                      value={settings.backupIntervalHours}
                      onChange={(e) => setSettings({ ...settings, backupIntervalHours: Math.max(1, parseInt(e.target.value, 10) || 24) })}
                      className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
                    />
                  </div>
                  <div>
                    <label className="block text-sm font-medium text-muted-foreground mb-1.5">Retention (backups kept)</label>
                    <input
                      type="number"
                      min={1}
                      step={1}
                      value={settings.backupRetentionCount}
                      onChange={(e) => setSettings({ ...settings, backupRetentionCount: Math.max(1, parseInt(e.target.value, 10) || 7) })}
                      className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
                    />
                  </div>
                </div>
              )}
              {settings.backupEnabled && (
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">Target Directory</label>
                  <input
                    type="text"
                    value={settings.backupTargetDir}
                    onChange={(e) => setSettings({ ...settings, backupTargetDir: e.target.value })}
                    placeholder="/backups"
                    className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
                  />
                  <p className="text-[10px] text-muted-foreground mt-1">
                    In Docker, defaults to a separate volume from mesh.db's own data volume, so deleting one doesn't take out the other.
                  </p>
                </div>
              )}
            </div>
          </div>

          <div className="mt-5 pt-5 border-t border-border">
            <p className="text-sm font-medium text-foreground mb-1">Restore from a backup</p>
            <p className="text-[10px] text-muted-foreground mb-3">
              Swaps mesh.db for the selected file and restarts the mesh. Requires the deployment to auto-restart
              on exit (systemd, Docker's <code>restart</code> policy, or Kubernetes) - otherwise start it manually
              afterward. This cannot be undone.
            </p>
            {restoreInitiated && (
              <p className="text-xs text-emerald-600 dark:text-emerald-400 mb-3">
                Restore initiated - the mesh is restarting now. This page will reconnect once it's back up.
              </p>
            )}
            {restoreError && (
              <p className="text-xs text-red-500 mb-3">{restoreError}</p>
            )}
            {uploadError && (
              <p className="text-xs text-red-500 mb-3">{uploadError}</p>
            )}
            {backupListLoading ? (
              <p className="text-xs text-muted-foreground">Loading backups...</p>
            ) : (
              <div>
                <div className="flex items-center gap-2">
                  {backupList.length === 0 ? (
                    <p className="flex-1 min-w-0 text-xs text-muted-foreground">
                      No backups found yet in the target directory.
                    </p>
                  ) : (
                    <select
                      value={selectedBackupName}
                      onChange={(e) => setSelectedBackupName(e.target.value)}
                      className="flex-1 min-w-0 px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
                    >
                      {backupList.map((b) => (
                        <option key={b.name} value={b.name}>{b.name}</option>
                      ))}
                    </select>
                  )}
                  <label
                    title="Attach a .db file from your computer as a backup"
                    className={`shrink-0 w-9 h-9 flex items-center justify-center bg-secondary/50 border border-border rounded-lg text-foreground transition-colors ${uploadingBackup ? 'opacity-50 cursor-not-allowed' : 'hover:bg-secondary cursor-pointer'}`}
                  >
                    {uploadingBackup ? (
                      <RefreshCw className="w-4 h-4 animate-spin" />
                    ) : (
                      <Plus className="w-4 h-4" />
                    )}
                    <input
                      type="file"
                      accept=".db"
                      aria-label="Attach a local .db file as a backup"
                      className="hidden"
                      disabled={uploadingBackup}
                      onChange={handleBackupFileChosen}
                    />
                  </label>
                  <button
                    onClick={() => selectedBackup && setRestoreTarget(selectedBackup)}
                    disabled={!selectedBackup}
                    className="shrink-0 px-3 py-2 bg-red-600 hover:bg-red-600/90 text-white text-xs font-medium rounded-lg transition-colors disabled:opacity-50"
                  >
                    Restore
                  </button>
                </div>
                {selectedBackup && (
                  <p className="text-[10px] text-muted-foreground mt-1.5">
                    {new Date(selectedBackup.modified_at).toLocaleString()} - {formatBackupSize(selectedBackup.size_bytes)}
                  </p>
                )}
              </div>
            )}
          </div>
        </div>
          </div>
        </section>
      </div>
    </div>

    <Modal
      isOpen={restoreTarget !== null}
      onClose={() => { if (!restoring) setRestoreTarget(null); }}
      title="Restore from Backup"
      maxWidth="sm"
    >
      <div className="space-y-4">
        <p className="text-sm text-muted-foreground">
          This will stop the mesh, replace the live database with <span className="font-medium text-foreground">{restoreTarget?.name}</span>,
          and restart. Everything written since that backup was taken - new nodes, keys, settings, routing history - will be lost.
        </p>
        <p className="text-sm text-destructive font-medium">This cannot be undone.</p>
        {restoreError && (
          <p className="text-sm text-destructive">{restoreError}</p>
        )}
        <div className="flex justify-end gap-3 pt-4 border-t border-border">
          <button
            onClick={() => setRestoreTarget(null)}
            disabled={restoring}
            className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors disabled:opacity-50"
          >
            Cancel
          </button>
          <button
            onClick={handleRestoreConfirm}
            disabled={restoring}
            className="px-4 py-2 bg-red-600 hover:bg-red-600/90 text-white font-medium rounded-lg text-sm transition-colors shadow-sm disabled:opacity-50"
          >
            {restoring ? 'Restoring...' : 'Restore & Restart'}
          </button>
        </div>
      </div>
    </Modal>

    <Modal
      isOpen={reloadConfirmOpen}
      onClose={() => setReloadConfirmOpen(false)}
      title="Reload From Database"
      maxWidth="sm"
    >
      <div className="space-y-4">
        <p className="text-sm text-muted-foreground">
          Re-sync nodes, API keys, and cloud providers from the database into the running process?
        </p>
        <p className="text-xs text-muted-foreground">
          Applies immediately without a restart. Other settings (listen ports/addresses, Docker/Webhook wiring)
          require a restart to take effect.
        </p>
        {error && (
          <p className="text-sm text-destructive">{error}</p>
        )}
        <div className="flex justify-end gap-3 pt-4 border-t border-border">
          <button
            onClick={() => setReloadConfirmOpen(false)}
            disabled={reloading}
            className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors disabled:opacity-50"
          >
            Cancel
          </button>
          <button
            onClick={handleReload}
            disabled={reloading}
            className="px-4 py-2 bg-amber-600 hover:bg-amber-600/90 text-white font-medium rounded-lg text-sm transition-colors shadow-sm disabled:opacity-50"
          >
            Reload
          </button>
        </div>
      </div>
    </Modal>

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
    </>
  );
}

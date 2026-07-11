import { useState, useEffect } from 'react';
import { Save, Copy, Check, Terminal, Shield, Activity, FileText, MonitorPlay, Cloud, RefreshCw, KeyRound, DollarSign, Sliders } from 'lucide-react';
import { Badge } from '../components/Badge';
import { StatusDot } from '../components/StatusDot';
import { defaultSettings, configFileYAML, mockCloudProviders } from '../lib/mockData';
import { fetchSettings, updateSettings, fetchCloudProviders, reloadConfig, changePassword } from '../lib/api';
import type { Settings, CloudProvider } from '../types';
import { useDemoMode } from '../hooks/useDemoMode';
import { useCurrency, CURRENCY_PRESETS } from '../hooks/useCurrency';

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
  const { currency, setCurrency, toDisplay, toUSD } = useCurrency();
  const roundDisplay = (n: number) => Math.round(n * 100) / 100;
  const [settings, setSettings] = useState<Settings>(defaultSettings);
  const [cloudProviders, setCloudProviders] = useState<CloudProvider[]>(demoMode ? mockCloudProviders : []);
  const [cloudLoading, setCloudLoading] = useState(!demoMode);
  const [saved, setSaved] = useState(false);
  const [reloaded, setReloaded] = useState(false);
  const [reloading, setReloading] = useState(false);
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Admin credentials change
  const [credCurrentPw, setCredCurrentPw] = useState('');
  const [credNewPw, setCredNewPw] = useState('');
  const [credConfirmPw, setCredConfirmPw] = useState('');
  const [credSaving, setCredSaving] = useState(false);
  const [credError, setCredError] = useState<string | null>(null);
  const [credSaved, setCredSaved] = useState(false);

  useEffect(() => {
    setCloudLoading(true);
    Promise.all([fetchSettings(), fetchCloudProviders().catch(() => mockCloudProviders)])
      .then(([settingsData, providersData]) => {
        setSettings({
          proxyPort: settingsData.proxy?.port || 11434,
          authMode: settingsData.auth?.enabled ? 'api-key' : 'no-auth',
          liteLLMEnabled: settingsData.litellm?.enabled || false,
          liteLLMEndpoint: settingsData.litellm?.url || '',
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
        });
        setCloudProviders(demoMode ? mockCloudProviders : (providersData || []));
        setError(null);
      })
      .catch(err => {
        setError(err.message || 'Failed to load settings');
        setCloudProviders([]);
      })
      .finally(() => setCloudLoading(false));
  }, [demoMode]);

  const handleSave = async () => {
    try {
      // Map UI settings to backend config format (also used in demo mode → localStorage)
      const payload = {
        timezone: settings.timezone,
        proxy: { port: settings.proxyPort, log_level: settings.logLevel },
        auth: { enabled: settings.authMode === 'api-key' },
        routing: { poll_interval_ms: settings.pollingInterval },
        metrics: { enabled: settings.prometheusEnabled, port: settings.prometheusPort },
        litellm: { enabled: settings.liteLLMEnabled, url: settings.liteLLMEndpoint },
        cloud_budget: { daily_usd_cap: settings.cloudDailyUsdCap, monthly_usd_cap: settings.cloudMonthlyUsdCap, soft_budget_pct: settings.cloudSoftBudgetPct },
        hide_demo_banner: settings.hideDemoBanner || false,
        hide_budget_banner: settings.hideBudgetBanner || false,
      };

      await updateSettings(payload);
      window.dispatchEvent(new Event('ollama-mesh-settings-change'));
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
      setError(null);
    } catch (err: any) {
      setError(err.message || 'Failed to save settings');
    }
  };

  const handleReload = async () => {
    if (demoMode) {
      setReloaded(true);
      setTimeout(() => setReloaded(false), 2000);
      return;
    }
    setReloading(true);
    try {
      await reloadConfig();
      setReloaded(true);
      setError(null);
      setTimeout(() => setReloaded(false), 2000);
    } catch (err: any) {
      setError(err.message || 'Config reload failed');
    } finally {
      setReloading(false);
    }
  };

  const buildYAML = (): string => {
    if (demoMode) return configFileYAML;
    return [
      `proxy:`,
      `  port: ${settings.proxyPort}`,
      `  log_level: ${settings.logLevel}`,
      ``,
      `auth:`,
      `  enabled: ${settings.authMode === 'api-key'}`,
      ``,
      `routing:`,
      `  poll_interval_ms: ${settings.pollingInterval}`,
      ``,
      `metrics:`,
      `  enabled: ${settings.prometheusEnabled}`,
      `  port: ${settings.prometheusPort}`,
      ``,
      `litellm:`,
      `  enabled: ${settings.liteLLMEnabled}`,
      settings.liteLLMEnabled ? `  url: ${settings.liteLLMEndpoint}` : null,
      ``,
      `cloud_budget:`,
      `  daily_usd_cap: ${settings.cloudDailyUsdCap}`,
      `  monthly_usd_cap: ${settings.cloudMonthlyUsdCap}`,
    ].filter(line => line !== null).join('\n');
  };

  const copyConfig = () => {
    const yaml = buildYAML();
    const legacyCopy = (text: string) => {
      const el = document.createElement('textarea');
      el.value = text;
      el.setAttribute('readonly', '');
      el.style.cssText = 'position:absolute;left:-9999px;top:auto;width:1px;height:1px';
      document.body.appendChild(el);
      el.focus();
      el.select();
      el.setSelectionRange(0, text.length);
      try { document.execCommand('copy'); } catch (_) {}
      document.body.removeChild(el);
    };
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(yaml).catch(() => legacyCopy(yaml));
    } else {
      legacyCopy(yaml);
    }
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
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

  return (
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
            onClick={handleReload}
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

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6 pt-2">
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
            </div>
            
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Timezone
              </label>
              <select
                value={settings.timezone}
                onChange={(e) => setSettings({ ...settings, timezone: e.target.value })}
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
              >
                {timezones.map(tz => (
                  <option key={tz} value={tz}>
                    {getTimezoneLabel(tz)}
                  </option>
                ))}
              </select>
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
              <select
                value={settings.logLevel}
                onChange={(e) => setSettings({ ...settings, logLevel: e.target.value as any })}
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
              >
                <option value="debug">Debug</option>
                <option value="info">Info</option>
                <option value="warn">Warning</option>
                <option value="error">Error</option>
              </select>
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
            <div className="grid grid-cols-2 gap-3">
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                  Display Currency
                </label>
                <select
                  value={currency.code}
                  onChange={(e) => {
                    const preset = CURRENCY_PRESETS.find(c => c.code === e.target.value);
                    setCurrency({ ...currency, code: e.target.value, symbol: preset?.symbol || currency.symbol });
                  }}
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
                >
                  {CURRENCY_PRESETS.map(c => (
                    <option key={c.code} value={c.code}>{c.code}</option>
                  ))}
                </select>
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

        {/* Cloud Providers */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6 lg:col-span-2">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-sky-500/10 rounded-lg">
              <Cloud className="w-5 h-5 text-sky-600 dark:text-sky-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Cloud Providers</h3>
              <p className="text-xs font-medium text-muted-foreground">Fallback cloud endpoints - configure in config.yaml</p>
            </div>
          </div>

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
              {cloudProviders.map(provider => (
                <div key={provider.name} className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
                  <div className="flex items-center gap-3">
                    <StatusDot status={provider.enabled ? 'online' : 'offline'} size="sm" />
                    <div>
                      <p className="text-sm font-medium text-foreground">{provider.name}</p>
                      <p className="text-xs font-medium text-muted-foreground">{provider.default_model} - ${provider.cost_per_1k_tokens.toFixed(4)}/1k tokens</p>
                    </div>
                  </div>
                  <Badge variant={provider.enabled ? 'success' : 'muted'} size="sm">
                    {provider.provider}
                  </Badge>
                </div>
              ))}
              <p className="text-xs font-medium text-muted-foreground pt-1">
                To add or remove providers, edit config.yaml and restart.
              </p>
            </div>
          )}
        </div>

        {/* Config File Preview */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6 lg:col-span-2">
          <div className="flex items-center justify-between mb-5">
            <div className="flex items-center gap-3">
              <div className="p-2 bg-secondary rounded-lg">
                <FileText className="w-5 h-5 text-muted-foreground" />
              </div>
              <div>
                <h3 className="text-sm font-semibold text-foreground">{demoMode ? 'Config Template' : 'Current Configuration'}</h3>
                <p className="text-xs font-medium text-muted-foreground">
                  {demoMode ? 'Reference configuration template' : 'Based on current settings above'}
                </p>
              </div>
            </div>
            <button
              onClick={copyConfig}
              className="flex items-center gap-2 px-3 py-1.5 text-xs font-medium text-muted-foreground hover:text-foreground bg-secondary hover:bg-secondary/80 rounded-lg transition-colors"
            >
              {copied ? (
                <>
                  <Check className="w-3.5 h-3.5" />
                  Copied
                </>
              ) : (
                <>
                  <Copy className="w-3.5 h-3.5" />
                  Copy
                </>
              )}
            </button>
          </div>

          <div className="relative">
            <pre className="font-mono text-sm bg-secondary/30 border border-border rounded-lg p-4 overflow-x-auto text-foreground/80 leading-relaxed">
              <code>{buildYAML()}</code>
            </pre>
            <Badge variant="muted" size="sm" className="absolute top-3 right-3 shadow-sm bg-background">
              Read-only
            </Badge>
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
    </div>
  );
}

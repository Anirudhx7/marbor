import { useState, useEffect } from 'react';
import { useLocation } from 'react-router-dom';
import { Plus, Copy, Trash2, Key, Pencil } from 'lucide-react';
import { Badge } from '../components/Badge';
import { Modal } from '../components/Modal';
import { SearchInput } from '../components/SearchInput';
import { mockAPIKeys } from '../lib/mockData';
import { fetchKeys, createKey, revokeKey, patchKey, fetchModels } from '../lib/api';
import type { APIKey } from '../types';


function maskKey(key: string): string {
  const parts = key.split('-');
  if (parts.length >= 3) {
    return `${parts[0]}-${parts[1]}-****-****`;
  }
  return key.slice(0, 12) + '****';
}

import { useDemoMode, currentAppPath } from '../hooks/useDemoMode';
import { useCurrency } from '../hooks/useCurrency';
import { CustomDateTimePicker } from '../components/DateTimePicker';

function nowLocalISO(): string {
  const d = new Date();
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function APIKeys() {
  const { demoMode } = useDemoMode();
  const location = useLocation();
  const { currency, toDisplay, toUSD } = useCurrency();
  const roundDisplay = (n: number) => Math.round(n * 100) / 100;
  const [keys, setKeys] = useState<APIKey[]>(demoMode ? mockAPIKeys : []);
  const [isLive, setIsLive] = useState(!demoMode);
  const [searchQuery, setSearchQuery] = useState('');
  const [isCreateModalOpen, setIsCreateModalOpen] = useState(false);
  const [isRevokeModalOpen, setIsRevokeModalOpen] = useState(false);
  const [keyToRevoke, setKeyToRevoke] = useState<APIKey | null>(null);
  const [copiedId, setCopiedId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [revokeError, setRevokeError] = useState<string | null>(null);
  const [newlyCreatedKey, setNewlyCreatedKey] = useState<string | null>(null);
  const [newKeyDismissTimer, setNewKeyDismissTimer] = useState<ReturnType<typeof setTimeout> | null>(null);

  const [editKey, setEditKey] = useState<APIKey | null>(null);
  // dailyUsdCapDisplay/monthlyUsdCapDisplay hold exactly what the admin typed,
  // in the display currency - unlike the other numeric fields, these can't
  // bind directly to the wire USD value, since the field is shown in
  // whatever currency the admin has selected. They must NOT be recomputed
  // from a stored USD value on every render (P106): parseFloat silently
  // drops a trailing decimal point, so a round-trip through currency math on
  // every keystroke turned typing "10.00" into "100+" purely from typing
  // order. Converted to USD only once, at save time.
  const [editForm, setEditForm] = useState<{ rateLimit: string; dailyLimit: string; monthlyLimit: string; dailyUsdCapDisplay: string; monthlyUsdCapDisplay: string; models: string[]; expiresAt: string; localOnly: boolean; allowLocalDegradation: boolean }>({ rateLimit: '', dailyLimit: '', monthlyLimit: '', dailyUsdCapDisplay: '', monthlyUsdCapDisplay: '', models: [], expiresAt: '', localOnly: false, allowLocalDegradation: false });
  const [editSaving, setEditSaving] = useState(false);
  const [editError, setEditError] = useState('');

  const openEditModal = (key: APIKey) => {
    setEditKey(key);
    setEditForm({
      rateLimit: key.rateLimit != null ? String(key.rateLimit) : '',
      dailyLimit: key.dailyLimit != null ? String(key.dailyLimit) : '',
      monthlyLimit: key.monthlyLimit != null ? String(key.monthlyLimit) : '',
      dailyUsdCapDisplay: key.dailyUsdCap != null ? String(roundDisplay(toDisplay(key.dailyUsdCap))) : '',
      monthlyUsdCapDisplay: key.monthlyUsdCap != null ? String(roundDisplay(toDisplay(key.monthlyUsdCap))) : '',
      models: key.allowedModels ?? [],
      expiresAt: key.expiresAt ?? '',
      localOnly: key.localOnly ?? false,
      allowLocalDegradation: key.allowLocalDegradation ?? false,
    });
    setEditError('');
  };

  const handleSaveKeyPatch = async () => {
    if (!editKey) return;
    const patch: { rate_limit?: number; daily_limit?: number; monthly_limit?: number; daily_usd_cap?: number; monthly_usd_cap?: number; models?: string[]; expires_at?: string; local_only?: boolean; allow_local_degradation?: boolean } = {};
    if (editForm.rateLimit.trim()) {
      const v = parseInt(editForm.rateLimit, 10);
      if (isNaN(v) || v < 0) { setEditError('Rate limit must be a non-negative integer'); return; }
      patch.rate_limit = v;
    }
    if (editForm.dailyLimit.trim()) {
      const v = parseInt(editForm.dailyLimit, 10);
      if (isNaN(v) || v < 0) { setEditError('Daily limit must be a non-negative integer'); return; }
      patch.daily_limit = v;
    }
    if (editForm.monthlyLimit.trim()) {
      const v = parseInt(editForm.monthlyLimit, 10);
      if (isNaN(v) || v < 0) { setEditError('Monthly limit must be a non-negative integer'); return; }
      patch.monthly_limit = v;
    }
    if (editForm.dailyUsdCapDisplay.trim()) {
      const v = toUSD(parseFloat(editForm.dailyUsdCapDisplay));
      if (isNaN(v) || v < 0) { setEditError('Daily USD cap must be a non-negative number'); return; }
      patch.daily_usd_cap = v;
    }
    if (editForm.monthlyUsdCapDisplay.trim()) {
      const v = toUSD(parseFloat(editForm.monthlyUsdCapDisplay));
      if (isNaN(v) || v < 0) { setEditError('Monthly USD cap must be a non-negative number'); return; }
      patch.monthly_usd_cap = v;
    }
    const originalModels = editKey.allowedModels ?? [];
    const modelsChanged = originalModels.length !== editForm.models.length || 
      !editForm.models.every(m => originalModels.includes(m));
    if (modelsChanged) {
      patch.models = editForm.models;
    }
    const originalExpiresAt = editKey.expiresAt ?? '';
    if (editForm.expiresAt !== originalExpiresAt) {
      if (editForm.expiresAt && new Date(editForm.expiresAt).getTime() <= Date.now()) {
        setEditError('Expiry date must be in the future');
        return;
      }
      patch.expires_at = editForm.expiresAt;
    }
    if (editForm.localOnly !== (editKey.localOnly ?? false)) {
      patch.local_only = editForm.localOnly;
    }
    if (editForm.allowLocalDegradation !== (editKey.allowLocalDegradation ?? false)) {
      patch.allow_local_degradation = editForm.allowLocalDegradation;
    }
    if (Object.keys(patch).length === 0) { setEditKey(null); return; }

    if (demoMode) {
      setKeys(prev => prev.map(k => k.id === editKey.id
        ? {
            ...k,
            rateLimit: patch.rate_limit ?? k.rateLimit,
            dailyLimit: patch.daily_limit ?? k.dailyLimit,
            monthlyLimit: patch.monthly_limit ?? k.monthlyLimit,
            dailyUsdCap: patch.daily_usd_cap ?? k.dailyUsdCap,
            monthlyUsdCap: patch.monthly_usd_cap ?? k.monthlyUsdCap,
            allowedModels: patch.models ?? k.allowedModels,
            expiresAt: patch.expires_at !== undefined ? (patch.expires_at || null) : k.expiresAt,
            localOnly: patch.local_only ?? k.localOnly,
            allowLocalDegradation: patch.allow_local_degradation ?? k.allowLocalDegradation,
          }
        : k));
      setEditKey(null);
      return;
    }

    if (!isLive) return;
    setEditSaving(true); setEditError('');
    try {
      await patchKey(editKey.name, patch);
      await loadKeys();
      setEditKey(null);
    } catch (e: any) {
      setEditError(e?.message || 'Failed to save changes');
    } finally {
      setEditSaving(false);
    }
  };

  const [newKeyForm, setNewKeyForm] = useState({
    name: '',
    rateLimit: '1000',
    allowedModels: [] as string[],
    expiresAt: '',
  });
  const [formErrors, setFormErrors] = useState<string[]>([]);

  const loadKeys = async (active: boolean = true) => {
    if (demoMode) {
      if (!active || currentAppPath() !== '/api-keys') return;
      setKeys(mockAPIKeys);
      setIsLive(false);
      setError(null);
      return;
    }
    try {
      const data = await fetchKeys();
      if (!active || currentAppPath() !== '/api-keys') return;
      setKeys(data || []);
      setIsLive(true);
      setError(null);
    } catch (e: any) {
      if (!active || currentAppPath() !== '/api-keys') return;
      setIsLive(false);
      setKeys([]);
      setError(e.message || 'Failed to connect to backend');
    }
  };

  const [availableModels, setAvailableModels] = useState<string[]>([]);

  useEffect(() => {
    if (currentAppPath() !== '/api-keys') return;
    let active = true;
    if (!demoMode) {
      fetchModels().then(data => {
        if (!active || currentAppPath() !== '/api-keys') return;
        setAvailableModels((data.models || []).map((m: any) => m.name));
      }).catch(() => {});
    }
    return () => { active = false; };
  }, [demoMode, location.pathname]);

  useEffect(() => {
    if (currentAppPath() !== '/api-keys') return;
    let active = true;
    loadKeys(active);
    if (demoMode) return () => { active = false; };
    const interval = setInterval(() => loadKeys(active), 30000);
    return () => {
      active = false;
      clearInterval(interval);
    };
  }, [demoMode, location.pathname]);

  useEffect(() => {
    return () => {
      if (newKeyDismissTimer) clearTimeout(newKeyDismissTimer);
    };
  }, [newKeyDismissTimer]);

  const filteredKeys = keys.filter(key =>
    key.name.toLowerCase().includes(searchQuery.toLowerCase()) ||
    key.key.toLowerCase().includes(searchQuery.toLowerCase())
  );

  const copyToClipboard = (key: string, id: string) => {
    const doCopy = () => {
      // Modern Clipboard API (HTTPS / localhost only)
      if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(key).catch(() => legacyCopy(key));
      } else {
        legacyCopy(key);
      }
    };

    const legacyCopy = (text: string) => {
      // Works on plain HTTP. Must run synchronously inside a user-gesture handler.
      const el = document.createElement('textarea');
      el.value = text;
      el.setAttribute('readonly', '');
      // Off-screen but NOT opacity:0 - some browsers block copy from invisible elements.
      el.style.cssText = 'position:absolute;left:-9999px;top:auto;width:1px;height:1px';
      document.body.appendChild(el);
      el.focus();
      el.select();
      el.setSelectionRange(0, text.length);
      try {
        document.execCommand('copy');
      } catch (_) {
        // Last resort: nothing we can do silently
      }
      document.body.removeChild(el);
    };

    doCopy();
    setCopiedId(id);
    setTimeout(() => setCopiedId(null), 2000);
  };

  const dismissNewKey = () => {
    setNewlyCreatedKey(null);
    if (newKeyDismissTimer) clearTimeout(newKeyDismissTimer);
  };

  const handleCreateKey = async () => {
    const errors: string[] = [];
    if (!newKeyForm.name.trim()) {
      errors.push('Name is required');
    }
    const parsedRateLimit = parseInt(newKeyForm.rateLimit, 10);
    if (isNaN(parsedRateLimit) || parsedRateLimit < 0) {
      errors.push('Rate limit must be a non-negative integer');
    }

    if (errors.length > 0) {
      setFormErrors(errors);
      return;
    }

    if (demoMode) {
      // Demo mode: fabricate a key locally for UI preview only
      const demoKey: APIKey = {
        id: `key-${Date.now()}`,
        name: newKeyForm.name,
        key: `sk-demo-${newKeyForm.name.toLowerCase().replace(/\s+/g, '-')}-preview`,
        created: new Date().toISOString().split('T')[0],
        requestsToday: 0,
        requestsThisMonth: 0,
        tokensThisMonth: 0,
        estimatedCostUsd: 0,
        rateLimit: parsedRateLimit,
        status: 'active',
        allowedModels: newKeyForm.allowedModels.length > 0 ? newKeyForm.allowedModels : ['all'],
        expiresAt: newKeyForm.expiresAt || null,
      };
      setKeys([demoKey, ...keys]);
      setIsCreateModalOpen(false);
      setNewKeyForm({ name: '', rateLimit: '1000', allowedModels: [], expiresAt: '' });
      setFormErrors([]);
      return;
    }

    const newKeyData = {
      name: newKeyForm.name,
      rate_limit: parsedRateLimit,
      models: newKeyForm.allowedModels,
      expires_at: newKeyForm.expiresAt || "",
    };

    try {
      const result = await createKey(newKeyData);
      await loadKeys();
      setIsCreateModalOpen(false);
      setNewKeyForm({ name: '', rateLimit: '1000', allowedModels: [], expiresAt: '' });
      setFormErrors([]);
      // Show the generated key for 30 seconds
      setNewlyCreatedKey(result.key);
      const timer = setTimeout(() => setNewlyCreatedKey(null), 30000);
      setNewKeyDismissTimer(timer);
    } catch (e: any) {
      setFormErrors([e?.message || 'Failed to create key on server']);
    }
  };

  const handleRevokeKey = async () => {
    if (!keyToRevoke) return;

    if (isLive) {
      try {
        await revokeKey(keyToRevoke.name);
        await loadKeys();
        setRevokeError(null);
      } catch (e: any) {
        setRevokeError(e?.message || `Failed to revoke key ${keyToRevoke.name}`);
        return;
      }
    } else {
      setKeys(keys.map(k =>
        k.id === keyToRevoke.id ? { ...k, status: 'suspended' as const } : k
      ));
    }
    setIsRevokeModalOpen(false);
    setKeyToRevoke(null);
  };

  const formatNumber = (num: number): string => {
    if (num >= 1000000) return (num / 1000000).toFixed(1) + 'M';
    if (num >= 1000) return (num / 1000).toFixed(1) + 'K';
    return num.toString();
  };

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-foreground">API Keys</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Manage {keys.length} API keys for authentication
          </p>
        </div>
        <div className="flex items-center gap-4">
          <div className="flex items-center gap-2">
            <div className={`w-2 h-2 rounded-full ${isLive || demoMode ? 'bg-success' : 'bg-amber-500'}`} />
            <span className={`text-xs font-medium ${isLive || demoMode ? 'text-success' : 'text-amber-600 dark:text-amber-400'}`}>
              {demoMode ? 'Demo Mode' : (isLive ? 'Live Data' : 'Disconnected')}
            </span>
          </div>
          <button
            onClick={() => setIsCreateModalOpen(true)}
            className="flex items-center gap-2 px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground font-medium rounded-lg transition-colors shadow-sm"
          >
            <Plus className="w-4 h-4" />
            Create Key
          </button>
        </div>
      </div>

      {error && !demoMode && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">
          {error}
        </div>
      )}

      {newlyCreatedKey && (
        <div className="p-4 bg-success/10 border border-success/30 rounded-xl">
          <div className="flex items-start justify-between gap-4">
            <div className="flex-1 min-w-0">
              <p className="text-sm font-semibold text-success mb-2">Key created - copy now, it won't be shown again</p>
              <code className="block font-mono text-sm bg-background border border-border rounded-lg px-3 py-2 break-all text-foreground select-all">
                {newlyCreatedKey}
              </code>
            </div>
            <div className="flex items-center gap-2 shrink-0">
              <button
                onClick={() => copyToClipboard(newlyCreatedKey, '__new__')}
                className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium bg-success/20 hover:bg-success/30 text-success rounded-lg transition-colors"
              >
                <Copy className="w-3.5 h-3.5" />
                {copiedId === '__new__' ? 'Copied!' : 'Copy'}
              </button>
              <button
                onClick={dismissNewKey}
                className="p-1.5 text-muted-foreground hover:text-foreground transition-colors"
                aria-label="Dismiss"
              >
                ✕
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Search */}
      <div className="max-w-md">
        <SearchInput
          value={searchQuery}
          onChange={setSearchQuery}
          placeholder="Search keys by name or value..."
        />
      </div>

      {/* Keys Table (desktop/tablet) */}
      <div className="hidden md:block bg-card border border-border shadow-sm rounded-xl overflow-hidden">
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="bg-secondary/50 border-b border-border text-muted-foreground">
                <th className="px-6 py-3 text-left font-medium">Key Name</th>
                <th className="px-6 py-3 text-left font-medium">Key Value</th>
                <th className="px-6 py-3 text-left font-medium">Created</th>
                <th className="px-6 py-3 text-left font-medium">Requests</th>
                <th className="px-6 py-3 text-left font-medium">Usage (mo)</th>
                <th className="px-6 py-3 text-left font-medium">Rate Limit</th>
                <th className="px-6 py-3 text-left font-medium">Status</th>
                <th className="px-6 py-3 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {filteredKeys.map((key) => (
                <tr key={key.id} className="hover:bg-secondary/30 transition-colors">
                  <td className="px-6 py-4">
                    <div className="flex items-center gap-3">
                      <div className="p-1.5 bg-secondary rounded-lg text-muted-foreground">
                        <Key className="w-4 h-4" />
                      </div>
                      <span className="font-semibold text-foreground">{key.name}</span>
                    </div>
                  </td>
                  <td className="px-6 py-4">
                    {/* The list never carries the full secret. Live keys arrive
                        already masked from the API; demo keys are full, so mask
                        them client-side. There is nothing to reveal or copy. */}
                    <code className="font-mono text-sm text-muted-foreground">
                      {isLive ? key.key : maskKey(key.key)}
                    </code>
                  </td>
                  <td className="px-6 py-4">
                    <span className="text-sm text-muted-foreground">{key.created}</span>
                  </td>
                  <td className="px-6 py-4">
                    <div className="flex flex-col">
                      <span className="font-mono text-foreground font-medium">
                        {formatNumber(key.requestsToday)} <span className="text-muted-foreground text-xs font-sans">today</span>
                      </span>
                      <span className="text-xs font-mono text-muted-foreground">
                        {formatNumber(key.requestsThisMonth)} <span className="font-sans">this month</span>
                      </span>
                    </div>
                  </td>
                  <td className="px-6 py-4">
                    <div className="flex flex-col">
                      <span className="font-mono text-foreground font-medium">
                        {key.tokensThisMonth > 0 ? formatNumber(key.tokensThisMonth) : '-'} <span className="text-muted-foreground text-xs font-sans">tokens</span>
                      </span>
                      <span className="text-xs font-mono text-muted-foreground">
                        {key.estimatedCostUsd > 0 ? `~$${key.estimatedCostUsd.toFixed(2)}` : '-'} <span className="font-sans">est.</span>
                      </span>
                    </div>
                  </td>
                  <td className="px-6 py-4">
                    <span className="font-mono text-sm text-foreground">
                      {(key.rateLimit ?? 0).toLocaleString()}<span className="text-muted-foreground font-sans">/hr</span>
                    </span>
                  </td>
                  <td className="px-6 py-4">
                    <Badge 
                      variant={
                        key.status === 'active' ? 'success' :
                        key.status === 'suspended' || key.status === 'revoked' ? 'muted' : 'warning'
                      }
                      size="sm"
                    >
                      {key.status}
                    </Badge>
                  </td>
                  <td className="px-6 py-4 text-right">
                    <div className="flex items-center justify-end gap-1">
                      <button
                        onClick={() => openEditModal(key)}
                        disabled={(!isLive && !demoMode) || key.status === 'suspended' || key.status === 'revoked' || key.status === 'expired'}
                        title="Edit key limits"
                        className="p-2 text-muted-foreground hover:text-primary disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
                      >
                        <Pencil className="w-4 h-4" />
                      </button>
                      <button
                        onClick={() => {
                          setRevokeError(null);
                          setKeyToRevoke(key);
                          setIsRevokeModalOpen(true);
                        }}
                        disabled={key.status === 'suspended' || key.status === 'revoked'}
                        className="p-2 text-muted-foreground hover:text-destructive disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
                      >
                        <Trash2 className="w-4 h-4" />
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      {/* Keys Cards (mobile) */}
      <div className="md:hidden space-y-3">
        {filteredKeys.map((key) => (
          <div key={key.id} className="bg-card/50 backdrop-blur-sm border border-border/60 rounded-xl p-4 space-y-3">
            <div className="flex items-start justify-between gap-3">
              <div className="flex items-center gap-3 min-w-0">
                <div className="p-1.5 bg-secondary rounded-lg text-muted-foreground shrink-0">
                  <Key className="w-4 h-4" />
                </div>
                <span className="font-semibold text-foreground truncate">{key.name}</span>
              </div>
              <Badge
                variant={
                  key.status === 'active' ? 'success' :
                  key.status === 'suspended' || key.status === 'revoked' ? 'muted' : 'warning'
                }
                size="sm"
              >
                {key.status}
              </Badge>
            </div>

            <div>
              <p className="text-[10px] uppercase tracking-wider text-muted-foreground mb-1">Key Value</p>
              <code className="block font-mono text-sm text-foreground break-all">
                {isLive ? key.key : maskKey(key.key)}
              </code>
            </div>

            <div className="grid grid-cols-2 gap-3">
              <div>
                <p className="text-[10px] uppercase tracking-wider text-muted-foreground mb-1">Created</p>
                <p className="text-sm text-foreground">{key.created}</p>
              </div>
              <div>
                <p className="text-[10px] uppercase tracking-wider text-muted-foreground mb-1">Rate Limit</p>
                <p className="text-sm text-foreground font-mono">
                  {(key.rateLimit ?? 0).toLocaleString()}<span className="text-muted-foreground font-sans">/hr</span>
                </p>
              </div>
              <div>
                <p className="text-[10px] uppercase tracking-wider text-muted-foreground mb-1">Requests</p>
                <p className="text-sm text-foreground font-mono">
                  {formatNumber(key.requestsToday)} <span className="text-muted-foreground text-xs font-sans">today</span>
                </p>
                <p className="text-xs text-muted-foreground font-mono">
                  {formatNumber(key.requestsThisMonth)} <span className="font-sans">this month</span>
                </p>
              </div>
              <div>
                <p className="text-[10px] uppercase tracking-wider text-muted-foreground mb-1">Usage (mo)</p>
                <p className="text-sm text-foreground font-mono">
                  {key.tokensThisMonth > 0 ? formatNumber(key.tokensThisMonth) : '-'} <span className="text-muted-foreground text-xs font-sans">tokens</span>
                </p>
                <p className="text-xs text-muted-foreground font-mono">
                  {key.estimatedCostUsd > 0 ? `~$${key.estimatedCostUsd.toFixed(2)}` : '-'} <span className="font-sans">est.</span>
                </p>
              </div>
            </div>

            <div className="flex items-center justify-end gap-1 pt-1 border-t border-border/60">
              <button
                onClick={() => openEditModal(key)}
                disabled={(!isLive && !demoMode) || key.status === 'suspended' || key.status === 'revoked' || key.status === 'expired'}
                title="Edit key limits"
                className="p-2 text-muted-foreground hover:text-primary disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
              >
                <Pencil className="w-4 h-4" />
              </button>
              <button
                onClick={() => {
                  setRevokeError(null);
                  setKeyToRevoke(key);
                  setIsRevokeModalOpen(true);
                }}
                disabled={key.status === 'suspended' || key.status === 'revoked'}
                className="p-2 text-muted-foreground hover:text-destructive disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
              >
                <Trash2 className="w-4 h-4" />
              </button>
            </div>
          </div>
        ))}
      </div>

      {filteredKeys.length === 0 && (
        <div className="text-center py-12">
          <Key className="w-12 h-12 text-muted-foreground/30 mx-auto mb-4" />
          <p className="text-muted-foreground">No API keys found matching your search.</p>
        </div>
      )}

      {/* Create Key Modal */}
      <Modal
        isOpen={isCreateModalOpen}
        onClose={() => {
          setIsCreateModalOpen(false);
          setFormErrors([]);
          setNewKeyForm({ name: '', rateLimit: '1000', allowedModels: [], expiresAt: '' });
        }}
        title="Create New API Key"
      >
        <div className="space-y-4">
          {formErrors.length > 0 && (
            <div className="p-3 bg-destructive/10 border border-destructive/20 rounded-lg">
              {formErrors.map((error, i) => (
                <p key={i} className="text-sm font-medium text-destructive">{error}</p>
              ))}
            </div>
          )}
          
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Key Name <span className="text-destructive">*</span>
            </label>
            <input
              type="text"
              value={newKeyForm.name}
              onChange={(e) => setNewKeyForm({ ...newKeyForm, name: e.target.value })}
              placeholder="e.g., Production API"
              className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
            />
          </div>
          
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Rate Limit (requests/hour)
            </label>
            <input
              type="number"
              value={newKeyForm.rateLimit}
              onChange={(e) => setNewKeyForm({ ...newKeyForm, rateLimit: e.target.value })}
              className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
            />
          </div>
          
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Allowed Models (optional)
            </label>
            <div className="max-h-32 overflow-y-auto bg-secondary border border-border rounded-lg p-2">
              <label className="flex items-center gap-2 p-1.5 hover:bg-background rounded cursor-pointer">
                <input
                  type="checkbox"
                  checked={newKeyForm.allowedModels.length === 0}
                  onChange={() => setNewKeyForm({ ...newKeyForm, allowedModels: [] })}
                  className="rounded border-border bg-background text-primary focus:ring-primary/20"
                />
                <span className="text-sm text-foreground">All models</span>
              </label>
              {availableModels.map((model) => (
                <label key={model} className="flex items-center gap-2 p-1.5 hover:bg-background rounded cursor-pointer">
                  <input
                    type="checkbox"
                    checked={newKeyForm.allowedModels.includes(model)}
                    onChange={(e) => {
                      if (e.target.checked) {
                        setNewKeyForm({
                          ...newKeyForm,
                          allowedModels: [...newKeyForm.allowedModels, model],
                        });
                      } else {
                        setNewKeyForm({
                          ...newKeyForm,
                          allowedModels: newKeyForm.allowedModels.filter(m => m !== model),
                        });
                      }
                    }}
                    className="rounded border-border bg-background text-primary focus:ring-primary/20"
                  />
                  <span className="text-sm font-mono text-muted-foreground">{model}</span>
                </label>
              ))}
            </div>
          </div>
          
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Expiry Date & Time (optional)
            </label>
            <CustomDateTimePicker
              value={newKeyForm.expiresAt}
              onChange={(val) => setNewKeyForm({ ...newKeyForm, expiresAt: val })}
              placeholder="Never expires"
              min={nowLocalISO()}
            />
          </div>
          
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setIsCreateModalOpen(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleCreateKey}
              className="px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Create Key
            </button>
          </div>
        </div>
      </Modal>

      {/* Edit Key Modal */}
      <Modal
        isOpen={editKey !== null}
        onClose={() => setEditKey(null)}
        title={`Edit Key: ${editKey?.name ?? ''}`}
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Update limits and allowed models at runtime. Counters and the key token are preserved.
          </p>
          <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Rate Limit (req/hr)</label>
              <input type="number" min="0" value={editForm.rateLimit}
                onChange={e => setEditForm({ ...editForm, rateLimit: e.target.value })}
                placeholder="e.g. 1000"
                className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Daily Limit</label>
              <input type="number" min="0" value={editForm.dailyLimit}
                onChange={e => setEditForm({ ...editForm, dailyLimit: e.target.value })}
                placeholder="0 = unlimited"
                className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Monthly Limit</label>
              <input type="number" min="0" value={editForm.monthlyLimit}
                onChange={e => setEditForm({ ...editForm, monthlyLimit: e.target.value })}
                placeholder="0 = unlimited"
                className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>
          </div>
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5 sm:h-10 flex sm:items-end">Daily Cloud Spend Cap ({currency.code})</label>
              <input type="number" min="0" step="0.01"
                value={editForm.dailyUsdCapDisplay}
                onChange={e => setEditForm({ ...editForm, dailyUsdCapDisplay: e.target.value })}
                placeholder="0 = unlimited"
                className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5 sm:h-10 flex sm:items-end">Monthly Cloud Spend Cap ({currency.code})</label>
              <input type="number" min="0" step="0.01"
                value={editForm.monthlyUsdCapDisplay}
                onChange={e => setEditForm({ ...editForm, monthlyUsdCapDisplay: e.target.value })}
                placeholder="0 = unlimited"
                className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>
          </div>
          <div className="flex items-center justify-between gap-3 py-1">
            <div>
              <div className="text-sm font-medium text-foreground">Local-only (never use cloud fallback)</div>
              <div className="text-xs text-muted-foreground">Requests will fail instead of spilling to a cloud provider.</div>
            </div>
            <button
              type="button"
              role="switch"
              aria-checked={editForm.localOnly}
              onClick={() => setEditForm({ ...editForm, localOnly: !editForm.localOnly })}
              className={`relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition-colors ${editForm.localOnly ? 'bg-primary' : 'bg-muted-foreground/30'}`}
            >
              <span className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${editForm.localOnly ? 'translate-x-6' : 'translate-x-1'}`} />
            </button>
          </div>
          <div className="flex items-center justify-between gap-3 py-1">
            <div>
              <div className="text-sm font-medium text-foreground">Allow local degradation</div>
              <div className="text-xs text-muted-foreground">Lets this key receive an operator-configured local alternate model (routing.local_degradation_chains) when its requested model is unavailable, before cloud fallback.</div>
            </div>
            <button
              type="button"
              role="switch"
              aria-checked={editForm.allowLocalDegradation}
              onClick={() => setEditForm({ ...editForm, allowLocalDegradation: !editForm.allowLocalDegradation })}
              className={`relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition-colors ${editForm.allowLocalDegradation ? 'bg-primary' : 'bg-muted-foreground/30'}`}
            >
              <span className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${editForm.allowLocalDegradation ? 'translate-x-6' : 'translate-x-1'}`} />
            </button>
          </div>
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Expiry Date & Time (optional)
            </label>
            <CustomDateTimePicker
              value={editForm.expiresAt}
              onChange={(val) => setEditForm({ ...editForm, expiresAt: val })}
              placeholder="Never expires"
              min={nowLocalISO()}
            />
          </div>
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Allowed Models (optional)
            </label>
            <div className="max-h-32 overflow-y-auto bg-secondary border border-border rounded-lg p-2">
              <label className="flex items-center gap-2 p-1.5 hover:bg-background rounded cursor-pointer">
                <input
                  type="checkbox"
                  checked={editForm.models.length === 0}
                  onChange={() => setEditForm({ ...editForm, models: [] })}
                  className="rounded border-border bg-background text-primary focus:ring-primary/20"
                />
                <span className="text-sm text-foreground">All models</span>
              </label>
              {availableModels.map((model) => (
                <label key={model} className="flex items-center gap-2 p-1.5 hover:bg-background rounded cursor-pointer">
                  <input
                    type="checkbox"
                    checked={editForm.models.includes(model)}
                    onChange={(e) => {
                      if (e.target.checked) {
                        setEditForm({ ...editForm, models: [...editForm.models, model] });
                      } else {
                        setEditForm({ ...editForm, models: editForm.models.filter(m => m !== model) });
                      }
                    }}
                    className="rounded border-border bg-background text-primary focus:ring-primary/20"
                  />
                  <span className="text-sm font-mono text-muted-foreground">{model}</span>
                </label>
              ))}
            </div>
          </div>
          {editError && <p className="text-sm text-destructive">{editError}</p>}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button onClick={() => setEditKey(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors">
              Cancel
            </button>
            <button onClick={handleSaveKeyPatch} disabled={editSaving}
              className="px-4 py-2 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm">
              {editSaving ? 'Saving...' : 'Save Changes'}
            </button>
          </div>
        </div>
      </Modal>

      {/* Revoke Confirmation Modal */}
      <Modal
        isOpen={isRevokeModalOpen}
        onClose={() => {
          setIsRevokeModalOpen(false);
          setKeyToRevoke(null);
          setRevokeError(null);
        }}
        title="Revoke API Key"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Are you sure you want to revoke the API key <span className="text-foreground font-semibold">{keyToRevoke?.name}</span>?
          </p>
          <p className="text-xs text-muted-foreground">
            This action cannot be undone. Any applications using this key will immediately lose access.
          </p>
          {revokeError && (
            <p className="text-sm text-destructive">{revokeError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setIsRevokeModalOpen(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleRevokeKey}
              className="px-4 py-2 bg-destructive hover:bg-destructive/90 text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Revoke Key
            </button>
          </div>
        </div>
      </Modal>
    </div>
  );
}

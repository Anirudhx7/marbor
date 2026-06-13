import { useState, useEffect } from 'react';
import { Plus, Copy, Eye, EyeOff, Trash2, Key } from 'lucide-react';
import { Badge } from '../components/Badge';
import { Modal } from '../components/Modal';
import { SearchInput } from '../components/SearchInput';
import { mockAPIKeys } from '../lib/mockData';
import { fetchKeys, createKey, revokeKey } from '../lib/api';
import type { APIKey } from '../types';

const MODELS = [
  'llama3.2:3b',
  'llama3.1:8b',
  'llama3.1:70b',
  'mistral:7b',
  'mixtral:8x7b',
  'qwen2.5:7b',
  'qwen2.5:14b',
  'gemma2:9b',
  'phi3:medium',
  'codellama:13b',
];

function maskKey(key: string): string {
  const parts = key.split('-');
  if (parts.length >= 3) {
    return `${parts[0]}-${parts[1]}-****-****`;
  }
  return key.slice(0, 12) + '****';
}

import { useDemoMode } from '../hooks/useDemoMode';

export function APIKeys() {
  const { demoMode } = useDemoMode();
  const [keys, setKeys] = useState<APIKey[]>(demoMode ? mockAPIKeys : []);
  const [isLive, setIsLive] = useState(!demoMode);
  const [searchQuery, setSearchQuery] = useState('');
  const [visibleKeys, setVisibleKeys] = useState<Set<string>>(new Set());
  const [isCreateModalOpen, setIsCreateModalOpen] = useState(false);
  const [isRevokeModalOpen, setIsRevokeModalOpen] = useState(false);
  const [keyToRevoke, setKeyToRevoke] = useState<APIKey | null>(null);
  const [copiedId, setCopiedId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [newlyCreatedKey, setNewlyCreatedKey] = useState<string | null>(null);
  const [newKeyDismissTimer, setNewKeyDismissTimer] = useState<ReturnType<typeof setTimeout> | null>(null);

  const [newKeyForm, setNewKeyForm] = useState({
    name: '',
    rateLimit: '1000',
    allowedModels: [] as string[],
    expiresAt: '',
  });
  const [formErrors, setFormErrors] = useState<string[]>([]);

  const loadKeys = async () => {
    if (demoMode) {
      setKeys(mockAPIKeys);
      setIsLive(false);
      setError(null);
      return;
    }
    try {
      const data = await fetchKeys();
      setKeys(data || []);
      setIsLive(true);
      setError(null);
    } catch (e: any) {
      setIsLive(false);
      setKeys([]);
      setError(e.message || 'Failed to connect to backend');
    }
  };

  useEffect(() => {
    loadKeys();
    const interval = setInterval(loadKeys, 30000);
    return () => clearInterval(interval);
  }, [demoMode]);

  const filteredKeys = keys.filter(key =>
    key.name.toLowerCase().includes(searchQuery.toLowerCase()) ||
    key.key.toLowerCase().includes(searchQuery.toLowerCase())
  );

  const toggleKeyVisibility = (id: string) => {
    const newVisible = new Set(visibleKeys);
    if (newVisible.has(id)) {
      newVisible.delete(id);
    } else {
      newVisible.add(id);
    }
    setVisibleKeys(newVisible);
  };

  const copyToClipboard = async (key: string, id: string) => {
    await navigator.clipboard.writeText(key);
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
        rateLimit: parseInt(newKeyForm.rateLimit),
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
      rate_limit: parseInt(newKeyForm.rateLimit),
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
    } catch (e) {
      setFormErrors(['Failed to create key on server']);
    }
  };

  const handleRevokeKey = async () => {
    if (!keyToRevoke) return;
    
    if (isLive) {
      try {
        await revokeKey(keyToRevoke.name);
        loadKeys();
      } catch (e) {
        console.error('Failed to revoke key');
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
            <div className={`w-2 h-2 rounded-full ${isLive ? 'bg-success' : 'bg-amber-500'}`} />
            <span className={`text-xs font-medium ${isLive ? 'text-success' : 'text-amber-600 dark:text-amber-400'}`}>
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

      {/* Keys Table */}
      <div className="bg-card border border-border shadow-sm rounded-xl overflow-hidden">
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
                    <div className="flex items-center gap-2">
                      <code className="font-mono text-sm text-muted-foreground">
                        {visibleKeys.has(key.id) ? key.key : maskKey(key.key)}
                      </code>
                      <button
                        onClick={() => toggleKeyVisibility(key.id)}
                        className="p-1 text-muted-foreground hover:text-foreground transition-colors"
                      >
                        {visibleKeys.has(key.id) ? (
                          <EyeOff className="w-4 h-4" />
                        ) : (
                          <Eye className="w-4 h-4" />
                        )}
                      </button>
                      <button
                        onClick={() => copyToClipboard(key.key, key.id)}
                        className="p-1 text-muted-foreground hover:text-primary transition-colors"
                      >
                        <Copy className="w-4 h-4" />
                      </button>
                      {copiedId === key.id && (
                        <span className="text-xs font-medium text-primary">Copied!</span>
                      )}
                    </div>
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
                        {key.tokensThisMonth > 0 ? formatNumber(key.tokensThisMonth) : '—'} <span className="text-muted-foreground text-xs font-sans">tokens</span>
                      </span>
                      <span className="text-xs font-mono text-muted-foreground">
                        {key.estimatedCostUsd > 0 ? `~$${key.estimatedCostUsd.toFixed(2)}` : '—'} <span className="font-sans">est.</span>
                      </span>
                    </div>
                  </td>
                  <td className="px-6 py-4">
                    <span className="font-mono text-sm text-foreground">
                      {key.rateLimit.toLocaleString()}<span className="text-muted-foreground font-sans">/hr</span>
                    </span>
                  </td>
                  <td className="px-6 py-4">
                    <Badge 
                      variant={
                        key.status === 'active' ? 'success' : 
                        key.status === 'suspended' ? 'muted' : 'warning'
                      }
                      size="sm"
                    >
                      {key.status}
                    </Badge>
                  </td>
                  <td className="px-6 py-4 text-right">
                    <button
                      onClick={() => {
                        setKeyToRevoke(key);
                        setIsRevokeModalOpen(true);
                      }}
                      disabled={key.status === 'suspended'}
                      className="p-2 text-muted-foreground hover:text-destructive disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
                    >
                      <Trash2 className="w-4 h-4" />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
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
              {MODELS.map((model) => (
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
              Expiry Date (optional)
            </label>
            <input
              type="date"
              value={newKeyForm.expiresAt}
              onChange={(e) => setNewKeyForm({ ...newKeyForm, expiresAt: e.target.value })}
              className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
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

      {/* Revoke Confirmation Modal */}
      <Modal
        isOpen={isRevokeModalOpen}
        onClose={() => {
          setIsRevokeModalOpen(false);
          setKeyToRevoke(null);
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

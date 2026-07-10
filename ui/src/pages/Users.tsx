import { useState, useEffect, useCallback } from 'react';
import { Users as UsersIcon, Plus, Check, Ban, Trash2, UserCheck, Key, RotateCcw } from 'lucide-react';
import { listUsers, createUser, approveUser, suspendUser, deleteUser, resetUserPassword, fetchKeys, fetchModels, loadSession } from '../lib/api';
import type { UserRecord, APIKey, ModelCatalog } from '../types';
import { Badge } from '../components/Badge';
import { Tooltip } from '../components/Tooltip';

const STATUS_BADGE: Record<string, { variant: 'warning' | 'success' | 'destructive' | 'muted'; label: string }> = {
  pending:   { variant: 'warning',     label: 'Pending' },
  active:    { variant: 'success',     label: 'Active' },
  suspended: { variant: 'destructive', label: 'Suspended' },
};

const ROLE_BADGE: Record<string, { variant: 'primary' | 'muted'; label: string }> = {
  admin: { variant: 'primary', label: 'Admin' },
  user:  { variant: 'muted',   label: 'User' },
};

// ── Approve Modal ─────────────────────────────────────────────────────────────

interface ApproveModalProps {
  user: UserRecord;
  onClose: () => void;
  onDone: () => void;
}

function ApproveModal({ user, onClose, onDone }: ApproveModalProps) {
  const [mode, setMode] = useState<'create' | 'assign'>('create');
  const [existingKeyName, setExistingKeyName] = useState('');
  const [newKeyName, setNewKeyName] = useState(`${user.username}-key`);
  const [rateLimit, setRateLimit] = useState(1000);
  const [dailyLimit, setDailyLimit] = useState(0);
  const [monthlyLimit, setMonthlyLimit] = useState(0);
  const [selectedModels, setSelectedModels] = useState<string[]>([]);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [createdKey, setCreatedKey] = useState<string | null>(null);
  const [activeKeys, setActiveKeys] = useState<APIKey[]>([]);
  const [availableModels, setAvailableModels] = useState<string[]>([]);

  useEffect(() => {
    fetchKeys().then(keys => setActiveKeys(keys.filter(k => k.status === 'active'))).catch(() => {});
    fetchModels().then((cat: ModelCatalog) => setAvailableModels(cat.models.map(m => m.name))).catch(() => {});
  }, []);

  function toggleModel(model: string) {
    setSelectedModels(prev =>
      prev.includes(model) ? prev.filter(m => m !== model) : [...prev, model]
    );
  }

  async function handleApprove() {
    setSaving(true);
    setError(null);
    try {
      const payload = mode === 'assign'
        ? { api_key_name: existingKeyName }
        : {
            create_key: {
              name: newKeyName,
              rate_limit_per_hour: rateLimit,
              daily_limit: dailyLimit,
              monthly_limit: monthlyLimit,
              models: selectedModels,
            },
          };
      const res = await approveUser(user.id, payload);
      if (res.api_key_value) {
        setCreatedKey(res.api_key_value);
      } else {
        onDone();
      }
    } catch (err: any) {
      setError(err.message || 'Approval failed');
    } finally {
      setSaving(false);
    }
  }

  if (createdKey) {
    return (
      <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 backdrop-blur-sm p-4">
        <div className="bg-card border border-border rounded-xl p-6 w-full max-w-md shadow-xl">
          <div className="flex items-center gap-3 mb-4">
            <div className="p-2 bg-green-500/10 rounded-lg">
              <Key className="w-5 h-5 text-green-600 dark:text-green-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">User Approved</h3>
              <p className="text-xs text-muted-foreground">Share this API key - shown once only</p>
            </div>
          </div>
          <p className="text-xs text-muted-foreground mb-2">API key for <strong>{user.username}</strong>:</p>
          <code className="block w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-xs font-mono text-primary break-all mb-2 select-all">
            {createdKey}
          </code>
          <p className="text-xs text-amber-600 dark:text-amber-400 mb-4">This key will not be shown again. Copy it now.</p>
          <button onClick={onDone} className="w-full py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 transition-colors">
            Done
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 backdrop-blur-sm p-4">
      <div className="bg-card border border-border rounded-xl p-6 w-full max-w-md shadow-xl max-h-[90vh] overflow-y-auto">
        <div className="flex items-center gap-3 mb-5">
          <div className="p-2 bg-green-500/10 rounded-lg">
            <UserCheck className="w-5 h-5 text-green-600 dark:text-green-400" />
          </div>
          <div>
            <h3 className="text-sm font-semibold text-foreground">Approve {user.username}</h3>
            <p className="text-xs text-muted-foreground">Assign an API key to activate this user</p>
          </div>
        </div>

        <div className="flex gap-2 mb-4">
          {(['create', 'assign'] as const).map(m => (
            <button key={m} onClick={() => setMode(m)}
              className={`flex-1 py-1.5 text-xs font-medium rounded-lg border transition-colors ${mode === m ? 'bg-primary/10 border-primary text-primary' : 'border-border text-muted-foreground hover:bg-secondary'}`}>
              {m === 'create' ? 'Create New Key' : 'Assign Existing Key'}
            </button>
          ))}
        </div>

        {mode === 'assign' ? (
          <div>
            <label className="block text-xs font-medium text-muted-foreground mb-1.5">Select Active Key</label>
            {activeKeys.length === 0 ? (
              <p className="text-xs text-muted-foreground italic">No active keys found.</p>
            ) : (
              <select value={existingKeyName} onChange={e => setExistingKeyName(e.target.value)}
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50">
                <option value="">-- select a key --</option>
                {activeKeys.map(k => <option key={k.name} value={k.name}>{k.name}</option>)}
              </select>
            )}
          </div>
        ) : (
          <div className="space-y-3">
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1.5">Key Name</label>
              <input type="text" value={newKeyName} onChange={e => setNewKeyName(e.target.value)}
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div className="grid grid-cols-1 sm:grid-cols-3 gap-2">
              {([['Rate/hr', rateLimit, setRateLimit], ['Daily', dailyLimit, setDailyLimit], ['Monthly', monthlyLimit, setMonthlyLimit]] as const).map(([label, val, setter]) => (
                <div key={label}>
                  <label className="block text-xs font-medium text-muted-foreground mb-1.5">{label}</label>
                  <input type="number" value={val} min={0} onChange={e => (setter as any)(+e.target.value)}
                    className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
                </div>
              ))}
            </div>
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1.5">
                Allowed Models <span className="text-muted-foreground/60">(none selected = all models)</span>
              </label>
              <div className="max-h-36 overflow-y-auto bg-secondary/50 border border-border rounded-lg p-2 space-y-0.5">
                <label className="flex items-center gap-2 p-1.5 hover:bg-background rounded cursor-pointer">
                  <input type="checkbox" checked={selectedModels.length === 0}
                    onChange={() => setSelectedModels([])}
                    className="rounded border-border bg-background text-primary focus:ring-primary/20" />
                  <span className="text-xs text-foreground">All models</span>
                </label>
                {availableModels.map(model => (
                  <label key={model} className="flex items-center gap-2 p-1.5 hover:bg-background rounded cursor-pointer">
                    <input type="checkbox" checked={selectedModels.includes(model)}
                      onChange={() => toggleModel(model)}
                      className="rounded border-border bg-background text-primary focus:ring-primary/20" />
                    <span className="text-xs font-mono text-muted-foreground">{model}</span>
                  </label>
                ))}
                {availableModels.length === 0 && (
                  <p className="text-xs text-muted-foreground italic p-1">No models loaded on cluster yet.</p>
                )}
              </div>
            </div>
          </div>
        )}

        {error && <p className="text-xs text-destructive bg-destructive/10 px-3 py-2 rounded-lg mt-3">{error}</p>}

        <div className="flex gap-2 mt-5">
          <button onClick={onClose} className="flex-1 py-2 border border-border text-sm font-medium rounded-lg text-muted-foreground hover:bg-secondary transition-colors">
            Cancel
          </button>
          <button onClick={handleApprove} disabled={saving}
            className="flex-1 py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 transition-colors disabled:opacity-50">
            {saving ? 'Approving...' : 'Approve'}
          </button>
        </div>
      </div>
    </div>
  );
}

// ── Reset Password Modal ──────────────────────────────────────────────────────

interface ResetPasswordModalProps {
  user: UserRecord;
  onClose: () => void;
}

function ResetPasswordModal({ user, onClose }: ResetPasswordModalProps) {
  const [loading, setLoading] = useState(true);
  const [password, setPassword] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    resetUserPassword(user.id)
      .then(r => { setPassword(r.initial_password); setLoading(false); })
      .catch(err => { setError(err.message || 'Failed'); setLoading(false); });
  }, [user.id]);

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 backdrop-blur-sm p-4">
      <div className="bg-card border border-border rounded-xl p-6 w-full max-w-md shadow-xl">
        <div className="flex items-center gap-3 mb-4">
          <div className="p-2 bg-amber-500/10 rounded-lg">
            <RotateCcw className="w-5 h-5 text-amber-600 dark:text-amber-400" />
          </div>
          <div>
            <h3 className="text-sm font-semibold text-foreground">Reset Password</h3>
            <p className="text-xs text-muted-foreground">{user.username}</p>
          </div>
        </div>
        {loading && <p className="text-sm text-muted-foreground">Generating new password...</p>}
        {error && <p className="text-xs text-destructive bg-destructive/10 px-3 py-2 rounded-lg">{error}</p>}
        {password && (
          <>
            <p className="text-xs text-muted-foreground mb-2">New temporary password (shown once):</p>
            <code className="block w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-xs font-mono text-primary break-all mb-2 select-all">
              {password}
            </code>
            <p className="text-xs text-amber-600 dark:text-amber-400 mb-4">
              User will be prompted to change it on next login. All active sessions revoked.
            </p>
          </>
        )}
        <button onClick={onClose} className="w-full py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 transition-colors">
          Done
        </button>
      </div>
    </div>
  );
}

// ── Create User Modal ─────────────────────────────────────────────────────────

interface CreateUserModalProps {
  onClose: () => void;
  onDone: () => void;
}

function CreateUserModal({ onClose, onDone }: CreateUserModalProps) {
  const [username, setUsername] = useState('');
  const [email, setEmail] = useState('');
  const [role, setRole] = useState<'user' | 'admin'>('user');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<{ username: string; initial_password: string } | null>(null);

  async function handleCreate() {
    if (!username) { setError('Username is required'); return; }
    setSaving(true);
    setError(null);
    try {
      const res = await createUser({ username, email, role });
      setCreated({ username: res.username, initial_password: res.initial_password });
    } catch (err: any) {
      setError(err.message || 'Failed to create user');
    } finally {
      setSaving(false);
    }
  }

  if (created) {
    return (
      <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 backdrop-blur-sm p-4">
        <div className="bg-card border border-border rounded-xl p-6 w-full max-w-md shadow-xl">
          <h3 className="text-sm font-semibold text-foreground mb-4">User Created</h3>
          <p className="text-xs text-muted-foreground mb-2">Initial password for <strong>{created.username}</strong>:</p>
          <code className="block w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-xs font-mono text-primary break-all mb-2 select-all">
            {created.initial_password}
          </code>
          <p className="text-xs text-amber-600 dark:text-amber-400 mb-4">Share with the user. They will be prompted to change it on first login.</p>
          <button onClick={onDone} className="w-full py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 transition-colors">
            Done
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 backdrop-blur-sm p-4">
      <div className="bg-card border border-border rounded-xl p-6 w-full max-w-md shadow-xl">
        <div className="flex items-center gap-3 mb-5">
          <div className="p-2 bg-blue-500/10 rounded-lg">
            <Plus className="w-5 h-5 text-blue-600 dark:text-blue-400" />
          </div>
          <div>
            <h3 className="text-sm font-semibold text-foreground">Create User</h3>
            <p className="text-xs text-muted-foreground">An initial password will be generated</p>
          </div>
        </div>

        <div className="space-y-3">
          <div>
            <label className="block text-xs font-medium text-muted-foreground mb-1.5">Username</label>
            <input type="text" value={username} onChange={e => setUsername(e.target.value)} autoFocus
              className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
          </div>
          <div>
            <label className="block text-xs font-medium text-muted-foreground mb-1.5">Email <span className="text-muted-foreground/60">(optional)</span></label>
            <input type="email" value={email} onChange={e => setEmail(e.target.value)}
              className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
          </div>
          <div>
            <label className="block text-xs font-medium text-muted-foreground mb-1.5">Role</label>
            <select value={role} onChange={e => setRole(e.target.value as any)}
              className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50">
              <option value="user">User</option>
              <option value="admin">Admin</option>
            </select>
          </div>
        </div>

        {error && <p className="text-xs text-destructive bg-destructive/10 px-3 py-2 rounded-lg mt-3">{error}</p>}

        <div className="flex gap-2 mt-5">
          <button onClick={onClose} className="flex-1 py-2 border border-border text-sm font-medium rounded-lg text-muted-foreground hover:bg-secondary transition-colors">
            Cancel
          </button>
          <button onClick={handleCreate} disabled={saving}
            className="flex-1 py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 transition-colors disabled:opacity-50">
            {saving ? 'Creating...' : 'Create'}
          </button>
        </div>
      </div>
    </div>
  );
}

// ── Main Users Page ───────────────────────────────────────────────────────────

export function Users() {
  const currentUsername = loadSession()?.username ?? '';
  const [users, setUsers] = useState<UserRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [approveTarget, setApproveTarget] = useState<UserRecord | null>(null);
  const [resetTarget, setResetTarget] = useState<UserRecord | null>(null);
  const [showCreate, setShowCreate] = useState(false);

  const load = useCallback(async () => {
    try {
      const data = await listUsers();
      setUsers(data);
      setError(null);
    } catch (err: any) {
      setError(err.message || 'Failed to load users');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { load(); }, [load]);

  async function handleSuspend(u: UserRecord) {
    try {
      await suspendUser(u.id);
      load();
    } catch (err: any) {
      alert(err.message || 'Failed to suspend user');
    }
  }

  async function handleDelete(u: UserRecord) {
    if (!confirm(`Delete user "${u.username}"? They will be soft-deleted and removed from the active list.`)) return;
    try {
      await deleteUser(u.id);
      load();
    } catch (err: any) {
      alert(err.message || 'Failed to delete user');
    }
  }

  const pendingCount = users.filter(u => u.status === 'pending').length;

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-3">
          <UsersIcon className="w-6 h-6 text-primary" />
          <div>
            <h1 className="text-xl font-bold text-foreground">Users</h1>
            <p className="text-xs text-muted-foreground">Manage dashboard access and API key assignments</p>
          </div>
          {pendingCount > 0 && (
            <span className="px-2 py-0.5 text-xs font-bold bg-amber-500/20 text-amber-600 dark:text-amber-400 rounded-full">
              {pendingCount} pending
            </span>
          )}
        </div>
        <button
          onClick={() => setShowCreate(true)}
          className="flex items-center gap-2 px-3 py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 transition-colors"
        >
          <Plus className="w-4 h-4" />
          Create User
        </button>
      </div>

      {error && (
        <div className="bg-destructive/10 border border-destructive/20 rounded-xl p-4 text-sm text-destructive">{error}</div>
      )}

      <div className="bg-card border border-border rounded-xl shadow-sm overflow-hidden">
        {loading ? (
          <div className="p-8 text-center text-sm text-muted-foreground">Loading...</div>
        ) : users.length === 0 ? (
          <div className="p-8 text-center text-sm text-muted-foreground">No users yet.</div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border bg-secondary/30">
                  <th className="text-left px-4 py-3 text-xs font-semibold text-muted-foreground">Username</th>
                  <th className="text-left px-4 py-3 text-xs font-semibold text-muted-foreground">Email</th>
                  <th className="text-left px-4 py-3 text-xs font-semibold text-muted-foreground">Role</th>
                  <th className="text-left px-4 py-3 text-xs font-semibold text-muted-foreground">Status</th>
                  <th className="text-left px-4 py-3 text-xs font-semibold text-muted-foreground">API Key</th>
                  <th className="text-left px-4 py-3 text-xs font-semibold text-muted-foreground">Created</th>
                  <th className="text-right px-4 py-3 text-xs font-semibold text-muted-foreground">Actions</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {users.map(u => {
                  const sb = STATUS_BADGE[u.status] ?? { variant: 'muted' as const, label: u.status };
                  const rb = ROLE_BADGE[u.role] ?? { variant: 'muted' as const, label: u.role };
                  return (
                    <tr key={u.id} className="hover:bg-secondary/20 transition-colors">
                      <td className="px-4 py-3 font-medium text-foreground">{u.username}</td>
                      <td className="px-4 py-3 text-muted-foreground">{u.email || '-'}</td>
                      <td className="px-4 py-3">
                        <Badge variant={rb.variant}>{rb.label}</Badge>
                      </td>
                      <td className="px-4 py-3">
                        <Badge variant={sb.variant}>{sb.label}</Badge>
                      </td>
                      <td className="px-4 py-3 font-mono text-xs text-muted-foreground">
                        {u.api_key_name || '-'}
                      </td>
                      <td className="px-4 py-3 text-xs text-muted-foreground">
                        {new Date(u.created_at).toLocaleDateString()}
                      </td>
                      <td className="px-4 py-3">
                        <div className="flex items-center justify-end gap-1">
                          {u.status === 'pending' && (
                            <Tooltip label="Approve user">
                              <button onClick={() => setApproveTarget(u)}
                                className="p-1.5 rounded-md text-green-600 hover:bg-green-500/10 transition-colors">
                                <Check className="w-4 h-4" />
                              </button>
                            </Tooltip>
                          )}
                          {u.status === 'active' && (
                            <Tooltip label="Suspend user">
                              <button onClick={() => handleSuspend(u)}
                                className="p-1.5 rounded-md text-amber-600 hover:bg-amber-500/10 transition-colors">
                                <Ban className="w-4 h-4" />
                              </button>
                            </Tooltip>
                          )}
                          {u.status === 'suspended' && (
                            <Tooltip label="Reactivate user">
                              <button onClick={() => setApproveTarget(u)}
                                className="p-1.5 rounded-md text-primary hover:bg-primary/10 transition-colors">
                                <UserCheck className="w-4 h-4" />
                              </button>
                            </Tooltip>
                          )}
                          <Tooltip label={u.username === currentUsername ? 'Use Settings to change your own password' : 'Reset password'}>
                            <button
                              onClick={() => setResetTarget(u)}
                              disabled={u.username === currentUsername}
                              className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors disabled:opacity-30 disabled:cursor-not-allowed disabled:hover:bg-transparent disabled:hover:text-muted-foreground"
                            >
                              <RotateCcw className="w-4 h-4" />
                            </button>
                          </Tooltip>
                          <Tooltip label="Delete user">
                            <button onClick={() => handleDelete(u)}
                              className="p-1.5 rounded-md text-destructive hover:bg-destructive/10 transition-colors">
                              <Trash2 className="w-4 h-4" />
                            </button>
                          </Tooltip>
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {approveTarget && (
        <ApproveModal
          user={approveTarget}
          onClose={() => setApproveTarget(null)}
          onDone={() => { setApproveTarget(null); load(); }}
        />
      )}
      {resetTarget && (
        <ResetPasswordModal
          user={resetTarget}
          onClose={() => setResetTarget(null)}
        />
      )}
      {showCreate && (
        <CreateUserModal
          onClose={() => setShowCreate(false)}
          onDone={() => { setShowCreate(false); load(); }}
        />
      )}
    </div>
  );
}

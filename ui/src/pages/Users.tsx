import { useState, useEffect, useCallback, useRef } from 'react';
import { useLocation } from 'react-router-dom';
import { Users as UsersIcon, Plus, Check, Ban, Trash2, UserCheck, Key, RotateCcw, Pencil } from 'lucide-react';
import { listUsers, createUser, approveUser, suspendUser, deleteUser, resetUserPassword, patchUser, fetchKeys, fetchModels, loadSession } from '../lib/api';
import type { UserRecord, APIKey, ModelCatalog } from '../types';
import { Badge } from '../components/Badge';
import { Modal } from '../components/Modal';
import { FormError } from '../components/FormError';
import { parseUserForm } from '../lib/formValidation';
import { useModalForm, errorMessage } from '../lib/useModalForm';
import { EmptyState } from '../components/EmptyState';
import { CustomSelect } from '../components/Select';
import { currentAppPath } from '../hooks/useDemoMode';
import { useTimezone } from '../hooks/useTimezone';
import { formatDateInZone } from '../lib/time';

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
    let active = true;
    fetchKeys().then(keys => { if (active) setActiveKeys(keys.filter(k => k.status === 'active')); }).catch(() => {});
    fetchModels().then((cat: ModelCatalog) => { if (active) setAvailableModels((cat.models ?? []).map(m => m.name)); }).catch(() => {});
    return () => { active = false; };
  }, []);

  function toggleModel(model: string) {
    setSelectedModels(prev =>
      prev.includes(model) ? prev.filter(m => m !== model) : [...prev, model]
    );
  }

  async function handleApprove() {
    // +e.target.value coerces a cleared field to 0 (documented elsewhere as
    // "unlimited") - that part is intended. A negative value is the actual
    // gap: it's truthy/non-zero so no existing check rejects it before it
    // reaches the create-key request. Validate on submit, consistent with
    // APIKeys.tsx's existing patch-validation checks.
    if (mode === 'create') {
      for (const [label, val] of [['Rate/hr', rateLimit], ['Daily', dailyLimit], ['Monthly', monthlyLimit]] as const) {
        if (!Number.isInteger(val) || val < 0) {
          setError(`${label} limit must be a non-negative integer`);
          return;
        }
      }
    }
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
      <Modal isOpen={true} onClose={onDone} title="User approved" maxWidth="sm">
        <div className="space-y-4">
          <p className="text-xs text-muted-foreground">API key for <strong>{user.username}</strong>:</p>
          <code className="block w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-xs font-mono text-primary break-all select-all">
            {createdKey}
          </code>
          <p className="text-xs text-amber-600 dark:text-amber-400">This key will not be shown again. Copy it now.</p>
          <button onClick={onDone} className="w-full py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 transition-colors shadow-sm">
            Done
          </button>
        </div>
      </Modal>
    );
  }

  return (
    <Modal isOpen={true} onClose={onClose} title={`Approve ${user.username}`}>
      <div className="space-y-4">
        <div className="flex gap-2">
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
              <CustomSelect
                value={existingKeyName}
                onChange={setExistingKeyName}
                options={[
                  { value: '', label: '-- select a key --' },
                  ...activeKeys.map(k => ({ value: k.name, label: k.name })),
                ]}
              />
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

        {error && <p className="text-xs text-destructive bg-destructive/10 px-3 py-2 rounded-lg">{error}</p>}

        <div className="flex gap-2 pt-4 border-t border-border">
          <button onClick={onClose} className="flex-1 py-2 border border-border text-sm font-medium rounded-lg text-muted-foreground hover:bg-secondary transition-colors">
            Cancel
          </button>
          <button onClick={handleApprove} disabled={saving || (mode === 'assign' && existingKeyName === '')}
            className="flex-1 py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 transition-colors disabled:opacity-50 shadow-sm">
            {saving ? 'Approving...' : 'Approve'}
          </button>
        </div>
      </div>
    </Modal>
  );
}

// ── Reset password Modal ──────────────────────────────────────────────────────

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
    <Modal isOpen={true} onClose={onClose} title="Reset password" maxWidth="sm">
      <div className="space-y-4">
        {loading && <p className="text-sm text-muted-foreground animate-pulse">Generating new password...</p>}
        {error && <p className="text-xs text-destructive bg-destructive/10 px-3 py-2 rounded-lg">{error}</p>}
        {password && (
          <div className="space-y-3">
            <p className="text-xs text-muted-foreground">New temporary password for <strong>{user.username}</strong> (shown once):</p>
            <code className="block w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-xs font-mono text-primary break-all select-all">
              {password}
            </code>
            <p className="text-xs text-amber-600 dark:text-amber-400">
              User will be prompted to change it on next login. All active sessions revoked.
            </p>
          </div>
        )}
        <button onClick={onClose} className="w-full py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 transition-colors shadow-sm">
          Done
        </button>
      </div>
    </Modal>
  );
}

// ── Create user Modal ─────────────────────────────────────────────────────────

interface CreatedCredentials {
  username: string;
  initial_password: string;
}

interface CreateUserModalProps {
  onClose: () => void;
  // Called with the one-time password. It lives in the parent, so it is shown
  // even when the operator closed this form while the request was running.
  // formStillOpen is false when the form that fired it was closed (and maybe a
  // new one opened) meanwhile, so the parent must not close the current form.
  onCreated: (created: CreatedCredentials, formStillOpen: boolean) => void;
}

function CreateUserModal({ onClose, onCreated }: CreateUserModalProps) {
  const [username, setUsername] = useState('');
  const [email, setEmail] = useState('');
  const [role, setRole] = useState<'user' | 'admin'>('user');
  const form = useModalForm();
  const actionRow = useRef<HTMLDivElement>(null);
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);

  async function handleCreate() {
    const { errors, invalid, values } = parseUserForm(username, email);
    if (errors.length > 0) { form.fail(errors, invalid); return; }
    const token = form.begin();
    if (token === null) return; // a submit is already in flight
    let res: Awaited<ReturnType<typeof createUser>>;
    try {
      res = await createUser({ username: values.username, email: values.email, role });
    } catch (err: unknown) {
      // If the form was closed meanwhile it is unmounted and this is a no-op.
      if (form.finish(token)) form.fail([errorMessage(err, 'Failed to create user')]);
      return;
    }
    // Outside the try above, so a failing callback can never be reported as a
    // failed create for a user that now exists.
    form.finish(token);
    onCreated({ username: res.username, initial_password: res.initial_password }, mounted.current);
  }

  return (
    <Modal isOpen={true} onClose={onClose} title="Create user">
      <div className="space-y-4">
        <div className="space-y-3">
          <div>
            <label htmlFor="create-user-username" className="block text-xs font-medium text-muted-foreground mb-1.5">Username <span aria-hidden="true" className="text-destructive">*</span></label>
            <input id="create-user-username" type="text" value={username} onChange={e => { setUsername(e.target.value); form.edit(); }} autoFocus aria-required="true"
              aria-invalid={form.state.invalid.includes('username') || undefined}
              aria-describedby={form.state.invalid.includes('username') ? 'create-user-error' : undefined}
              className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
          </div>
          <div>
            <label htmlFor="create-user-email" className="block text-xs font-medium text-muted-foreground mb-1.5">Email <span className="text-muted-foreground/60">(optional)</span></label>
            <input id="create-user-email" type="email" value={email} onChange={e => { setEmail(e.target.value); form.edit(); }}
              className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
          </div>
          <div>
            <label id="create-user-role-label" htmlFor="create-user-role" className="block text-xs font-medium text-muted-foreground mb-1.5">Role</label>
            <CustomSelect
              id="create-user-role"
              labelId="create-user-role-label"
              value={role}
              onChange={val => { setRole(val === 'admin' ? 'admin' : 'user'); form.edit(); }}
              options={[
                { value: 'user', label: 'User' },
                { value: 'admin', label: 'Admin' },
              ]}
            />
          </div>
        </div>

        <FormError
          id="create-user-error"
          errors={form.state.errors}
          attempt={form.state.attempt}
          focusFieldId={form.state.invalid.length > 0 ? 'create-user-username' : undefined /* the only required field */}
          actionRowRef={actionRow}
        />

        <div ref={actionRow} className="flex gap-2 pt-4 border-t border-border">
          <button onClick={onClose} className="flex-1 py-2 border border-border text-sm font-medium rounded-lg text-muted-foreground hover:bg-secondary transition-colors">
            Cancel
          </button>
          <button onClick={handleCreate}
            aria-disabled={form.state.busy || undefined}
            className="flex-1 py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 transition-colors shadow-sm aria-disabled:opacity-50 aria-disabled:cursor-not-allowed">
            {form.state.busy ? 'Creating...' : 'Create'}
          </button>
        </div>
      </div>
    </Modal>
  );
}

function UserCreatedModal({ created, remaining, onDone }: { created: CreatedCredentials; remaining: number; onDone: () => void }) {
  return (
    <Modal isOpen={true} onClose={onDone} title={remaining > 0 ? `User created (${remaining} more to show)` : 'User created'} maxWidth="sm">
      <div className="space-y-4">
        <p className="text-xs text-muted-foreground">Initial password for <strong>{created.username}</strong>:</p>
        <code className="block w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-xs font-mono text-primary break-all select-all">
          {created.initial_password}
        </code>
        <p className="text-xs text-amber-600 dark:text-amber-400">Share with the user. They will be prompted to change it on first login.</p>
        <button onClick={onDone} className="w-full py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 transition-colors shadow-sm">
          Done
        </button>
      </div>
    </Modal>
  );
}

// ── Edit user Modal ───────────────────────────────────────────────────────────
// PATCH /admin/v1/users/{id} (handlePatchUser) has had a CLI command (marbor
// users patch) for a while but no UI surface until now.

interface EditUserModalProps {
  user: UserRecord;
  onClose: () => void;
  onDone: () => void;
}

function EditUserModal({ user, onClose, onDone }: EditUserModalProps) {
  const [email, setEmail] = useState(user.email);
  const [role, setRole] = useState<'user' | 'admin'>(user.role);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function handleSave() {
    setSaving(true);
    setError(null);
    try {
      await patchUser(user.id, { email, role });
      onDone();
    } catch (err: any) {
      setError(err.message || 'Failed to update user');
    } finally {
      setSaving(false);
    }
  }

  return (
    <Modal isOpen={true} onClose={onClose} title="Edit user">
      <div className="space-y-4">
        <div className="space-y-3">
          <div>
            <label className="block text-xs font-medium text-muted-foreground mb-1.5">Username</label>
            <input type="text" value={user.username} disabled
              className="w-full px-3 py-2 bg-secondary/30 border border-border rounded-lg text-sm text-muted-foreground cursor-not-allowed" />
          </div>
          <div>
            <label className="block text-xs font-medium text-muted-foreground mb-1.5">Email</label>
            <input type="email" value={email} onChange={e => setEmail(e.target.value)} autoFocus
              className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
          </div>
          <div>
            <label className="block text-xs font-medium text-muted-foreground mb-1.5">Role</label>
            <CustomSelect
              value={role}
              onChange={val => setRole(val as any)}
              options={[
                { value: 'user', label: 'User' },
                { value: 'admin', label: 'Admin' },
              ]}
            />
          </div>
        </div>

        {error && <p className="text-xs text-destructive bg-destructive/10 px-3 py-2 rounded-lg">{error}</p>}

        <div className="flex gap-2 pt-4 border-t border-border">
          <button onClick={onClose} className="flex-1 py-2 border border-border text-sm font-medium rounded-lg text-muted-foreground hover:bg-secondary transition-colors">
            Cancel
          </button>
          <button onClick={handleSave} disabled={saving}
            className="flex-1 py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 transition-colors disabled:opacity-50 shadow-sm">
            {saving ? 'Saving...' : 'Save'}
          </button>
        </div>
      </div>
    </Modal>
  );
}

// ── Main Users Page ───────────────────────────────────────────────────────────

export function Users() {
  const tz = useTimezone();
  const currentUsername = loadSession()?.username ?? '';
  const location = useLocation();
  const [users, setUsers] = useState<UserRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [approveTarget, setApproveTarget] = useState<UserRecord | null>(null);
  const [editTarget, setEditTarget] = useState<UserRecord | null>(null);
  const [resetTarget, setResetTarget] = useState<UserRecord | null>(null);
  const [resetConfirmTarget, setResetConfirmTarget] = useState<UserRecord | null>(null);
  const [suspendTarget, setSuspendTarget] = useState<UserRecord | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<UserRecord | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  // One-time passwords waiting to be shown, oldest first. A queue, so a second
  // create finishing late never overwrites a password the operator has not seen.
  const [createdUsers, setCreatedUsers] = useState<CreatedCredentials[]>([]);

  const load = useCallback(async (active = true) => {
    if (currentAppPath() !== '/users') return;
    try {
      const data = await listUsers();
      if (!active || currentAppPath() !== '/users') return;
      setUsers(data);
      setError(null);
    } catch (err: any) {
      if (!active || currentAppPath() !== '/users') return;
      setError(err.message || 'Failed to load users');
    } finally {
      if (active && currentAppPath() === '/users') {
        setLoading(false);
      }
    }
  }, [location.pathname]);

  useEffect(() => {
    if (currentAppPath() !== '/users') return;
    let active = true;
    load(active);
    return () => {
      active = false;
    };
  }, [load, location.pathname]);

  async function handleSuspend(u: UserRecord): Promise<boolean> {
    try {
      await suspendUser(u.id);
      load();
      return true;
    } catch (err: any) {
      setActionError(err.message || 'Failed to suspend user');
      return false;
    }
  }

  async function handleDelete(u: UserRecord): Promise<boolean> {
    try {
      await deleteUser(u.id);
      load();
      return true;
    } catch (err: any) {
      setActionError(err.message || 'Failed to delete user');
      return false;
    }
  }

  const pendingCount = users.filter(u => u.status === 'pending').length;

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div className="flex items-center gap-3 flex-wrap">
          <UsersIcon className="w-6 h-6 text-primary animate-pulse" />
          <div>
            <h1 className="text-xl font-bold tracking-tight text-foreground">Users</h1>
            <p className="text-xs text-muted-foreground">Manage dashboard access and API key assignments</p>
          </div>
          {pendingCount > 0 && (
            <span className="px-2 py-0.5 text-xs font-bold bg-amber-500/20 text-amber-600 dark:text-amber-400 rounded-md whitespace-nowrap">
              {pendingCount} pending
            </span>
          )}
        </div>
        <button
          onClick={() => setShowCreate(true)}
          className="flex items-center gap-2 px-3 py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 transition-colors self-start sm:self-auto"
        >
          <Plus className="w-4 h-4" />
          Create user
        </button>
      </div>

      {error && (
        <div className="bg-destructive/10 border border-destructive/20 rounded-xl p-4 text-sm text-destructive">{error}</div>
      )}

      <div className="bg-card border border-border rounded-xl shadow-sm overflow-hidden">
        {loading ? (
          <div className="p-8 text-center text-sm text-muted-foreground">Loading...</div>
        ) : users.length === 0 ? (
          <div className="px-8 pb-8"><EmptyState compact icon={UsersIcon} title="No users yet" copy="Operator accounts you create will be listed here." /></div>
        ) : (
          <div className="hidden md:block overflow-x-auto scroll-region" tabIndex={0} role="region" aria-label="Users table: scroll horizontally for more columns">
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
                    <tr key={u.id} className="hover:bg-secondary/30 transition-colors">
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
                        {formatDateInZone(u.created_at, tz)}
                      </td>
                      <td className="px-4 py-3">
                        <div className="flex items-center justify-end gap-1">
                          {u.status === 'pending' && (
                            <button onClick={() => setApproveTarget(u)}
                              title="Approve user"
                              className="p-1.5 rounded-md text-green-600 dark:text-green-400 hover:bg-green-500/10 transition-colors">
                              <Check className="w-4 h-4" />
                            </button>
                          )}
                          {u.status === 'active' && (
                            <button onClick={() => { setActionError(null); setSuspendTarget(u); }}
                              title="Suspend user"
                              className="p-1.5 rounded-md text-amber-600 dark:text-amber-400 hover:bg-amber-500/10 transition-colors">
                              <Ban className="w-4 h-4" />
                            </button>
                          )}
                          {u.status === 'suspended' && (
                            <button onClick={() => setApproveTarget(u)}
                              title="Reactivate user"
                              className="p-1.5 rounded-md text-primary hover:bg-primary/10 transition-colors">
                              <UserCheck className="w-4 h-4" />
                            </button>
                          )}
                          <button onClick={() => { setActionError(null); setEditTarget(u); }}
                            title="Edit email/role"
                            className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors">
                            <Pencil className="w-4 h-4" />
                          </button>
                          <button
                            onClick={() => { setActionError(null); setResetConfirmTarget(u); }}
                            disabled={u.username === currentUsername}
                            title={u.username === currentUsername ? 'Use Settings to change your own password' : 'Reset password'}
                            className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors disabled:opacity-30 disabled:cursor-not-allowed disabled:hover:bg-transparent disabled:hover:text-muted-foreground"
                          >
                            <RotateCcw className="w-4 h-4" />
                          </button>
                          <button onClick={() => { setActionError(null); setDeleteTarget(u); }}
                            title="Delete user"
                            className="p-1.5 rounded-md text-destructive hover:bg-destructive/10 transition-colors">
                            <Trash2 className="w-4 h-4" />
                          </button>
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}

        {!loading && users.length > 0 && (
          <div className="md:hidden space-y-3 p-3">
            {users.map(u => {
              const sb = STATUS_BADGE[u.status] ?? { variant: 'muted' as const, label: u.status };
              const rb = ROLE_BADGE[u.role] ?? { variant: 'muted' as const, label: u.role };
              return (
                <div key={u.id} className="bg-card/50 backdrop-blur-sm border border-border/60 rounded-xl p-4 space-y-3">
                  <div className="flex items-start justify-between gap-2">
                    <div>
                      <p className="text-sm font-medium text-foreground">{u.username}</p>
                      <p className="text-sm text-foreground/80">{u.email || '-'}</p>
                    </div>
                    <div className="flex items-center gap-1.5 flex-wrap justify-end">
                      <Badge variant={rb.variant}>{rb.label}</Badge>
                      <Badge variant={sb.variant}>{sb.label}</Badge>
                    </div>
                  </div>

                  <div className="grid grid-cols-2 gap-x-3 gap-y-2">
                    <div>
                      <p className="text-[10px] uppercase tracking-wider text-muted-foreground">API Key</p>
                      <p className="text-sm text-foreground font-mono truncate">{u.api_key_name || '-'}</p>
                    </div>
                    <div>
                      <p className="text-[10px] uppercase tracking-wider text-muted-foreground">Created</p>
                      <p className="text-sm text-foreground">{formatDateInZone(u.created_at, tz)}</p>
                    </div>
                  </div>

                  <div className="flex items-center justify-end gap-1 pt-2 border-t border-border/60">
                    {u.status === 'pending' && (
                      <button onClick={() => setApproveTarget(u)}
                        title="Approve user"
                        className="p-1.5 rounded-md text-green-600 dark:text-green-400 hover:bg-green-500/10 transition-colors">
                        <Check className="w-4 h-4" />
                      </button>
                    )}
                    {u.status === 'active' && (
                      <button onClick={() => { setActionError(null); setSuspendTarget(u); }}
                        title="Suspend user"
                        className="p-1.5 rounded-md text-amber-600 dark:text-amber-400 hover:bg-amber-500/10 transition-colors">
                        <Ban className="w-4 h-4" />
                      </button>
                    )}
                    {u.status === 'suspended' && (
                      <button onClick={() => setApproveTarget(u)}
                        title="Reactivate user"
                        className="p-1.5 rounded-md text-primary hover:bg-primary/10 transition-colors">
                        <UserCheck className="w-4 h-4" />
                      </button>
                    )}
                    <button onClick={() => { setActionError(null); setEditTarget(u); }}
                      title="Edit email/role"
                      className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors">
                      <Pencil className="w-4 h-4" />
                    </button>
                    <button
                      onClick={() => { setActionError(null); setResetConfirmTarget(u); }}
                      disabled={u.username === currentUsername}
                      title={u.username === currentUsername ? 'Use Settings to change your own password' : 'Reset password'}
                      className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors disabled:opacity-30 disabled:cursor-not-allowed disabled:hover:bg-transparent disabled:hover:text-muted-foreground"
                    >
                      <RotateCcw className="w-4 h-4" />
                    </button>
                    <button onClick={() => { setActionError(null); setDeleteTarget(u); }}
                      title="Delete user"
                      className="p-1.5 rounded-md text-destructive hover:bg-destructive/10 transition-colors">
                      <Trash2 className="w-4 h-4" />
                    </button>
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </div>

      {editTarget && (
        <EditUserModal
          user={editTarget}
          onClose={() => setEditTarget(null)}
          onDone={() => { setEditTarget(null); load(); }}
        />
      )}
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
          onCreated={(c, formStillOpen) => {
            // Only close the form that fired this; a late result must not close a reopened one.
            if (formStillOpen) setShowCreate(false);
            setCreatedUsers((q) => [...q, c]);
            load();
          }}
        />
      )}

      <Modal
        isOpen={suspendTarget !== null}
        onClose={() => setSuspendTarget(null)}
        title="Suspend user"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Are you sure you want to suspend <span className="text-foreground font-semibold">{suspendTarget?.username}</span>?
          </p>
          <p className="text-xs text-muted-foreground">
            They will immediately lose dashboard access until reactivated.
          </p>
          {actionError && (
            <p className="text-sm text-destructive">{actionError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setSuspendTarget(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={async () => {
                if (!suspendTarget) return;
                const ok = await handleSuspend(suspendTarget);
                if (ok) setSuspendTarget(null);
              }}
              className="px-4 py-2 bg-amber-600 hover:bg-amber-600/90 text-white font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Suspend user
            </button>
          </div>
        </div>
      </Modal>

      <Modal
        isOpen={deleteTarget !== null}
        onClose={() => setDeleteTarget(null)}
        title="Delete user"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Are you sure you want to delete <span className="text-foreground font-semibold">{deleteTarget?.username}</span>?
          </p>
          <p className="text-xs text-muted-foreground">
            They will be soft-deleted and removed from the active list. This action cannot be undone.
          </p>
          {actionError && (
            <p className="text-sm text-destructive">{actionError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setDeleteTarget(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={async () => {
                if (!deleteTarget) return;
                const ok = await handleDelete(deleteTarget);
                if (ok) setDeleteTarget(null);
              }}
              className="px-4 py-2 bg-destructive hover:bg-destructive/90 text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Delete user
            </button>
          </div>
        </div>
      </Modal>

      <Modal
        isOpen={resetConfirmTarget !== null}
        onClose={() => setResetConfirmTarget(null)}
        title="Reset password"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Are you sure you want to reset the password for <span className="text-foreground font-semibold">{resetConfirmTarget?.username}</span>?
          </p>
          <p className="text-xs text-muted-foreground">
            A new temporary password will be generated and all of their active sessions will be revoked immediately.
          </p>
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setResetConfirmTarget(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={() => {
                const target = resetConfirmTarget;
                setResetConfirmTarget(null);
                if (target) setResetTarget(target);
              }}
              className="px-4 py-2 bg-destructive hover:bg-destructive/90 text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Reset password
            </button>
          </div>
        </div>
      </Modal>

      {/* Last in the page so the one-time password appears on top of any other
          modal the operator opened while the create request ran. Closing those
          would discard their input, and hiding the password would lose it. */}
      {/* A create form that fired late can mount beside a reopened one; the
          credentials win because they are rendered last and cannot be recovered. */}
      {createdUsers.length > 0 && (
        <UserCreatedModal
          created={createdUsers[0]}
          remaining={createdUsers.length - 1}
          onDone={() => setCreatedUsers((q) => q.slice(1))}
        />
      )}
    </div>
  );
}

import { useEffect, useRef, useState } from 'react';
import { Plus, Trash2, Tags } from 'lucide-react';
import { Badge } from './Badge';
import { Modal } from './Modal';
import { CustomCombobox } from './Select';
import { fetchModelAliases, setModelAlias, deleteModelAlias } from '../lib/api';
import { getMockModelAliases, setMockModelAlias, deleteMockModelAlias } from '../lib/mockData';
import { ModelAlias } from '../types';

interface ModelAliasesSectionProps {
  demoMode: boolean;
  // Fleet-wide model names, used only as suggestions for the target picker.
  knownModelNames: string[];
}

// Target availability comes straight from the Admin API's live inventory
// check. "not checked" (inventory could not be read) is kept distinct from a
// confirmed "not on fleet" so the badge never overstates what is known.
function TargetBadge({ row }: { row: ModelAlias }) {
  if (!row.inventory_checked) return <Badge variant="muted" size="sm">not checked</Badge>;
  if (!row.target_available) return <Badge variant="warning" size="sm">target not on fleet</Badge>;
  if (row.target_status === 'loaded') return <Badge variant="success" size="sm">loaded</Badge>;
  return <Badge variant="primary" size="sm">available</Badge>;
}

export function ModelAliasesSection({ demoMode, knownModelNames }: ModelAliasesSectionProps) {
  const [aliases, setAliases] = useState<ModelAlias[]>([]);
  const [loading, setLoading] = useState(!demoMode);
  const [aliasToOverwrite, setAliasToOverwrite] = useState<{ existing: ModelAlias; target: string } | null>(null);
  const [saving, setSaving] = useState(false);
  const [newAlias, setNewAlias] = useState('');
  const [newTarget, setNewTarget] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [aliasToRemove, setAliasToRemove] = useState<ModelAlias | null>(null);
  const mountedRef = useRef(true);
  // Bumped on every reload and on demo-mode change so a slow response from an
  // earlier request can never overwrite newer rows.
  const requestRef = useRef(0);

  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  const reload = async () => {
    const id = ++requestRef.current;
    if (demoMode) {
      setAliases(getMockModelAliases());
      setLoading(false);
      return;
    }
    try {
      const rows = await fetchModelAliases();
      if (mountedRef.current && id === requestRef.current) setAliases(rows);
    } catch (err: any) {
      if (mountedRef.current && id === requestRef.current) setError(err.message);
    } finally {
      if (mountedRef.current && id === requestRef.current) setLoading(false);
    }
  };

  useEffect(() => {
    // Drop rows from the other mode right away so demo aliases never show as
    // live ones (or the reverse) while the new mode loads.
    setAliases([]);
    setError(null);
    setLoading(!demoMode);
    reload();
    return () => { requestRef.current++; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [demoMode]);

  const saveAlias = async (alias: string, target: string) => {
    setSaving(true);
    try {
      const row = demoMode ? setMockModelAlias(alias, target) : await setModelAlias(alias, target);
      if (!mountedRef.current) return;
      if (row.shadows_model) {
        setNotice(`"${row.alias}" is also a model on the fleet. The alias wins, so requests for it are now served by "${row.target}".`);
      }
      setNewAlias('');
      setNewTarget('');
      await reload();
    } catch (err: any) {
      if (mountedRef.current) setError(err.message);
    } finally {
      if (mountedRef.current) {
        setSaving(false);
        setAliasToOverwrite(null);
      }
    }
  };

  const handleAdd = async () => {
    const alias = newAlias.trim();
    const target = newTarget.trim();
    setError(null);
    setNotice(null);
    if (!alias || !target) {
      setError('Enter both an alias name and a target model.');
      return;
    }
    // Saving over an existing alias re-points live traffic, so confirm first.
    const existing = aliases.find(a => a.alias === alias);
    if (existing && existing.target !== target) {
      setAliasToOverwrite({ existing, target });
      return;
    }
    await saveAlias(alias, target);
  };

  const handleRemove = async () => {
    if (!aliasToRemove) return;
    setSaving(true);
    setError(null);
    try {
      if (demoMode) deleteMockModelAlias(aliasToRemove.alias);
      else await deleteModelAlias(aliasToRemove.alias);
      if (!mountedRef.current) return;
      await reload();
    } catch (err: any) {
      if (mountedRef.current) setError(err.message);
    } finally {
      if (mountedRef.current) {
        setSaving(false);
        setAliasToRemove(null);
      }
    }
  };

  return (
    <div className="bg-card border border-border shadow-sm rounded-xl p-6">
      <div className="flex items-center gap-3 mb-5">
        <div className="p-2 bg-indigo-500/10 rounded-lg">
          <Tags className="w-5 h-5 text-indigo-600 dark:text-indigo-400" />
        </div>
        <div className="min-w-0">
          <h3 className="text-sm font-semibold text-foreground">Model aliases</h3>
          <p className="text-xs font-medium text-muted-foreground">Serve a real fleet model under the name clients already use (e.g. gpt-4) - takes effect immediately, one hop only</p>
        </div>
      </div>

      {loading ? (
        <div className="space-y-2 mb-4">
          {[1, 2].map(i => (
            <div key={i} className="h-14 bg-secondary/30 rounded-lg animate-pulse" />
          ))}
        </div>
      ) : (
        <div className="space-y-2 mb-4">
          {aliases.length === 0 ? (
            <p className="text-sm text-muted-foreground py-2">No model aliases declared</p>
          ) : (
            aliases.map(row => (
              <div key={row.alias} className="flex items-center justify-between gap-3 p-2.5 rounded-lg border border-border bg-secondary/30 min-w-0">
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium text-foreground truncate" title={`${row.alias} -> ${row.target}`}>
                    {row.alias} <span className="text-muted-foreground">-&gt;</span> {row.target}
                  </p>
                  <div className="flex flex-wrap items-center gap-1.5 mt-1">
                    <TargetBadge row={row} />
                    {row.shadows_model && (
                      <span title={`"${row.alias}" is also a model on the fleet; the alias wins, so requests for it go to ${row.target}`}>
                        <Badge variant="warning" size="sm">shadows a fleet model</Badge>
                      </span>
                    )}
                  </div>
                </div>
                <button
                  onClick={() => setAliasToRemove(row)}
                  disabled={saving}
                  aria-label={`Remove alias ${row.alias}`}
                  className="p-2 text-muted-foreground hover:text-destructive rounded-md hover:bg-secondary transition-colors disabled:opacity-50 shrink-0 min-h-[40px] min-w-[40px] flex items-center justify-center"
                >
                  <Trash2 className="w-3.5 h-3.5" />
                </button>
              </div>
            ))
          )}
        </div>
      )}

      <form
        className="flex flex-col sm:flex-row gap-2"
        onSubmit={(e) => { e.preventDefault(); if (!saving) void handleAdd(); }}
      >
        <input
          type="text"
          value={newAlias}
          onChange={(e) => { setNewAlias(e.target.value); setError(null); }}
          placeholder="gpt-4"
          aria-label="Alias name"
          className="w-full sm:flex-1 px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
        />
        <CustomCombobox
          value={newTarget}
          onChange={(v) => { setNewTarget(v); setError(null); }}
          options={knownModelNames}
          placeholder="llama3.2:8b"
          className="sm:flex-1"
        />
        <button
          type="submit"
          disabled={saving}
          aria-label="Add model alias"
          className="flex items-center justify-center gap-1.5 px-3 py-2 min-h-[40px] min-w-[40px] bg-primary hover:bg-primary/90 text-primary-foreground text-sm font-medium rounded-lg transition-colors disabled:opacity-50"
        >
          <Plus className="w-4 h-4" />
        </button>
      </form>
      {error && <p role="alert" className="text-sm text-destructive mt-2">{error}</p>}
      {notice && <p role="status" className="text-sm text-warning mt-2">{notice}</p>}

      <Modal
        isOpen={aliasToOverwrite !== null}
        onClose={() => setAliasToOverwrite(null)}
        title="Re-point model alias"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground break-words">
            <span className="text-foreground font-medium">{aliasToOverwrite?.existing.alias}</span> currently
            serves <span className="text-foreground font-medium">{aliasToOverwrite?.existing.target}</span>.
            Saving re-points it to <span className="text-foreground font-medium">{aliasToOverwrite?.target}</span>;
            live requests for this name switch immediately. Reversible by saving the old target again.
          </p>
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              type="button"
              onClick={() => setAliasToOverwrite(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              type="button"
              onClick={() => aliasToOverwrite && saveAlias(aliasToOverwrite.existing.alias, aliasToOverwrite.target)}
              disabled={saving}
              className="px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm disabled:opacity-50"
            >
              Re-point alias
            </button>
          </div>
        </div>
      </Modal>

      <Modal
        isOpen={aliasToRemove !== null}
        onClose={() => setAliasToRemove(null)}
        title="Remove model alias"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground break-words">
            Clients requesting <span className="text-foreground font-medium">{aliasToRemove?.alias}</span> will
            no longer reach <span className="text-foreground font-medium">{aliasToRemove?.target}</span>; their
            requests will fail until they switch names or you re-add the alias. Reversible by re-adding it.
          </p>
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setAliasToRemove(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleRemove}
              disabled={saving}
              className="px-4 py-2 bg-destructive hover:bg-destructive/90 text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm disabled:opacity-50"
            >
              Remove alias
            </button>
          </div>
        </div>
      </Modal>
    </div>
  );
}

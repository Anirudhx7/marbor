import { useEffect, useRef, useState } from 'react';
import { Plus, Trash2, Cloud } from 'lucide-react';
import { fetchCloudOnlySettings, addCloudOnlyModel, removeCloudOnlyModel } from '../lib/api';
import type { CloudOnlySettings } from '../lib/api';
import { getMockCloudOnly, setMockCloudOnly } from '../lib/mockData';
import { addEntry, removeEntry, providerStatus, shouldWarnNoMatch, writeBlocked } from '../lib/cloudOnlyView';

interface CloudOnlyModelsSectionProps {
  demoMode: boolean;
  cloudProviders: Array<{ enabled?: boolean }>;
  // True once the cloud provider list has loaded; false while it loads or
  // after a failed load, so no warning is raised from missing data.
  providersReady: boolean;
  // LiteLLM is a usable route only with a URL; null until the settings load.
  liteLLMUsable: boolean | null;
  // Model names the nodes serve right now, used only for a non-blocking notice.
  knownModelNames: string[];
}

interface Ops {
  read: () => Promise<CloudOnlySettings>;
  add: (entry: string) => Promise<string[]>;
  remove: (entry: string) => Promise<string[]>;
}

// Demo mode (the dashboard's demo toggle) never calls the live API.
const demoOps: Ops = {
  read: async () => ({ models: getMockCloudOnly(), unreadable: false }),
  add: async (entry) => {
    const next = addEntry(getMockCloudOnly(), entry);
    if ('error' in next) throw new Error(next.error);
    return setMockCloudOnly(next.list);
  },
  remove: async (entry) => setMockCloudOnly(removeEntry(getMockCloudOnly(), entry)),
};
const liveOps: Ops = { read: fetchCloudOnlySettings, add: addCloudOnlyModel, remove: removeCloudOnlyModel };

export function CloudOnlyModelsSection({ demoMode, cloudProviders, providersReady, liteLLMUsable, knownModelNames }: CloudOnlyModelsSectionProps) {
  const ops = demoMode ? demoOps : liveOps;
  const [models, setModels] = useState<string[]>([]);
  const [unreadable, setUnreadable] = useState(false);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  // The entry just removed, offered back until the next action.
  const [undoEntry, setUndoEntry] = useState<string | null>(null);
  const [ack, setAck] = useState(false);
  const [newEntry, setNewEntry] = useState('');
  const mountedRef = useRef(true);
  // Synchronous guard so two quick submits cannot both start a save before the
  // saving state has re-rendered.
  const busyRef = useRef(false);
  // Bumped on every load and on a mode change so a slow response from an
  // earlier request can never overwrite newer state.
  const requestRef = useRef(0);

  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  const load = async (showSkeleton: boolean) => {
    const id = ++requestRef.current;
    if (showSkeleton) setLoading(true);
    try {
      const data = await ops.read();
      if (!mountedRef.current || id !== requestRef.current) return;
      setModels(data.models);
      setUnreadable(data.unreadable);
      if (!data.unreadable) setAck(false);
      setLoadError(null);
    } catch (err: any) {
      // A failed refresh after a save keeps the list the save returned; only a
      // load that has nothing to show becomes the read-failure card.
      if (mountedRef.current && id === requestRef.current && showSkeleton) setLoadError(err.message || 'Could not read the cloud-only models list');
    } finally {
      if (mountedRef.current && id === requestRef.current) setLoading(false);
    }
  };

  useEffect(() => {
    // Drop the other mode's list right away so demo entries never show as live
    // ones (or the reverse) while the new mode loads.
    setModels([]);
    setUnreadable(false);
    setError(null);
    setNotice(null);
    setUndoEntry(null);
    setSaving(false);
    busyRef.current = false;
    void load(true);
    return () => { requestRef.current++; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [demoMode]);

  const blocked = writeBlocked({ unreadable, acknowledged: ack, saving });

  const run = async (action: () => Promise<string[]>, after: (saved: string[]) => void) => {
    if (busyRef.current) return;
    busyRef.current = true;
    const mode = requestRef.current;
    setSaving(true);
    setError(null);
    setNotice(null);
    setUndoEntry(null);
    try {
      const saved = await action();
      // Drop the result if the mode changed while saving.
      if (!mountedRef.current || mode !== requestRef.current) return;
      setModels(saved);
      setAck(false);
      after(saved);
      // Re-read so the unreadable banner reflects what the server now says.
      await load(false);
    } catch (err: any) {
      // The server's own message (validation, unreadable list, failed write).
      if (mountedRef.current) setError(err.message || 'Could not save the cloud-only models list');
    } finally {
      busyRef.current = false;
      if (mountedRef.current) setSaving(false);
    }
  };

  const handleAdd = async () => {
    const entry = newEntry.trim();
    if (blocked) return;
    await run(() => ops.add(entry), () => {
      setNewEntry('');
      if (shouldWarnNoMatch(entry, knownModelNames)) {
        setNotice(`"${entry}" matches no model your nodes currently serve; it is still saved.`);
      }
    });
  };

  const handleRemove = async (entry: string) => {
    await run(() => ops.remove(entry), () => setUndoEntry(entry));
  };

  const handleUndo = async () => {
    if (!undoEntry || blocked) return;
    const entry = undoEntry;
    await run(() => ops.add(entry), () => undefined);
  };

  const status = providerStatus({ providersReady, liteLLMUsable, providers: cloudProviders });
  const summary = loadError ? 'could not be read' : loading ? '' : unreadable ? 'stored list unreadable' : models.length === 0 ? 'none listed' : `${models.length} listed`;

  return (
    <div className="bg-card border border-border shadow-sm rounded-xl p-6">
      <div className="flex items-center gap-3 mb-5">
        <div className="p-2 bg-sky-500/10 rounded-lg">
          <Cloud className="w-5 h-5 text-sky-600 dark:text-sky-400" />
        </div>
        <div className="min-w-0">
          <h3 className="text-sm font-semibold text-foreground">Cloud-only models</h3>
          <p className="text-xs font-medium text-muted-foreground">
            Models that never run on your nodes - sent to your cloud provider{summary ? ` (${summary})` : ''}
          </p>
        </div>
      </div>

      {loading ? (
        <div className="space-y-2 mb-4" role="status" aria-busy="true" aria-label="Loading cloud-only models">
          {[1, 2].map(i => <div key={i} className="h-10 bg-secondary/30 rounded-lg animate-pulse" />)}
        </div>
      ) : loadError ? (
        <div role="alert" className="mb-4 p-3 rounded-lg border border-destructive/30 bg-destructive/10 text-sm text-destructive">
          <p className="break-words">{loadError}</p>
          <button
            type="button"
            onClick={() => void load(true)}
            className="mt-2 px-3 py-2 min-h-[40px] text-sm font-medium border border-border rounded-lg text-foreground hover:bg-secondary transition-colors"
          >
            Retry
          </button>
        </div>
      ) : (
        <>
          {unreadable && (
            <div role="alert" className="mb-4 p-3 rounded-lg border border-destructive/30 bg-destructive/10 text-sm text-destructive">
              <p className="break-words">
                The saved list could not be read. Every request that names a model is refused until you save a list.
                Saving replaces the stored list with exactly the entries shown here, so re-add every model that must stay cloud-only.
              </p>
              <label className="mt-2 flex items-center gap-2 min-h-[40px] text-foreground cursor-pointer">
                <input
                  type="checkbox"
                  checked={ack}
                  onChange={(e) => setAck(e.target.checked)}
                  className="w-4 h-4"
                />
                <span>Replace the stored list</span>
              </label>
            </div>
          )}

          {models.length > 0 && status === 'none' && (
            <p role="status" className="mb-3 text-sm text-warning break-words">
              No cloud provider is usable (none enabled, and LiteLLM is off or has no URL). Requests for these models are refused with a 503 until you enable one.
            </p>
          )}

          <div className="space-y-2 mb-4">
            {models.length === 0 ? (
              <p className="text-sm text-muted-foreground py-2">
                {unreadable ? 'No entries yet. Add every model that must stay cloud-only.' : 'No models are cloud-only. Every model is routed to your local nodes as usual.'}
              </p>
            ) : (
              models.map(entry => (
                <div key={entry} className="flex items-center justify-between gap-3 p-2.5 rounded-lg border border-border bg-secondary/30 min-w-0">
                  <p className="text-sm font-medium text-foreground break-all min-w-0 flex-1">{entry}</p>
                  <button
                    type="button"
                    onClick={() => void handleRemove(entry)}
                    disabled={saving}
                    aria-label={`Remove ${entry} from the cloud-only list`}
                    className="p-2 text-muted-foreground hover:text-destructive rounded-md hover:bg-secondary transition-colors disabled:opacity-50 shrink-0 min-h-[40px] min-w-[40px] flex items-center justify-center"
                  >
                    <Trash2 className="w-3.5 h-3.5" />
                  </button>
                </div>
              ))
            )}
          </div>

          <form
            className="flex flex-col sm:flex-row gap-2"
            onSubmit={(e) => { e.preventDefault(); void handleAdd(); }}
          >
            <input
              type="text"
              value={newEntry}
              onChange={(e) => { setNewEntry(e.target.value); setError(null); }}
              placeholder="llama3.3:70b or qwen*"
              aria-label="Model name to keep on cloud"
              className="w-full sm:flex-1 px-3 py-2 min-h-[40px] bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
            />
            <button
              type="submit"
              disabled={blocked || newEntry.trim() === ''}
              aria-label="Add model to the cloud-only list"
              className="flex items-center justify-center gap-1.5 px-3 py-2 min-h-[40px] min-w-[40px] bg-primary hover:bg-primary/90 text-primary-foreground text-sm font-medium rounded-lg transition-colors disabled:opacity-50"
            >
              <Plus className="w-4 h-4" />
            </button>
          </form>
          <p className="text-xs text-muted-foreground mt-2">Takes effect on the next request. Use a name ending in * to cover a family, as in qwen*.</p>
        </>
      )}

      {error && <p role="alert" className="text-sm text-destructive mt-2 break-words">{error}</p>}
      {notice && <p role="status" className="text-sm text-warning mt-2 break-words">{notice}</p>}
      {undoEntry && (
        <p role="status" className="text-sm text-muted-foreground mt-2 break-all">
          Removed {undoEntry}.{' '}
          <button
            type="button"
            onClick={() => void handleUndo()}
            disabled={blocked}
            className="underline text-foreground hover:text-primary disabled:opacity-50 disabled:no-underline min-h-[40px]"
          >
            Undo
          </button>
          {unreadable && !ack && <span> (tick &quot;Replace the stored list&quot; first)</span>}
        </p>
      )}

      <details className="mt-4 text-xs text-muted-foreground">
        <summary className="cursor-pointer min-h-[40px] flex items-center">How names match</summary>
        <ul className="list-disc pl-5 space-y-1 pb-1">
          <li>An exact name ignores case and treats llama3 and llama3:latest as the same, but llama3 does not cover llama3:8b.</li>
          <li>A name ending in * is a prefix: llama3* covers llama3:8b and anything else that starts with llama3.</li>
          <li>The name is compared as your clients send it. Spellings such as library/llama3 are not matched.</li>
          <li>Requests for a listed model go to the first usable cloud provider; there is no per-model choice. With no provider, no budget left, or on an endpoint cloud cannot serve, they are refused with a 503 and never run locally.</li>
          <li>Keys marked local-only receive a 503 for a listed model.</li>
        </ul>
      </details>
    </div>
  );
}

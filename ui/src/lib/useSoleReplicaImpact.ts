import { useCallback, useEffect, useRef, useState } from 'react';
import { getSoleReplicaImpact } from './api';
import { mockSoleReplicaImpact } from './mockData';
import { resolveImpactState, soleCheckErrorMessage, type SoleImpactState } from './soleReplica';

type Stored = { node: string | null } & SoleImpactState;

// A check that has not answered by then stops waiting, so the confirm button is
// never held by a hung request (the action is still checked by the server).
const CHECK_TIMEOUT_MS = 10_000;

// useSoleReplicaImpact fetches the "what stops being served" check for the node
// a Drain or Restart dialog is open on (nodeName null = dialog closed). reload
// refetches the same node, used after the server rejects a confirm because the
// answer changed. Demo mode reads the sample data and never calls the API.
export function useSoleReplicaImpact(nodeName: string | null, demoMode: boolean) {
  const [stored, setStored] = useState<Stored>({ node: null, status: 'idle' });
  const seq = useRef(0);
  const abort = useRef<AbortController | null>(null);

  const load = useCallback(async (name: string) => {
    const id = ++seq.current;
    abort.current?.abort();
    const controller = new AbortController();
    abort.current = controller;
    const timer = setTimeout(() => controller.abort(), CHECK_TIMEOUT_MS);
    setStored({ node: name, status: 'loading' });
    try {
      const impact = demoMode
        ? (mockSoleReplicaImpact[name] ?? { models: [], cold_load_only: [], unknown: [] })
        : await getSoleReplicaImpact(name, controller.signal);
      if (id === seq.current) setStored({ node: name, status: 'ready', impact });
    } catch (e) {
      if (id === seq.current) setStored({ node: name, status: 'error', message: soleCheckErrorMessage(e) });
    } finally {
      clearTimeout(timer);
    }
  }, [demoMode]);

  useEffect(() => {
    if (!nodeName) {
      seq.current++;
      abort.current?.abort();
      setStored({ node: null, status: 'idle' });
      return;
    }
    void load(nodeName);
    return () => { seq.current++; abort.current?.abort(); };
  }, [nodeName, load]);

  const reload = useCallback(() => { if (nodeName) void load(nodeName); }, [nodeName, load]);
  return { state: resolveImpactState(stored, nodeName), reload };
}

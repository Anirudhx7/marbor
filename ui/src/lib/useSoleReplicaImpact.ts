import { useCallback, useEffect, useRef, useState } from 'react';
import { getSoleReplicaImpact } from './api';
import { mockSoleReplicaImpact } from './mockData';
import { resolveImpactState, type SoleImpactState } from './soleReplica';

type Stored = { node: string | null } & SoleImpactState;

// useSoleReplicaImpact fetches the "what stops being served" check for the node
// a Drain or Restart dialog is open on (nodeName null = dialog closed). reload
// refetches the same node, used after the server rejects a confirm because the
// answer changed. Demo mode reads the sample data and never calls the API.
export function useSoleReplicaImpact(nodeName: string | null, demoMode: boolean) {
  const [stored, setStored] = useState<Stored>({ node: null, status: 'idle' });
  const seq = useRef(0);

  const load = useCallback(async (name: string) => {
    const id = ++seq.current;
    setStored({ node: name, status: 'loading' });
    try {
      const impact = demoMode
        ? (mockSoleReplicaImpact[name] ?? { models: [], cold_load_only: [], unknown: [] })
        : await getSoleReplicaImpact(name);
      if (id === seq.current) setStored({ node: name, status: 'ready', impact });
    } catch {
      if (id === seq.current) setStored({ node: name, status: 'error' });
    }
  }, [demoMode]);

  useEffect(() => {
    if (!nodeName) {
      seq.current++;
      setStored({ node: null, status: 'idle' });
      return;
    }
    void load(nodeName);
    return () => { seq.current++; };
  }, [nodeName, load]);

  const reload = useCallback(() => { if (nodeName) void load(nodeName); }, [nodeName, load]);
  return { state: resolveImpactState(stored, nodeName), reload };
}

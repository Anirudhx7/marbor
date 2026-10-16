import { useCallback, useEffect, useMemo, useState } from 'react';
import { getSoleReplicaImpact } from './api';
import { mockSoleReplicaImpact } from './mockData';
import { createImpactChecker, resolveImpactState, type StoredImpact } from './soleReplica';

// useSoleReplicaImpact fetches the "what stops being served" check for the node
// a Drain or Restart dialog is open on (nodeName null = dialog closed). reload
// refetches the same node, used after the server rejects a confirm because the
// answer changed. Demo mode reads the sample data and never calls the API. The
// abort, timeout and stale-result handling lives in createImpactChecker.
export function useSoleReplicaImpact(nodeName: string | null, demoMode: boolean) {
  const [stored, setStored] = useState<StoredImpact>({ node: null, status: 'idle' });
  const checker = useMemo(() => createImpactChecker(
    async (name, signal) => demoMode
      ? (mockSoleReplicaImpact[name] ?? { models: [], cold_load_only: [], unknown: [] })
      : getSoleReplicaImpact(name, signal),
    setStored,
  ), [demoMode]);

  useEffect(() => {
    if (!nodeName) {
      checker.cancel();
      setStored({ node: null, status: 'idle' });
      return;
    }
    void checker.load(nodeName);
    return () => checker.cancel();
  }, [nodeName, checker]);

  const reload = useCallback(() => { if (nodeName) void checker.load(nodeName); }, [nodeName, checker]);
  return { state: resolveImpactState(stored, nodeName), reload };
}

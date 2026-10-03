import type { GPUNode } from '../types';

export type ReplicaAction = 'stop' | 'restart' | 'drain';

// isReplicaMember is true for a node that is the head, a worker, or an
// unresolved member of a multi-host replica declaration.
export function isReplicaMember(node: GPUNode | null | undefined): boolean {
  const role = node?.schedulingRole;
  return role === 'head' || role === 'worker' || role === 'unresolved';
}

// replicaImpactCopy states, from the node's replica topology alone, what
// stopping, restarting or draining it does to its multi-host replica. It
// returns null for a standalone node so those dialogs stay unchanged. It
// states topology facts only: it never claims other members recover on
// their own and never invents a count of affected requests or models.
export function replicaImpactCopy(node: GPUNode | null | undefined, action: ReplicaAction): string | null {
  if (!node || !isReplicaMember(node)) return null;
  const name = node.name;
  const members = [...(node.replicaPeers?.members ?? [])].sort().join(', ');
  const head = node.replicaHead ?? node.replicaPeers?.head ?? '';
  const verb = action === 'restart' ? 'Restarting' : 'Stopping';

  if (action === 'drain') {
    if (node.schedulingRole === 'head') {
      return `${name} is the head of a multi-host replica (members: ${members}). Draining it stops routing new requests to the whole replica.`;
    }
    if (node.schedulingRole === 'worker') {
      return `${name} is a worker in the multi-host replica headed by ${head}. Draining a worker has no routing effect, because only the head receives requests. Drain the head ${head} to drain the replica.`;
    }
    return `${name} is part of a multi-host replica declaration that does not resolve (members: ${members}). Its routing role cannot be determined until the declaration is fixed in Edit Node.`;
  }

  if (node.schedulingRole === 'head') {
    return `${name} is the head of a multi-host replica (members: ${members}). ${verb} its runtime takes the whole replica offline. Marbor does not restart the other members.`;
  }
  if (node.schedulingRole === 'worker') {
    return `${name} is a worker in the multi-host replica headed by ${head} (members: ${members}). ${verb} its runtime breaks that replica. Marbor does not restart or re-sync the other members.`;
  }
  return `${name} is part of a multi-host replica declaration that does not resolve (members disagree on membership or head; members: ${members}). The effect of ${verb.toLowerCase()} its runtime cannot be determined. Fix the declaration in Edit Node first.`;
}

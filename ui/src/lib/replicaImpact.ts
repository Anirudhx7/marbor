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
//
// The wording deliberately mirrors the Admin API's replica warning (the same
// sentences, same quoting) so the dialog and the CLI/API say the same thing;
// keep the two in step when changing either. The members clause is omitted
// when the member list is unknown rather than printed empty.
export function replicaImpactCopy(node: GPUNode | null | undefined, action: ReplicaAction): string | null {
  if (!node || !isReplicaMember(node)) return null;
  const name = JSON.stringify(node.name);
  const members = [...(node.replicaPeers?.members ?? [])].sort().join(', ');
  const head = JSON.stringify(node.replicaHead ?? node.replicaPeers?.head ?? '');
  const membersNote = members ? `members: ${members}` : '';
  // paren renders " (lead; members: x)", " (lead)", " (members: x)" or "".
  const paren = (lead: string) => {
    const parts = [lead, membersNote].filter(Boolean);
    return parts.length ? ` (${parts.join('; ')})` : '';
  };
  const verb = action === 'restart' ? 'restarting' : 'stopping';

  if (action === 'drain') {
    if (node.schedulingRole === 'head') {
      return `node ${name} is the head of a multi-host replica${paren('')}: draining it stops routing new requests to the whole replica.`;
    }
    if (node.schedulingRole === 'worker') {
      return `node ${name} is a worker in the multi-host replica headed by ${head}: draining a worker has no routing effect, because only the head receives requests. Drain the head ${head} to drain the replica.`;
    }
    return `node ${name} is part of a multi-host replica declaration that does not resolve${paren('')}: its routing role cannot be determined until the declaration is fixed in the replica settings.`;
  }

  if (node.schedulingRole === 'head') {
    return `node ${name} is the head of a multi-host replica${paren('')}: ${verb} its runtime takes the whole replica offline. Marbor does not restart the other members.`;
  }
  if (node.schedulingRole === 'worker') {
    return `node ${name} is a worker in the multi-host replica headed by ${head}${paren('')}: ${verb} its runtime breaks that replica. Marbor does not restart or re-sync the other members.`;
  }
  return `node ${name} is part of a multi-host replica declaration that does not resolve${paren('members disagree on membership or head')}: the effect of ${verb} its runtime cannot be determined. Fix the declaration in the replica settings first.`;
}

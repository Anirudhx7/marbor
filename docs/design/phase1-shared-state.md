# Phase 1 — Shared Cluster State & Leader Election

**Status:** Design / decision pending
**Author:** engineering review, 2026-07
**Goal:** Turn ollama-mesh from a smart *single-node* proxy into *infrastructure* — multiple
mesh instances that share one consistent view of quotas, registry, and ownership, and that
survive the loss of any single instance without double-counting or split-brain.

---

## 1. Why this is the gate

Everything above this layer (real scheduler, model lifecycle) can be built per-instance.
Cluster-wide correctness cannot. Today each mesh instance is authoritative only for itself:

- **Quota divergence (correctness/billing bug).** `auth.Middleware` holds per-key rate/day/month
  counters in memory and flushes to local SQLite every 30s (`main.go` ticker). Two instances
  behind a load balancer each enforce the limit independently → a key capped at 1000/day
  effectively gets `1000 × N`. Verified in the July review.
- **"HA" is observation-only.** `internal/ha` polls peers and logs reachability. There is no
  leader, no fencing, no shared state — the docs imply failover that does not exist.
- **Registry is local.** Nodes, drain state, routing rules, and API keys live in each instance's
  SQLite. Add a node on instance A and instance B never learns about it.

Until these are shared, "HA" and "run 20 GPUs as a cluster" are not truthful claims.

## 2. What state must be shared (and how strongly)

| State | Consistency needed | Notes |
|---|---|---|
| Per-key quota counters (rate/day/month) | **Strong-ish** | Double-spend is the headline bug. Bounded overshoot may be acceptable (see §5). |
| API keys (create/revoke) | Strong | A revoked key must stop working cluster-wide promptly. |
| Node registry + drain/override state | Eventual (seconds) | All instances should converge on the node set. |
| Routing rules | Eventual | Rarely changes. |
| Admin users/sessions | Strong | A session must validate on any instance. |
| Analytics / request log | Eventual, mergeable | Aggregation; can tolerate lag and per-node partial writes. |
| Leadership (who runs singletons: warmer, counter authority) | Strong | Exactly-one semantics. |

Key insight: **only quotas + keys + leadership need strong consistency.** Registry/analytics can
be eventually consistent. This lets us avoid putting consensus on the request hot path.

## 3. Options

### A. Embedded Raft (`hashicorp/raft` or `dragonboat`)
Mesh instances form their own quorum; state lives in a replicated FSM, persisted to each node's
disk. Leader elected among the mesh instances themselves.
- **Pros:** Preserves the "single static Go binary, no external deps" identity (the core brand).
  Zero extra services to operate. Strong consistency + real leader election out of the box.
- **Cons:** We *own* a consensus deployment — quorum sizing (need 3+ for fault tolerance), snapshot
  management, membership changes, and a large test surface (partition/restart/rejoin). Highest
  code + long-term maintenance cost. Overkill for the single-node homelab default.

### B. External Postgres as state authority (recommended — see §6)
Shared state in Postgres. Quota increments = atomic `UPDATE ... RETURNING`. Leader election =
`pg_advisory_lock`. Keys/registry/sessions = ordinary rows other instances read.
- **Pros:** Mature, boring, operable — enterprises already run Postgres. Quota integrity and
  analytics become trivial SQL. Far less code than owning Raft. Strong consistency for free.
- **Cons:** Adds a required external dependency **when running multi-instance**. A single busy
  Postgres becomes a dependency for the control plane (mitigated by connection pooling + the fact
  that local nodes keep serving inference even if Postgres blips — see §5 degradation).

### C. etcd
Distributed KV with native leases/leader election (what Kubernetes uses).
- **Pros:** Purpose-built for leader election + watches; strong consistency.
- **Cons:** Another stateful cluster to run; quota counters/analytics are awkward in a KV model
  (no atomic numeric increments or aggregation queries). Ops burden without Postgres's ergonomics.

### D. Redis (rejected as primary)
Great for fast shared counters, but not strongly consistent and leader election via Redlock is
contested. Fine as an *optional* rate-limit accelerator later; not a correctness foundation.

## 4. Recommendation: **Tiered — SQLite single-node (default), optional Postgres for HA**

Do **not** force a distributed system on the 90% single-node case. Instead:

- **Single instance (default, homelab):** unchanged — local SQLite, no external dependency, single
  static binary. This protects the brand and the common user.
- **Multi-instance HA (opt-in, the paying ICP):** set `storage.backend: postgres` +
  `cluster.enabled: true`. All instances point at one Postgres; quotas/keys/registry/sessions/
  leadership move there.

This gives real HA with a well-understood dependency, **an order of magnitude less code to own than
embedded Raft**, and keeps the single-binary story intact where it matters. Embedded Raft (Option A)
stays the fallback if "zero external deps even in HA" ever becomes a hard requirement — but that is
a large build whose cost is hard to justify against the project's real bottleneck (distribution),
per the product strategy.

Rejected alternatives: etcd (poor fit for quota/analytics), Redis (not a correctness store).

## 5. Quota model (the hard part) — leased allocation, not per-request consensus

Naively hitting shared state on every request adds a round-trip to the hot path and makes the
store a throughput bottleneck. Instead:

1. Introduce a `QuotaStore` interface. Local (SQLite) impl = today's behavior. Postgres impl =
   shared.
2. **Lease-based counting:** each instance requests a *lease* of N units (e.g. 50 requests) for a
   key via one atomic `UPDATE keys SET day_used = day_used + 50 ... WHERE day_used + 50 <= day_limit
   RETURNING`. It then serves those 50 locally with zero coordination, and requests another lease
   near exhaustion. Worst-case overshoot is bounded by `N × instances` and is tunable (N=1 →
   exact, higher N → less coordination).
3. On graceful shutdown / lease expiry, unused lease is returned so counters don't leak.

This keeps the request path fast, makes double-spend bounded-and-configurable rather than
unbounded, and degrades safely: if Postgres is unreachable, an instance keeps serving from its
outstanding lease and fails **closed** on quota once the lease is exhausted (operator-tunable to
fail-open for availability-first deployments).

## 6. Leadership & singletons

`pg_advisory_lock(key)` (Postgres) or a Raft leader (Option A) elects exactly one instance to own
cluster singletons: the model **warmer** (today every instance warms independently — wasteful and
racy), lease reconciliation, and analytics compaction. Non-leaders proxy inference normally;
losing the leader triggers re-election, and singletons resume on the new leader.

## 7. Migration path (incremental, no big-bang)

1. **Extract interfaces** behind today's concrete types: `QuotaStore`, `Registry`, `SessionStore`,
   `Leader`. Wire the existing SQLite code as the default impl. *No behavior change; fully
   shippable on its own.*
2. **Add the Postgres impl** of each interface behind `storage.backend: postgres`. Ship
   quotas first (highest-value correctness fix), then keys/sessions, then registry.
3. **Add leader election**; move the warmer and lease reconciliation behind it.
4. **Re-document HA honestly**; add an integration test that runs 2 instances + Postgres and
   asserts a shared key's quota is not exceeded and survives killing one instance.

Each step is independently mergeable and leaves `main` green.

## 8. Open questions for the maintainer

- Is a required Postgres acceptable for the HA tier, or is "zero external deps even in HA" a hard
  constraint (→ then Option A / embedded Raft, at materially higher cost)?
- Acceptable quota overshoot bound under HA (drives the lease size N; N=1 = exact but chattier)?
- Fail-open vs fail-closed on state-store unreachability (availability vs strict quota integrity)?

## 9. Recommended first PR

Step 1 only — extract `QuotaStore`/`Registry`/`SessionStore`/`Leader` interfaces with the current
SQLite behavior as the default implementation. Pure refactor, no new dependency, no behavior
change, `main` stays green — and it unblocks everything else.

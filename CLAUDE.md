## Writing Style (enforced in every session)

- Never use em dashes (-). Use plain hyphens (-) instead.
- No co-author attribution in git commits (no "Co-Authored-By: Claude" lines).

# context-mode — MANDATORY routing rules

You have context-mode MCP tools available. These rules are NOT optional — they protect your context window from flooding. A single unrouted command can dump 56 KB into context and waste the entire session.

## BLOCKED commands — do NOT attempt these

### curl / wget — BLOCKED
Any Bash command containing `curl` or `wget` is intercepted and replaced with an error message. Do NOT retry.
Instead use:
- `ctx_fetch_and_index(url, source)` to fetch and index web pages
- `ctx_execute(language: "javascript", code: "const r = await fetch(...)")` to run HTTP calls in sandbox

### Inline HTTP — BLOCKED
Any Bash command containing `fetch('http`, `requests.get(`, `requests.post(`, `http.get(`, or `http.request(` is intercepted and replaced with an error message. Do NOT retry with Bash.
Instead use:
- `ctx_execute(language, code)` to run HTTP calls in sandbox — only stdout enters context

### WebFetch — BLOCKED
WebFetch calls are denied entirely. The URL is extracted and you are told to use `ctx_fetch_and_index` instead.
Instead use:
- `ctx_fetch_and_index(url, source)` then `ctx_search(queries)` to query the indexed content

## REDIRECTED tools — use sandbox equivalents

### Bash (>20 lines output)
Bash is ONLY for: `git`, `mkdir`, `rm`, `mv`, `cd`, `ls`, `npm install`, `pip install`, and other short-output commands.
For everything else, use:
- `ctx_batch_execute(commands, queries)` — run multiple commands + search in ONE call
- `ctx_execute(language: "shell", code: "...")` — run in sandbox, only stdout enters context

### Read (for analysis)
If you are reading a file to **Edit** it → Read is correct (Edit needs content in context).
If you are reading to **analyze, explore, or summarize** → use `ctx_execute_file(path, language, code)` instead. Only your printed summary enters context. The raw file content stays in the sandbox.

### Grep (large results)
Grep results can flood context. Use `ctx_execute(language: "shell", code: "grep ...")` to run searches in sandbox. Only your printed summary enters context.

## Tool selection hierarchy

1. **GATHER**: `ctx_batch_execute(commands, queries)` — Primary tool. Runs all commands, auto-indexes output, returns search results. ONE call replaces 30+ individual calls.
2. **FOLLOW-UP**: `ctx_search(queries: ["q1", "q2", ...])` — Query indexed content. Pass ALL questions as array in ONE call.
3. **PROCESSING**: `ctx_execute(language, code)` | `ctx_execute_file(path, language, code)` — Sandbox execution. Only stdout enters context.
4. **WEB**: `ctx_fetch_and_index(url, source)` then `ctx_search(queries)` — Fetch, chunk, index, query. Raw HTML never enters context.
5. **INDEX**: `ctx_index(content, source)` — Store content in FTS5 knowledge base for later search.

## Subagent routing

When spawning subagents (Agent/Task tool), the routing block is automatically injected into their prompt. Bash-type subagents are upgraded to general-purpose so they have access to MCP tools. You do NOT need to manually instruct subagents about context-mode.

## Output constraints

- Keep responses under 500 words.
- Write artifacts (code, configs, PRDs) to FILES — never return them as inline text. Return only: file path + 1-line description.
- When indexing content, use descriptive source labels so others can `ctx_search(source: "label")` later.

## ctx commands

| Command | Action |
|---------|--------|
| `ctx stats` | Call the `ctx_stats` MCP tool and display the full output verbatim |
| `ctx doctor` | Call the `ctx_doctor` MCP tool, run the returned shell command, display as checklist |
| `ctx upgrade` | Call the `ctx_upgrade` MCP tool, run the returned shell command, display as checklist |

---

# ollama-mesh — Project Context

Private AI inference control plane written in Go. Sits in front of multiple GPU backend nodes and manages intelligent load balancing, warm-state scheduling, VRAM-aware eviction, session-affinity, and predictive model warming. Single Go binary, zero external dependencies. Apache-2.0.

## Standing Directive: Product-First Sequencing (Anirudh, 2026-07-16)

- **Current priority is making the product elite-class, not distributing it.** Until Anirudh
  says otherwise, do not propose, plan, or flag distribution/marketing work (launch posts,
  Reddit/HN timing, directory submissions, SEO, outreach) in any audit, queue suggestion, or
  "what's next" answer. That work is deliberately paused, not forgotten - raising it anyway is
  noise, not helpfulness.
- **What counts as in-scope right now:** anything that makes the product itself better for a
  real user's day-to-day usage - bug fixes (especially ones that would embarrass a serious
  user), stability, missing features that close real gaps, UI/UX polish, performance,
  reliability, upgrades to dependencies/runtimes. If it changes what the product DOES or how
  well it does it, it's in scope. If it changes who hears about it, it's out of scope for now.
  This does not touch the Buyer Gate or Architecture Laws - a feature still has to serve the
  4-20 GPU on-prem buyer and stay within the architecture; it just doesn't need a distribution
  angle to be worth doing.
- **Cutting a release (tagging v*, updating CHANGELOG) is product work, not distribution** -
  that's shipping the fixes to the people who already have it installed, not acquiring new
  users. Keep doing releases normally.
- Distribution comes back into scope only when Anirudh explicitly reopens it.

## Working Style (Repeatable Preferences)

- **Autonomous multi-agent execution.** Use parallel background agents for independent workstreams. Sequence agents that share files (`placement.go`, `proxy.go`, `admin.go` are hotspots). Agents implement + test but never commit; main session commits in clean conventional-commit increments. No co-author lines.
- **Git Hygiene.** Never use `git add .` or wildcard staging. Always add files intentionally by naming the exact paths (e.g., `git add docs/integrations/litellm.md`).
- **Keep momentum.** Don't ask permission for reversible work. Push to main when told to push. Releases ship by tagging v* (goreleaser workflow handles binaries + ghcr image).
- **Internal docs stay in `.local/`** (gitignored). Strategy, vision, gates, launch playbooks NEVER get committed.
- **`.local/` syncs across machines via its own private repo** (`ollama-mesh-private`, nested at
  `.local/.git` — invisible to this outer repo since `.local/` is excluded via
  `.git/info/exclude`; a second, independent repo on the same disk path, not a submodule).
  Run `.local/sync.sh pull` at the START of any session, before reading or editing anything
  under `.local/` or this file. Run `.local/sync.sh push` at the END of any session/turn where
  a file under `.local/` **OR this root `CLAUDE.md` file** was created or edited — root
  `CLAUDE.md` is the one agents actually touch day to day (not `.local/CLAUDE.md` directly),
  and it's just as much a sync trigger as anything under `.local/`, even though it isn't
  physically inside that folder. Batch pushes, don't push after every individual edit (that's
  noisy commit spam and adds git latency to every small change for no benefit). Also run
  `.local/sync.sh push` or `pull` immediately whenever Anirudh explicitly says push/pull/sync.
  If `sync.sh pull` fails (merge conflict from the other machine), stop and surface it to
  Anirudh rather than resolving a `.local/` conflict silently — these are his strategy/roadmap
  docs, not code with an obvious right answer.
- **Go is installed locally** (go1.26+, confirmed 2026-07-15). Use it directly: `go build ./...`, `go test -race ./...`. Docker (`golang:1.25` image) is only a fallback if the local toolchain breaks.
- **Brand icon is the golden "M" logo.** `ui/public/favicon.svg` is the source of truth - dark rounded rect (`#0a0a0a`), gold path (`#d4a853`), amber circle (`#a87f3a`). Use it everywhere an icon/logo appears in the UI.
- **Honest data is the brand.** Real parsed token counts or "—". Never estimates presented as measurements. Demo mode defaults OFF with a visible banner.
- **Repo indexed with codebase-memory** (project `C-Users-AKM-Desktop-Ollama-Mesh-ollama-mesh`). Use `search_graph`/`search_code`/`trace_path`/`get_code_snippet`/`query_graph`/`get_architecture` for code exploration before Grep/Read. Re-index (`index_repository`) after big structural changes.

---

## Architecture Laws (Permanent - NOT Preferences)

If any suggestion or audit recommendation violates these laws, **reject it immediately**:
1. **ONE mesh process, N backend GPU nodes.** Always. Single-instance gateway is the design, not a limitation.
2. **SQLite is the ONLY datastore.** Always. No Postgres, Redis, etcd, Cassandra, DynamoDB, or MySQL. Ever.
3. **No distributed state.** Ever. No leader election, no consensus systems, no Raft, no multi-instance coordination, and no shared state between mesh processes. Ever.
4. **Single static Go binary** is a permanent product promise. Zero external dependencies.

---

## Critical Code Guards (Do Not Violate)

1. **R1. No fake data in production paths.** Live parsed metrics or `—`. Never random numbers or estimated/simulated latency figures.
2. **R2. Streaming must stay streaming.** Never buffer proxy responses. `statusRecorder` must implement `http.Flusher`.
3. **R3. Concurrent map access requires mutex.** `auth.go` maps must use `m.mu.RWMutex` (RLock on reads, Lock on writes).
4. **R4. Admin token auth is exact match.** `strings.TrimPrefix(authHeader, "Bearer ") == s.adminToken`. Never use `strings.Contains`.
5. **R5. Port derivation must parse the URL.** `url.Parse(n.URL).Port()`. Never derive ports arithmetically.
6. **R6. The UI is embedded in the binary.** Uses `//go:embed web/dist`. Run `make build` for a full release to avoid stale UI embedding.
7. **R7. Config backward compatibility.** New fields require defaults in `config.go Validate()`, a `settings` KV entry (`internal/store/settings_helpers.go`), and admin API + UI wiring (`admin.go` handleUpdateSettings/handleSettings, `Settings.tsx`) — DB-first config has no file to keep an "example" contract for.
8. **R8. Secrets at rest (`internal/store/secretbox.go`).** Cloud provider API keys, mesh-issued API keys, LiteLLM key, HuggingFace token, and webhook secret are AES-256-GCM encrypted in `mesh.db` (master key in `mesh.db.key` or `MESH_ENCRYPTION_KEY` env). Rules for ANY new secret-bearing field:
   - **Settings-table secrets** (a plain string value under a `settings` key, like `litellm_api_key`) → add the key to `sensitiveSettingKeys` in `secretbox.go`. That's the only wiring needed — `GetSetting`/`SetSetting` encrypt/decrypt transparently at that one chokepoint. Forgetting this is the failure mode: the value silently stays plaintext, no error, nothing flags it.
   - **Dedicated table/column secrets** (like `cloud_providers.api_key`, `runtime_keys.key`) → call `encryptSecret(s.secretKey, ...)` before every `INSERT`/`UPDATE` that writes the column, and `decryptSecret(s.secretKey, ...)` after every `SELECT` that reads it. Add the table to `migrateEncryptSecrets()` so pre-existing plaintext rows get encrypted on the next boot.
   - **Never fail a whole-list read (`AllKeys`, `AllCloudProviders`, or any future `AllX`) on one bad row's decrypt error.** These lists feed `auth.go`'s key map and the router's cloud-provider chain at boot/reload behind an `if err == nil` pattern (`main.go:283`, `admin.go:1204`) — one corrupt/undecryptable row returning an error there zeroes out EVERY key/provider, not just the broken one. Log and **drop the row** (`continue`), matching `AllKeys`/`AllCloudProviders` in `sqlite.go`.
   - **Never substitute `""` for a secret that failed to decrypt.** `auth.go` keys its map by the literal secret string; a row with `Key=""` is trivially reachable via `Authorization: Bearer ` (trailing space, empty token) and would authenticate as that key. Drop the row instead — this is not a hypothetical, it was caught and fixed in the same session R8 shipped (see LESSONS.md L13).
   - Single-value reads (`GetSetting` on one sensitive key) may return the error as-is — the blast radius is that one setting, not a whole list, so failing loudly there is fine; callers already default gracefully via `GetStringSetting`.
9. **R9. Node Agent Protocol compatibility (frozen v1, `internal/nodeagent`, `internal/router/agent_poll.go`).** The `/v1/status`/`/metrics` wire format must support rolling upgrades — mesh and agents on different versions across the same fleet, indefinitely, never requiring a simultaneous fleet-wide upgrade. Rules for ANY future change to `Telemetry`/`GPUBlock`/`GPUInfo`/`HostTelemetry`/`RuntimeInfo` or their mesh-side consumers:
   - **Additive whenever possible.** New information is a new optional field or a new `Capabilities` entry — never a change to an existing field's type, name, or JSON key.
   - **Existing fields never change meaning, never get removed.** A field no longer needed is left in place (possibly always empty going forward), not deleted.
   - **Unknown fields always ignored.** Never add `DisallowUnknownFields()` (or equivalent) to any decode path in this protocol.
   - **Missing fields always treated as unknown**, never fabricated as a real zero-value measurement — R1 extended to "a field this version doesn't send yet."
   - **New functionality goes through `Capabilities`, not a wire-format change.** A capability string is added in the same commit that implements the feature it names, never speculatively, named `resource.verb` (e.g. `models.pull`, `runtime.restart`) to match the protocol's resource-oriented route namespace (`/v1/models`, `/v1/runtime/*`).
   - **The `runtime` resource stays generic** (`name`/`version`/`status`/`warm_models`/`queue_depth`) — a runtime-specific detail (e.g. a future vLLM tensor-parallel degree, a ROCm/TensorRT version) belongs in its own future runtime-specific resource, never a field bolted onto this one.
   - **`ProtocolVersion` bumps only for a genuinely breaking wire-format change** — not for ordinary additive field additions. If one is ever needed, pair it with a migration note in `.local/specs/node-agent.md`'s Compatibility Guarantees section, not just a CHANGELOG line.
   - Full reasoning, the rolling-upgrade review, and the enforcement rule: `.local/specs/node-agent.md` §15's "Node Agent Protocol Compatibility Guarantees" — that section is the source of truth if the two ever disagree.

---

## State Hierarchy (Truth Precedence)

1. **Live Runtime State** (what `/api/ps` actually returns from nodes right now)
2. **SQLite Persisted State** (warm_state table, recovery memory on boot)
3. **Derived Heuristics** (predictions, placement scores)

*Live always beats persisted. Persisted always beats derived. SQLite is recovery memory, not the ultimate source of truth.*

---

## Routing Hierarchy (Precedence Order)

1. **Hard Constraints** (unhealthy nodes, incompatible runtimes, capacity limits, pinned models, explicit deny rules). Cannot be overridden.
2. **Session Affinity** (soft preference for KV-cache preservation). `session_id -> node_id` mapping. Never hard-lock; failover if capacity/health degrades.
3. **Warm Residency** (prefer already-loaded models to avoid cold starts).
4. **Weighted Placement Scoring**:
   `score = (warm_resident x 50) + (free_vram_headroom x 20) + (inverse_queue_depth x 15) + (node_health x 10) + (recent_success_rate x 5) + (prefix_match x prefix_locality_weight, default 10, opt-in)`
   Two more factors apply outside this base formula, both in `computeNodeScore` (placement.go): a **-50 error-cooldown penalty** (score floored at 0) when a node had an upstream error in the last 60s, and the **prefix_match** term above (Step 6) - a soft nudge toward the node that last served an identical prompt-prefix hash, gated by `routing.prefix_locality_enabled` (default off) and capped well below `warm_resident`/`inverse_queue_depth` so it can never flip a warm-vs-cold or real-load decision.
5. **Predictive Hints** (weak intelligence layer, time-of-day/frequency trends). Influence only, never override warm reality.

---

## Roadmap — Source of Truth (MANDATORY)

- **`.local/core/ROADMAP-MASTER.md` is the ONLY authoritative roadmap.** Public `ROADMAP.md` is derived from it (public-safe subset). If they conflict, the master wins.
- **`.local/ai-audits/roadmap-2.7.26.md` is STALE — never follow it.** Its Steps 1-5 already shipped in v0.11-v0.14. Only Step 6 (prefix locality) from that document remains relevant, and it is captured in both current roadmaps.
- **Current sequence:** (1) hardware validation benchmarks + community launch → (2) feedback window → (3) Step 6 prefix locality → (4) "Later" bucket items, graduated by user pull only.
- **Do NOT start Step 6 or any "Later" item before launch.** Pre-launch work is limited to: bench/validation, launch assets, and small pending closables.
- **Exception in progress:** Step 6 (prefix locality) is being built EARLY, as a gated experiment, on branch `feature/step6-prefix-locality` — Anirudh's explicit call, not a queue violation. Full design: `.local/specs/step6-prefix-locality.md`. Live status/branch rules/go-no-go test protocol/merge-or-delete decision tree: `.local/core/STEP6-EXPERIMENT-STATUS.md` — **check this file whenever Step 6, prefix locality, that branch, or `router.go`/`placement.go`/`proxy.go`/`admin.go`/`sqlite.go` changes come up**, since another session may be mid-build on it and the file records what state it's in.
- **Rejected directions live in ROADMAP-MASTER "Rejected - with reasons."** Do not relitigate them when an audit or AI suggestion re-proposes distributed state, policy engines, or speculative runtime adapters — reject citing that section.
- Strategic frame: the target is default control plane for **non-Kubernetes self-hosted multi-GPU inference (4-20 GPU shops)** — not competing with llm-d/Ray Serve/GPUStack for 100+ GPU Kubernetes estates.

## Versioning Policy (MANDATORY)

- **The product is in beta. All releases stay on `v0.x.x` until further notice.**
- **v1.0.0 is a deliberate milestone, never an increment.** It ships only when the product is
  fully ready for the target buyer's use cases — and ONLY with Anirudh's explicit sign-off.
  Never propose or tag a 1.0 autonomously. Prior agreed criteria: real external operators in
  production, retention signal, zero known data-integrity bugs, stranger-installable docs.
- Patch (`v0.x.Y`) for fixes; minor (`v0.X.0`) for features. Breaking config changes need a
  migration note in CHANGELOG even pre-1.0.

---

## Public vs Private (MANDATORY judgment rule)

- **Public repo** gets: code, tests, user docs, CHANGELOG, public ROADMAP.md, CI. Nothing else.
- **`.local/` (never committed)** gets: strategy, audits, launch drafts, runbooks, competitive analysis, revenue/pricing thinking, anything mentioning internal gates or tripwires.
- **`.local/core/`** holds the essential never-delete operating files (ROADMAP-MASTER.md, EXECUTION-QUEUE.md, posts.md, future runbooks). Treat as protected: never delete, never bulk-overwrite; robocopy/sync operations must exclude it (/XD .local) or use /E not /MIR.
- **Default when unsure: private.** Moving a file public later is easy; unpublishing is impossible.
- The public `.gitignore` stays boring (standard Go/Node entries + secret protection only). Private-structure ignores live in `.git/info/exclude`, which is never visible publicly. When creating a NEW private file type, add its pattern to `.git/info/exclude`, not `.gitignore`.

---

## Operator Commands (how Anirudh drives this project)

Anirudh gives short commands; agents execute the full pipeline without asking for direction:

- **"build the next item"** → open `.local/core/EXECUTION-QUEUE.md`, take the topmost UNBLOCKED item, verify-before-build, implement to Definition of Done, mark the item done in the queue with the commit hash, report honestly.
- **"test the new updates"** → run `scripts/gate.sh` (requires Docker Desktop running) + exercise the changed feature end-to-end (real requests through the proxy, not just unit tests). Report pass/fail with evidence; never soften failures.
- **"ship a release"** → gate green on main, CHANGELOG.md updated, tag `v*`, push tag (goreleaser builds binaries + ghcr image). Confirm the tag with Anirudh before pushing it — releases are irreversible.
  - **Minor releases (v0.X.0) additionally require the `release-auditor` agent** (defined in `.claude/agents/release-auditor.md`, runs on Sonnet): audit the diff since the previous minor tag for security regressions, correctness bugs, and architecture/vision drift. A BLOCK verdict stops the release until findings are fixed or Anirudh explicitly overrides. Patch releases (v0.x.Y) skip the audit unless `auth.go`, `proxy.go`, or `router/` changed.
- Agents may APPEND newly discovered work to the queue as `PROPOSED` items with rationale; only Anirudh promotes them to the ordered list.
- If the queue is empty or every item is blocked, say so and stop — do not invent work.

### Continuous Improvement Protocol (MANDATORY — every session, not just bug fixes)

The workflow improves itself autonomously, with oversight. Two tiers, defined in
`.local/core/IMPROVEMENTS.md`:

- **After completing any work item**, run a 60-second retrospective: what was slow, repeated,
  manually re-explained, or error-prone this session? If something qualifies:
  - **Tier A (auto-apply):** additive, reversible improvements inside `.local/` or rules-of-record
    (new lessons, sharper queue specs, stale-path fixes, regression tests for existing behavior)
    → apply immediately, log in IMPROVEMENTS.md AUTO-APPLIED.
  - **Tier B (propose-only):** anything changing standards, process, CI, DoD, conventions, or
    public files beyond item scope → write it under IMPROVEMENTS.md PROPOSED with rationale.
    Never self-apply.
- **Report every time:** any session that logged an improvement MUST state it in the final
  message to Anirudh ("Self-improvements this session: ..."), so drift is visible immediately,
  not discovered later.
- **Check REJECTED before proposing** — 2+ rejections of the same flavor means stop proposing
  that flavor and record why.
- This protocol persists across all sessions via this file; IMPROVEMENTS.md and LESSONS.md are
  the memory. Read both when starting substantive work.

### Recurring Problem Protocol (MANDATORY — the workflow must learn)

- **Before fixing any bug, test failure, or CI failure:** read `.local/core/LESSONS.md`. If the
  problem matches a known class, apply the recorded permanent fix pattern — do not rediscover it.
- **After any fix:** if the problem class was seen before (2nd occurrence), a patch is NOT enough.
  Add a permanent guard in the same work item: a regression test, a gate.sh/CI step, a DoD item,
  or a release-auditor checklist line — whatever kills the class. Then record it in LESSONS.md.
- **When Anirudh gives the same correction twice**, that is a failed capture: encode it
  immediately as a LESSONS.md entry or a CLAUDE.md rule so he never has to say it a third time.
- **3+ recurrences of one class = the guard is failing.** Stop, report it to Anirudh explicitly,
  do not silently patch again.

### Skill usage during development (targeted, not maximal)

- **Building a queue item with testable behavior** → use `superpowers:test-driven-development` (test first, then implementation).
- **Fixing a reported bug** → use `superpowers:systematic-debugging` (reproduce → isolate → fix), never patch on symptom.
- **Before committing hot-path changes** (`auth.go`, `proxy.go`, `router/`) → run the `code-review` skill on the diff; `verify` skill to exercise the change end-to-end when it has a runtime surface.
- **Do NOT use GSD skills (`gsd-*`) on this project.** GSD is a competing project-management system (own roadmaps, own planning dirs); this project's workflow is CLAUDE.md + ROADMAP-MASTER + EXECUTION-QUEUE (both in `.local/core/`). Two sources of truth = drift.
- Marketing/SEO/launch skills (`seo-audit`, `launch`, `ai-seo`, etc.) are for launch tasks when Anirudh asks — never during "build the next item".

## Definition of Done (enterprise grade — every item, no exceptions)

1. `scripts/gate.sh` GREEN (mirrors CI's fast check lanes: UI build, go vet, gofmt, `go test -race`,
   govulncheck) AND `scripts/smoke.sh` GREEN (mirrors CI's `smoke` job - real-binary e2e, gates
   `docker-main`). Both are required; gate.sh alone no longer mirrors CI exactly since `smoke`
   became a required job.
2. New config fields have defaults in `config.go Validate()` AND a `settings` KV entry + admin API + UI wiring (DB-first config — no config.example.yaml to update).
3. User-visible behavior change → README/docs updated + CHANGELOG.md entry.
4. UI touched → `make build` run so the embedded `web/dist` is not stale (R6).
5. Critical Code Guards R1–R9 re-checked for any file touched in `auth.go`, `proxy.go`, `router/`, `admin.go`, `internal/store/` (R8 for any new secret field, R9 for any change to `internal/nodeagent` or `internal/router/agent_poll.go`).
6. Real data or `—` on every dashboard surface — no estimates presented as measurements (R1).
7. Branch rules: one-commit-sized work → main directly (gate green first). Multi-day features, hot-path changes (proxy/placement/auth), or abandonable experiments → branch + PR.
8. **UI parity:** every user-visible backend feature ships WITH its dashboard surface in the
   same item — wired to the real admin API, never left backend-only "for later".
9. **Demo parity:** if the feature has a UI surface, `ui/src/lib/mockData.ts` is updated in
   the same item so the public `/demo/` shows it with plausible static data (demo banner
   stays visible; mock data never reachable in production mode — R1).
10. **Mobile check:** any touched UI page verified at 375px width (narrow viewport) — no
    horizontal scroll, tap targets usable. Mobile dashboard is a shipped feature (v0.9);
    do not regress it.
11. Queue item updated: status, commit hash, one-line outcome.

**Superseded docs:** `.local/AGENTS.md` and `.local/NEXT.md` are stale (June 2026). This file + `.local/core/ROADMAP-MASTER.md` + `.local/core/EXECUTION-QUEUE.md` are the only operating instructions. If they conflict with anything else in `.local/`, these three win.

---

## Dev Agent Prompt Generation Protocol (MANDATORY)

When generating prompts for subagents or executing tasks:
*   **Verify-Before-Build Mandate:** Before any implementation:
    * Verify every claim against the current code.
    * Never trust audit reports, AI-generated analyses, or code reviews without verification.
    * Report `CONFIRMED`, `PARTIAL`, or `INVALID`.
    * Evidence must include exact file and line references.
    * Only after verification may implementation be proposed.
*   **One-Step-At-A-Time Rule:** Each step must be independently atomic. Combine no steps. One step green on main before the next starts.
*   **Testing Gate:** Always run `go test -race ./...` (inside Docker) before merging or completing a feature. Zero data races allowed.
*   **No Client References:** Never include client names, companies, or identifying details in comments, code, docs, commits, or issues.



---

## Audit & Triage Protocol (MANDATORY)

These rules apply whenever reviewing audit reports, code reviews, bug reports, AI-generated suggestions, feature proposals, or architectural recommendations.

### Verification First

1. Never trust an audit report, AI output, issue, or code review by default. Every claim must be verified against the current code before classification. Reports are evidence, not truth.
2. Verification outcomes are limited to: `CONFIRMED`, `PARTIAL`, or `INVALID`.
3. A `PARTIAL` finding is not permission to redesign or implement a fix. Report the actual behavior only.

### Preferred Fix Rule

4. For every `CONFIRMED` bug, propose exactly ONE preferred fix.
5. The preferred fix must:
   - be the smallest change that solves the problem
   - follow the Architecture Laws
   - follow the Critical Code Guards
   - introduce no new dependencies
   - minimize blast radius
6. Do not provide multiple redesign options unless explicitly requested.

### Priority Rule

7. Confirmation does not imply implementation. Every confirmed issue must also receive one priority:
   - NOW
   - POST-LAUNCH
   - BACKLOG

### Atomic Execution

8. Never recommend implementing multiple unrelated fixes together.
9. Every approved change must solve one problem, pass tests, merge cleanly, and become the new baseline before another begins.

### Blast Radius

10. Every confirmed issue must estimate:
    - Blast radius (LOW / MEDIUM / HIGH)
    - Estimated files touched
    - Whether routing hierarchy changes
    - Whether SQLite schema changes
    - Whether configuration changes

### Architecture Gate

11. Before proposing any fix or feature, verify it does not violate:
    - Architecture Laws
    - Critical Code Guards
    - State Hierarchy
    - Routing Hierarchy

If it violates one of them, reject it immediately.

### Buyer Gate

12. New features must first pass the Entry-Buyer Test.

If they do not solve a real problem for the current target buyer, classify them as CEILING WORK unless explicitly requested.
---

## Market Positioning & Moat

*   **Target Buyer:** Infrastructure/MLOps engineer running 4-20 GPUs on-premise with no orchestration. Solve their immediate pains: cold starts, VRAM waste, lack of visibility.
*   **The Moat:** Model lifecycle intelligence (warm persistence, warm restoration before traffic, predictive warming, session-affinity KV preservation, VRAM-aware eviction, weighted placement).
*   **Category:** Private AI inference control plane. Not an API gateway, not a LiteLLM replacement.

---

## Repository Structure

```
ollama-mesh/
  main.go                         # Entry point. Wires everything.
  mesh.db                         # SQLite database (gitignored) - sole config/state store, DB-first
  Makefile                        # build, ui, test, clean, dev-ui
  Dockerfile                      # Multi-stage Go build
  docker-compose.yml              # Full stack: mesh + 2 ollama instances
  .github/workflows/ci.yml        # CI: npm build + go test on every PR

  internal/
    config/config.go              # Config struct + Validate() defaults (no file I/O - DB-first)
    auth/auth.go                  # API key middleware, rate limiter
    router/router.go              # Warm-first routing, node state, poller
    proxy/proxy.go                # httputil reverse proxy, streaming, cloud fallback
    metrics/metrics.go            # 14 Prometheus metrics
    admin/admin.go                # REST admin API + UI embedded serving
    store/sqlite.go               # SQLite database access layer
```

---

## Build & Run Commands

```bash
make build      # Full build (UI + Go binary)
make backend    # Go only (fast, for backend changes)
make ui         # UI only
go test ./...   # Run tests (if local Go available)
make dev-ui     # UI dev server (hot reload, hits backend at localhost:8080)
make clean      # Clean everything
```

**Binary starts three servers:**
- `:11434` — Ollama-compatible proxy (main service)
- `:8080` — Admin dashboard + REST API
- `:9090` — Prometheus metrics
- **Prefer deletion over addition.** Prefer simplification over abstraction. Prefer one well-tested implementation over configurable alternatives.
- **Optimize only after measurement.** Never introduce complexity for speculative performance improvements. Benchmarks take precedence over intuition.

**`scripts/gate.sh` Docker caches:** two named volumes persist across runs so modules/build
objects aren't re-downloaded from `proxy.golang.org` every time — `ollama-mesh-gomod`
(`/go/pkg/mod`) and `ollama-mesh-gobuild` (`/root/.cache/go-build`). Both are pure caches, safe
to delete anytime (`docker volume rm ollama-mesh-gomod ollama-mesh-gobuild`) — gate.sh
repopulates them from scratch on the next run. If disk usage looks high and these are candidates,
tell Anirudh rather than deleting unprompted. See `.local/core/LESSONS.md` L8 for why the mount
path matters (must match `go env GOMODCACHE`/`GOCACHE` for the base image in use, not assumed
`/root/go/...`).


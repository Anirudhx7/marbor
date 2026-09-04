# Backup & Restore

marbor is DB-first: `marbor.db` (SQLite) holds nodes, API keys, routing rules, warm-state
history, encrypted secrets, and every setting. Losing it means reconfiguring marbor from
scratch. This page covers taking backups and restoring one, both built into the dashboard's
Settings > Backup & Restore card.

---

## Taking backups

- **Download Backup Now** - takes an on-demand, point-in-time-consistent copy of `marbor.db` (via
  SQLite's `VACUUM INTO`, safe to run while marbor keeps serving requests) and downloads it
  straight to your browser as `marbor-backup-<UTC timestamp>.db`.
- **Scheduled Backup** - enable it, then set:
  - **Interval (hours)** - how often a backup runs.
  - **Retention (backups kept)** - how many scheduled backup files to keep; older ones are
    deleted automatically after each successful run. Manually-downloaded backups (via the button
    above) are never touched by retention pruning - only the scheduler's own files count.
  - **Target Directory** - where scheduled backups are written on the machine running the marbor
    process. In Docker, this defaults to `/backups`, a *separate* named volume from the one
    holding `marbor.db` (see `docker-compose.yml`) - so deleting the container, or even
    `docker volume rm`-ing the data volume, doesn't take the backups down with it. On bare metal /
    systemd, it defaults to a `backups/` directory next to `marbor.db` unless you set `MARBOR_BACKUP_DIR`
    or change it in Settings.

The last scheduled-run outcome (timestamp of the last success, or the last error) is shown on the
same Settings card - a failed scheduled backup (bad path, full disk, permissions) is never
silently swallowed; it's retried on the next check and the error stays visible until a run
succeeds.

---

## Restoring a backup

The Settings > Backup & Restore card lists every scheduled backup file already sitting in the
target directory in a dropdown - pick one and click **Restore** next to it. Behind that click:

1. Marbor validates the file (`PRAGMA quick_check`) before touching anything - a corrupt or
   unrelated file is rejected here, before the live database is ever at risk.
2. It gracefully drains in-flight requests (the same shutdown path a `SIGTERM` uses).
3. It stages the full copy alongside the live `marbor.db` first, and only swaps it in via an atomic
   rename once the copy is proven complete - a failure mid-copy leaves the *live* database
   untouched, never truncated or half-written.
4. It exits the process on purpose, with a non-zero status.

**That last step is why this requires your deployment to actually restart the process on exit** -
marbor does not restart itself; it relies on whatever already supervises it:

| Deployment | Requirement | Status in this repo |
|---|---|---|
| Docker / Docker Compose | a `restart` policy | `docker-compose.yml` ships with `restart: unless-stopped` |
| systemd | `Restart=on-failure` (or `always`) | already in the example unit in [`PRODUCTION.md`](PRODUCTION.md) |
| Kubernetes | default `restartPolicy` | `Always` by default for a Deployment - nothing extra needed |
| A bare `./marbor` with no supervisor | none available | **will not come back on its own** - see below |

If you're running the binary directly with nothing supervising it (no systemd unit, no Docker, no
Kubernetes), a one-click restore will stop marbor and it will stay stopped until you start it
again by hand. Either run it under one of the supervised setups above, or use the fully manual
procedure below instead, where you control every step yourself.

### Restoring from a file that isn't already on the server

Next to the dropdown is a **+** button - it opens your browser's normal file picker (works the
same in Chrome, Firefox, Safari, and Edge, on Linux, Windows, and macOS) so you can attach any
`.db` file from your own machine, not just files the scheduler already produced on the server.
Pick a file and it uploads to the marbor, which validates it's a genuine SQLite database
(`PRAGMA quick_check` - the identical check every other backup path already goes through) and
saves it into the same target directory as scheduled/manual backups, under the standard
`marbor-backup-<timestamp>.db` name. It then appears in the dropdown like any other backup and can
be restored the same way. A spinner replaces the **+** icon while the upload is in flight; an
invalid file (wrong format, corrupt, not a marbor.db backup at all) is rejected with an error and
never touches the target directory.

### Fully manual alternative

Restore doesn't have to go through the dashboard. Stopping the marbor, swapping the file, and
starting it again works identically and isn't restricted to files in the scheduled backup
directory - **any** `.db` file that was itself produced by a backup of this marbor (scheduled,
manual download, or a straight file copy taken while the process was stopped) can be restored
from, regardless of where it currently lives.

**Bare metal / systemd:**

```bash
sudo systemctl stop marbor
sudo cp /path/to/marbor-backup-20260730-140000.db /opt/marbor/marbor.db
sudo systemctl start marbor
```

Replace `/opt/marbor/marbor.db` with whatever path you actually run with (`--db` flag or
`MARBOR_DB_PATH`) - not necessarily this exact path.

**Docker / Docker Compose:**

```bash
docker compose stop marbor
docker cp ./marbor-backup-20260730-140000.db marbor:/data/marbor.db
docker compose start marbor
```

If the file is already inside the container (e.g. a scheduled backup under `/backups`), copy it
inside instead of using `docker cp`:

```bash
docker compose stop marbor
docker compose run --rm --entrypoint sh marbor -c "cp /backups/marbor-backup-20260730-140000.db /data/marbor.db"
docker compose start marbor
```

Either way, the container must be **stopped** first, since SQLite's WAL sidecar files
(`marbor.db-wal`, `marbor.db-shm`) would otherwise still be attached to the old database.

**Kubernetes:** scale the deployment to 0 replicas, copy the backup file onto the PVC (e.g. via a
temporary debug pod with the same PVC mounted, or `kubectl cp` into that pod), then scale back to 1.

---

## Downgrading after an upgrade

**Downgrade is not automatically supported.** If you upgrade marbor and hit a problem, the only
real path back to an older binary is restoring a backup taken *before* the upgrade - there is no
in-place backward migration.

Why: `marbor.db` carries a `schema_version` setting, stamped on every successful startup by the
binary that opened it. `migrate()` in `internal/store/sqlite.go` is forward-only and refuses to
start if the stored `schema_version` is newer than the one the running binary understands
(`internal/store/sqlite.go:158-160`):

```
marbor.db schema_version 2 is newer than this binary supports (1) - refusing to start;
upgrade the binary or restore an older marbor.db backup
```

This is deliberate, not a bug - it exists specifically to stop an older binary from silently
running unmigrated logic against a database a newer release already wrote to. There is no code
path that lets an older binary "downgrade" a database in place; the schema check fails closed and
the process exits without touching the file.

**Tested** (`internal/store/schema_version_test.go`'s `TestOpenRefusesNewerSchemaVersion`, and a
manual end-to-end drill: create a DB, back it up, stamp it with a schema version one ahead of
`CurrentSchemaVersion` to stand in for "a newer release already touched this file," confirm the
same binary refuses to reopen it with the exact error above - no crash, no silent corruption -
then copy the pre-upgrade backup back over the live path and confirm the binary opens it cleanly
with all data intact). Both directions behave as documented.

### The procedure

**Before upgrading**, always take a backup first (see "Taking backups" above) - this is the thing
that makes downgrade possible at all:

- `marbor.db` itself (via **Download Backup Now**, a scheduled backup, or a plain file copy taken
  while the process is stopped).
- `marbor.db.key` alongside it, or your `MARBOR_ENCRYPTION_KEY` value recorded somewhere durable -
  see "The encryption key" below. A `marbor.db` backup without its matching key restores as
  unreadable ciphertext for every secret field (cloud provider keys, runtime API keys, marbor-agent
  enrollment tokens).

**If a problem surfaces after upgrading**, roll back using the same manual restore steps as any
other restore (see "Fully manual alternative" above), pointed at your pre-upgrade backup instead of
a later one:

```bash
# bare metal / systemd - swap in both the pre-upgrade db AND its matching key
sudo systemctl stop marbor
sudo cp /path/to/pre-upgrade-marbor-backup.db /opt/marbor/marbor.db
sudo cp /path/to/pre-upgrade-marbor-backup.db.key /opt/marbor/marbor.db.key
sudo systemctl start marbor    # now running the OLDER binary against the OLDER schema
```

The same pattern applies to Docker/Compose and Kubernetes - stop the process, replace both
`marbor.db` and `marbor.db.key`, start the older binary. The dashboard's one-click **Restore** also
works for this (it validates and swaps `marbor.db` the same way) but only handles the database file
- if the upgrade rotated or regenerated the key, you still need to manually restore
`marbor.db.key` alongside it.

**What you lose**: exactly what you'd lose restoring any backup - every request, config change, or
node/model state change made between the backup and the restore is gone. This mirrors the
in-flight-request-loss framing in [`LIMITATIONS.md`](LIMITATIONS.md#deployment-topology)'s
Deployment Topology section: marbor is stateless for the routing path, so the blast radius of a
restore is "state since the last backup," not "requests currently in flight only."

---

## The encryption key - back it up like the database

Secrets stored in `marbor.db` (cloud provider API keys, marbor-issued runtime API keys,
marbor-agent enrollment tokens, LiteLLM/HuggingFace/webhook credentials) are encrypted at rest
with AES-256-GCM under a 32-byte master key. That key is **not stored in the database** - it lives
either in the `MARBOR_ENCRYPTION_KEY` environment variable or, if that's unset, in a sibling file
next to the database named `<db path>.key` (e.g. `marbor.db.key`), generated automatically on
first boot. A `.db` backup without its matching key is a backup of unreadable ciphertext for every
secret field.

**Back up `marbor.db.key` with the same discipline as `marbor.db` itself** - alongside it in the
same backup, not left behind on the original host. If you set `MARBOR_ENCRYPTION_KEY` instead of
relying on the auto-generated file, that variable's value is the thing to store safely (a secrets
manager, a password vault) - it is exactly as sensitive as the plaintext secrets it protects, and
losing it is exactly as final as losing the key file.

### What happens if the key is lost

There is no recovery. AES-256-GCM with a lost key is unrecoverable by design - that's what
"encrypted at rest" means. If you have lost both `marbor.db.key` and never had `MARBOR_ENCRYPTION_KEY`
set to a saved value, every previously-encrypted secret in that database is gone permanently. There
is no backdoor, no re-derivation, no support path that gets it back.

What actually happens at the next boot depends on how the key went missing:

- **The key file was deleted, or an env-var-only deployment lost the env var, and no `.db.key` file
  ever existed** - marbor currently does **not** detect this and refuse to start. It generates a
  brand-new random key and writes it to `marbor.db.key`, then boots normally with no warning. Every
  secret encrypted under the old key becomes permanently unreadable under the new one, one field at
  a time, the next time each is used:
  - A single-value read (e.g. the LiteLLM API key setting) fails outright with a decrypt error.
  - A list read (marbor agents, runtime API keys, cloud provider keys, other settings) does not fail
    the whole list - the specific row with the undecryptable secret is silently dropped from the
    result and logged (`store: AllX: dropping ...: wrong key or corrupt data ...`), while every other
    row keeps working. A dropped marbor agent or cloud provider effectively disappears from its list
    until you re-enter its credential; a dropped runtime API key stops authenticating.
  - **marbor itself still boots and keeps routing/proxying traffic** - key loss degrades specific
    secret-bearing features, it does not brick the process or the database.
  - This silent-regeneration path is a confirmed gap, not intended behavior - see
    "Known gap" below.
- **The key file exists but is corrupted or truncated** (not exactly 32 bytes) - marbor refuses to
  start and exits with an explicit error naming the file and its wrong size. This is the safe
  behavior; it exists today.

### Known gap - silent key regeneration on loss

Verified in `internal/store/secretbox.go`'s `loadOrCreateSecretKey`: on a missing key file (deleted,
or never written because `MARBOR_ENCRYPTION_KEY` was previously used and is now unset), the function
cannot distinguish "this is a fresh install with nothing encrypted yet" from "the real key was lost
and the database already holds ciphertext" - it treats both cases identically and silently generates
a new key. The corrupted-key-file case already refuses to start instead; the missing-key case should
arguably do the same whenever the database already contains encrypted rows, but doing so needs a
schema-aware check across every secret-bearing table before the key is finalized, which is more than
a one-line fix. This is filed as a confirmed follow-up, not fixed in this pass - the answer above
describes the codebase's actual current behavior, not the behavior it should ideally have.

### Prevention

- Treat `marbor.db.key` as part of the database for backup purposes - same schedule, same target
  directory, same restore drill. The scheduled/manual backup flows above copy `marbor.db` only; back
  up `marbor.db.key` yourself alongside it (it's a static 32-byte file, no `VACUUM INTO` needed - a
  plain file copy is safe at any time).
  - Docker: it lives in the same volume as `marbor.db` unless you've relocated `MARBOR_DB_PATH` -
    include it in whatever backs up that volume.
  - systemd/bare metal: it sits next to `marbor.db` on disk - a filesystem-level backup of that
    directory already includes it.
- If you use `MARBOR_ENCRYPTION_KEY` instead, record its value in a secrets manager or password
  vault with the same durability guarantees you'd want for any other production credential - it
  never touches disk, so a filesystem backup will not save it for you.
- When restoring a `marbor.db` backup onto a new host, restore `marbor.db.key` (or set
  `MARBOR_ENCRYPTION_KEY` to the matching value) to the same directory *before* starting marbor -
  otherwise the missing-key path above kicks in and the restored secrets are immediately orphaned
  under a freshly generated key.

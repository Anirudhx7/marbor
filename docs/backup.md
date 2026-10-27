# Backup & Restore

marbor is DB-first: `marbor.db` (SQLite) holds nodes, API keys, routing rules, warm-state
history, encrypted secrets, and every setting. Losing it means reconfiguring marbor from
scratch. This page covers taking backups and restoring one, both built into the dashboard's
Settings > Backup & Restore card.

---

## Taking backups

- **Download Backup Now** - takes an on-demand, point-in-time-consistent copy of `marbor.db` (via
  SQLite's `VACUUM INTO`, safe to run while marbor keeps serving requests), compresses it with
  gzip as it streams, and saves it to your computer as `marbor-backup-<UTC timestamp>.db.gz`.
  Nothing is held in memory on the server. The dashboard prepares the backup first, which takes
  longer than a browser keeps a click valid, so it never opens a save dialog for this: the file is
  collected in browser memory (refused above 512 MiB, a limit chosen to keep a tab from crashing,
  checked from the declared size before anything is read) and saved to the downloads folder. If the
  save is cancelled or does not finish, the status line says the backup was not saved to this
  computer. An error (expired session, refused or failed backup) is always shown on the card; the
  download never saves an error page as a backup. For a larger database, or to choose where the
  file goes, run `marbor backup now [--output file] [--force]` in a terminal: it streams to disk
  through a temporary file and never replaces an existing file unless you pass `--force`. Stored
  backups (see below) can be downloaded through the save dialog at any size.
- **Scheduled Backup** - enable it, then set:
  - **Interval (hours)** - how often a backup runs.
  - **Retention (backups kept)** - how many backup files to keep in the target directory; older
    ones are deleted automatically after each successful run. Plain `.db` files from earlier
    releases and compressed `.db.gz` files count together. On-demand downloads are not stored on
    the server, so retention never touches them. Files you upload (see below) have their own
    budget of the same size, so uploading never evicts a scheduled backup and a scheduled run never
    deletes an upload you are about to restore. A file that fails the integrity check is flagged
    in the list and kept; marbor never deletes a file it cannot read.
  - **Target Directory** - where scheduled backups are written on the machine running the marbor
    process. In Docker, this defaults to `/backups`, a *separate* named volume from the one
    holding `marbor.db` (see `docker-compose.yml`) - so deleting the container, or even
    `docker volume rm`-ing the data volume, doesn't take the backups down with it. On bare metal /
    systemd, it defaults to a `backups/` directory next to `marbor.db` unless you set `MARBOR_BACKUP_DIR`
    or change it in Settings.

### How a backup is written

1. marbor checks free space first, on every volume the run writes to. The snapshot is staged
   uncompressed (about the size of the live database) before it is compressed, so the volume that
   holds it needs roughly one database of free room plus the compressed output, plus room for the
   write-ahead log to grow while the snapshot runs (planned at 10 percent of the database, never
   less than 64 MiB, or the log's current size if that is larger). A margin is kept spare on top: at
   least 128 MiB, 5 percent of the database, or 1 percent of the volume (that part capped at 4 GiB),
   whichever is largest, so a backup never pushes the volume holding `marbor.db` to zero. If there
   is not enough, the run is **refused** with a message that says how much is needed and how much
   is free; the live database is not touched. A scheduled run that fails or is refused is retried
   after an hour, not every minute.
2. The snapshot is taken with `VACUUM INTO` into a temporary file, streamed through gzip into
   `marbor-backup-<UTC timestamp>.db.gz.tmp`, synced, **read back in full and verified** (the gzip
   checksums, the database header and the fingerprint below must all agree), and only then renamed
   into place. A file that fails the read-back is deleted and the run is reported as failed, so a
   backup that exists in the directory has been read end to end once. The read-back adds time to
   every run (measured below). A run that dies leaves its temporary files behind; marbor removes
   them the next time it starts.
3. Every compressed backup ends with a tiny extra gzip member holding a SHA-256 fingerprint of the
   database content. A scheduled run compares it with the fingerprints already in the directory and
   keeps nothing new when the database has not changed since the last backup. The fingerprint at
   the end of a file only nominates a candidate: a file is treated as a duplicate only after it has
   been read in full and its content fingerprint recomputed, so a damaged or tampered file is never
   mistaken for a good copy of the current database. `gunzip` and any gzip reader ignore the extra
   member and give you back the plain database. Plain `.db` files from earlier releases carry no
   fingerprint and are never used to skip a scheduled run.
4. Backup files are created `0600` and the directory `0700`. If the directory already exists with
   wider permissions, marbor warns about it on the Settings card and in `marbor backup list` but
   does not change it. On Windows these permission bits have no effect, so the directory warning
   is not shown there either; restrict the backup directory with NTFS permissions instead, since a
   backup contains API keys and session data.

The only compression format is gzip (`.db.gz`), chosen because every machine can open it with
`gunzip` and marbor stays a single binary with no extra libraries. Plain `.db` backups from earlier
releases stay listed, restorable and counted by retention.

**Impact on a running marbor.** `VACUUM INTO` holds one read transaction for the whole snapshot. In
SQLite's WAL mode that read cannot be interrupted, so the write-ahead log keeps growing while the
snapshot runs and cannot be folded back into the database until it ends. Writes keep working; the
log is just larger for the duration. marbor checkpoints and truncates the log right after the
snapshot. Plan for the log to grow by the amount of data written during the run.

Measured on synthetic request-log rows (about 170 bytes of data per row on disk), one run per cell, the
backup written to the same disk as the database. "Writer" means a second connection inserting a
small row every few milliseconds for the whole run (and, for the table, its latency). Windows is
the native Windows build; Linux is the same code cross-compiled and run under WSL2 (8 CPUs, ext4).

| Platform | Rows (database) | Writer | Snapshot and compress | Read-back | Total | Backup size | Peak memory | Peak log |
|---|---|---|---|---|---|---|---|---|
| Windows | 1,000,000 (162 MiB) | no | 5.7 s | 0.5 s | 6.2 s | 23.9 MiB (6.6x) | 34 MiB | none |
| Windows | 1,000,000 | yes | 5.4 s | 0.5 s | 5.9 s | 23.9 MiB (6.6x) | 33 MiB | 3.9 MiB |
| Windows | 10,000,000 (1.60 GiB) | no | 71.2 s | 5.2 s | 76.4 s | 242.5 MiB (6.55x) | 38 MiB | none |
| Windows | 10,000,000 | yes | 64.5 s | 5.2 s | 69.7 s | 242.5 MiB (6.55x) | 38 MiB | 31.7 MiB |
| Linux | 1,000,000 | no | 9.2 s | 0.6 s | 9.8 s | 23.9 MiB (6.6x) | 34 MiB | none |
| Linux | 1,000,000 | yes | 5.6 s | 0.5 s | 6.0 s | 23.9 MiB (6.6x) | 33 MiB | 3.9 MiB |
| Linux | 10,000,000 | yes | 104.2 s | 6.3 s | 110.5 s | 242.5 MiB (6.55x) | 37 MiB | 51.8 MiB |

Writes during the run (the writer rows above):

| Platform | Rows | Writes | Median | 99th percentile | Slowest |
|---|---|---|---|---|---|
| Windows | 1,000,000 | 1,793 | below the 1 ms clock tick | 0.5 ms | 9.8 ms |
| Windows | 10,000,000 | 19,770 | below the 1 ms clock tick | 0.5 ms | 18.7 ms |
| Linux | 1,000,000 | 1,809 | 0.16 ms | 0.4 ms | 56 ms |
| Linux | 10,000,000 | 29,929 | 0.17 ms | 1.6 ms | 267 ms |

Restore of the same backups (unpack and validate into the staged file, then the full check marbor
runs again once the live database is closed):

| Platform | Rows | Unpack and validate | Full check at swap |
|---|---|---|---|
| Windows | 1,000,000 | 1.5 s (no writer), 1.7 s | 0.5 s |
| Windows | 10,000,000 | 30.9 s (no writer), 13.5 s | 5.8 s |
| Linux | 1,000,000 | 2.9 s (no writer), 2.2 s | 0.4 s |
| Linux | 10,000,000 | 67.5 s | 4.1 s |

Reading these:

- The read-back verification adds 8 to 9 percent to the run time. Memory did not grow with the
  database: the peak working set of the test process was 33 to 38 MiB at both sizes (the figure
  includes building the test data, so it is an upper bound for the backup).
- The staged uncompressed snapshot plus the compressed output together peaked at 1.12 times the
  database size on disk on Linux (1.92 GB for a 1.72 GB database); plan free space accordingly.
  The pre-run check plans the output as the full size of the snapshot, so an incompressible
  database cannot run the volume out of space mid-run.
- The write-ahead log grew to 3 percent of the database at the highest write rate tested.
- Unpack and validate times for the same file varied by a factor of two between runs on the same
  machine (disk cache state); treat the restore times as a range.
- The swap itself only re-reads the staged file's header when the staged copy is already proven
  unchanged; the "full check" column is what it would cost to repeat the whole validation, and is
  the time a restore saves.
- One machine each, one run per cell, synthetic data; real data compresses differently, and a
  disk that is slower to read back than to write lengthens the verification.

The last outcome of the scheduled and of the on-demand backup are tracked separately (so a failed
download cannot hide a healthy schedule, or the reverse): the time of the last success, or the
failure, refusal or cancellation with its reason. They are shown on the same Settings card and in
`marbor backup list`, together with the last restore result, whether a backup is running, whether
the backup directory exists, any permission warning, a failure to prune old files, leftover
temporary files that could not be removed, and the number of directory entries that could not be
listed. The status is kept in a small file in the backup directory, not in the database, so that a
backup of an unchanged database is byte-for-byte reproducible; if that file cannot be written the
card says so and the status then lives in memory until restart. A failed backup (bad path, full
disk, permissions) is never silently swallowed; the error stays visible until a run succeeds.

### Downloading a stored backup

The restore list has a download button next to the selected file, and from a terminal:

```bash
marbor backup list
marbor backup download marbor-backup-20260730-140000.db.gz --output ./saved.db.gz
```

Both stream the file to disk; the CLI writes through a temporary file and moves it into place
only when the transfer completed, and refuses to replace an existing file without `--force`.
Without `--output`, the file is saved under its own name, which must be a valid backup name. The
Admin API route is `GET /admin/backup/download?name=<file name>`; the name must be a bare
`marbor-backup-<date>-<time>.db`, `.db.gz` or `-upload` variant of those that already exists in the
backup directory. The browser fetches the file through the save dialog where there is one (a transfer that breaks is
discarded, not left as a short file); where there is none, or the click is no longer fresh, it
falls back to memory with the same 512 MiB limit, checked before anything is read. A failure is
shown instead of saving an error page.

To remove a stored backup, use the delete button next to the selected file (it asks first) or
`marbor backup delete <name>` (it asks first); the route is `DELETE /admin/backup?name=<file name>` and answers
`204`, `404` when the file is gone, or `409` while a restore or another operation is using it.
Deleting frees the space for the next run and, like retention, never touches a file that is being
read.

A transfer that stalls is cut off: the server gives up on a client that sends or reads nothing
for two minutes (the CLI gives up an upload that makes no progress for the same time), while a
slow but steady transfer is never cut short. Ctrl-C during a CLI download removes the partial
file. If the final directory sync fails after the file is in place, the CLI prints a warning and
still exits 0, because the file is complete.

### What "ok" means, and when it is re-checked

In `marbor backup list`, "ok" means the gzip checksums, the SQLite header and the trailing
fingerprint all agree. It is not a full restore test: a restore runs the full database integrity
check again after the live database is closed. A good verification is remembered for 24 hours in
memory only, keyed on the file's identity so a replaced file is never trusted; because it is not
stored, every file is read again after marbor restarts, and older results are re-verified in the
background once a day. A damaged file is
flagged, kept, and never counted toward retention. `marbor backup list --check` exits 3 when any
file is flagged or unverified, the last scheduled or on-demand backup failed, was refused or was
cancelled, a retention or leftover-file problem exists, entries or the whole directory could not be
scanned, a backup has been running for over an hour, the last restore failed or finished with a
note, there are no backups, or the scheduler is enabled but no backup has succeeded within its
interval plus an hour (or none ever has), so a cron job can alert on it. The status carries
`schedule_enabled`, `schedule_interval_hours` and `running_since` for that purpose.

### Space planning, upload pool and limits

- The pre-run check plans the compressed output as the full size of the snapshot, the worst case
  for data that does not compress (measured compression here was 6.6x, with a peak of about 1.12x
  the database on disk), on top of the staged
  snapshot, the log growth allowance and the margin described above. Concurrent runs, uploads and
  restores reserve against one shared ledger, so two of them cannot both claim the same free space.
- Uploaded files live in their own pool with the same retention count as scheduled backups, so up
  to twice the configured number of files can be on disk. A prune failure in that pool is reported
  separately.
- At most 2 manual backups run at once, on top of the scheduled one (`429` beyond that). An upload
  that falls below 64 KiB/s is cut off, and every upload also has a total deadline. An upload
  reserves its space as it arrives and a run reserves the output it is writing, so growth during
  the transfer is accounted for; the `warning` field of the upload answer carries a non-fatal
  problem, such as a directory that could not be flushed.
- Deadlines on the command line: `marbor backup now` waits up to 30 minutes for the server to start
  answering, a stored download 2 minutes, and an upload waits for the server's verdict for
  `max(15 minutes, size / 8 MiB/s)` after the last byte is sent.

---

## Restoring a backup

The Settings > Backup & Restore card lists every scheduled backup file already sitting in the
target directory in a dropdown - pick one and click **Restore** next to it. Behind that click:

1. Before answering the request, marbor validates the file (`PRAGMA quick_check`) and, for a
   `.db.gz` backup, decompresses it once, reading it end to end (the gzip checksums catch a
   truncated or damaged file) under a size cap of the free space on that volume, so a gzip bomb or
   a full disk fails here. The result is staged next to the live database as
   `marbor.db.restoring-<id>`. Only one restore can be staged at a time; a second request gets `409`.
   A restore of an in-memory database is refused with `501`.
   A corrupt or unrelated file is rejected here, with an error on the card, before the live database
   is ever at risk. Free space is checked first on every volume involved; a compressed backup needs
   room for one full-size copy next to the live database, and the refusal names the amount.
2. Only after the staged copy is complete does marbor answer `202`. It then gracefully drains
   in-flight requests (the same shutdown path a `SIGTERM` uses).
3. Once the live database is closed, the process validates the staged file again in full, refuses
   to swap if an old write-ahead log still holds data, swaps the staged file in via an atomic
   rename, and removes the old log and shared-memory files only after the rename. When the staged
   file was already fully checked and is proven unchanged, only its header is re-read at this point,
   so the swap stays quick for a large database. A failure before
   the swap leaves the *live* database untouched, never truncated or half-written. A stale
   `marbor.db.restoring-*` from an interrupted restore is removed at the next start.
4. It exits the process on purpose, with a non-zero status, and writes a small outcome marker that
   the next start reads, so the card can show whether the last restore succeeded or failed. If
   staging or the swap fails after the servers have stopped, the process still exits (it cannot
   serve without its store); a supervisor restarts it onto the old database, and without a
   supervisor it stays down until you start it. A pending marker is written before the restart,
   so the next start reports "restore pending" or the final outcome. A failed or ambiguous outcome
   stays on the card (and in `marbor backup list`) until you acknowledge it with the dashboard's
   dismiss button or `marbor backup ack-restore`. Each restore has a 16-character reference, shown
   with the result; the acknowledgement must carry the reference it read (`?ref=`, or `-` for a
   result without one), and an acknowledgement of a result that has since been replaced is refused
   with `409`. Dismissing clears the result for every operator and for the command line.

**Following a restore.** `POST /admin/backup/restore` answers `{restarting, file, ref}`. `/health`
carries a `started_at` identifier of the running process, so a client tells a restarted marbor from
the old one still shutting down; a `503` from `/health` is a running (degraded) marbor, not a down
one. `marbor backup restore <file> --wait` follows it that way (up to 5 minutes). The dashboard does the same from a watcher
that survives leaving the page or reloading the tab, then reads the stored result for that
reference before reloading. If marbor is back but its session is gone, the restored database is
the one in use and the result could not be read: the page says it could not confirm the outcome
instead of claiming success. A failed restore keeps the old database, so the session still works
and the failure is shown with its reason.

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
`.db` or `.db.gz` file from your own machine, not just files the scheduler already produced on the
server. The browser checks the extension and the 2 GiB size limit first, so a wrong or oversized
file is turned away before it is sent. Pick a file and it uploads to the marbor, which recognises
plain or gzip by content (not by file name), reads it in full, validates it's a genuine SQLite
database (`PRAGMA quick_check` - the identical check every other backup path already goes through)
and saves it into the same target directory as scheduled backups, under a
`marbor-backup-<timestamp>-upload.db` or `-upload.db.gz` name; the `-upload` marker is how the list
tells your files from ones marbor made. The CLI equivalent is
`marbor backup upload --file <path>`; both stream the file, nothing is buffered whole. While an
upload runs the dashboard does not start a stored download, and the reverse, and says why. It then
appears in the dropdown like any other backup, marked as uploaded, and can be restored the same
way. A spinner replaces the **+** icon while the upload is in flight; an invalid file (wrong
format, corrupt, not a marbor.db backup at all) is rejected with an error and never touches the
target directory. An upload byte-for-byte identical to a file already stored is not added twice:
the existing file is selected and the card says so.

### Fully manual alternative

Restore doesn't have to go through the dashboard. Stopping the marbor, swapping the file, and
starting it again works identically and isn't restricted to files in the scheduled backup
directory - **any** `.db` file that was itself produced by a backup of this marbor (scheduled,
manual download, or a straight file copy taken while the process was stopped) can be restored
from, regardless of where it currently lives.

A compressed backup is unpacked first with `gunzip -k marbor-backup-20260730-140000.db.gz`
(or `gzip -dc file.db.gz > marbor.db`); everything below then works on the resulting `.db`.

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

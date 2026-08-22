# Backup & Restore

marbor is DB-first: `marbor.db` (SQLite) holds nodes, API keys, routing rules, warm-state
history, encrypted secrets, and every setting. Losing it means reconfiguring the marbor from
scratch. This page covers taking backups and restoring one, both built into the dashboard's
Settings > Backup & Restore card.

---

## Taking backups

- **Download Backup Now** - takes an on-demand, point-in-time-consistent copy of `marbor.db` (via
  SQLite's `VACUUM INTO`, safe to run while the marbor keeps serving requests) and downloads it
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

1. The marbor validates the file (`PRAGMA quick_check`) before touching anything - a corrupt or
   unrelated file is rejected here, before the live database is ever at risk.
2. It gracefully drains in-flight requests (the same shutdown path a `SIGTERM` uses).
3. It stages the full copy alongside the live `marbor.db` first, and only swaps it in via an atomic
   rename once the copy is proven complete - a failure mid-copy leaves the *live* database
   untouched, never truncated or half-written.
4. It exits the process on purpose, with a non-zero status.

**That last step is why this requires your deployment to actually restart the process on exit** -
the marbor does not restart itself; it relies on whatever already supervises it:

| Deployment | Requirement | Status in this repo |
|---|---|---|
| Docker / Docker Compose | a `restart` policy | `docker-compose.yml` ships with `restart: unless-stopped` |
| systemd | `Restart=on-failure` (or `always`) | already in the example unit in [`PRODUCTION.md`](PRODUCTION.md) |
| Kubernetes | default `restartPolicy` | `Always` by default for a Deployment - nothing extra needed |
| A bare `./marbor` with no supervisor | none available | **will not come back on its own** - see below |

If you're running the binary directly with nothing supervising it (no systemd unit, no Docker, no
Kubernetes), a one-click restore will stop the marbor and it will stay stopped until you start it
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

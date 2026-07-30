# Backup & Restore

ollama-mesh is DB-first: `mesh.db` (SQLite) holds nodes, API keys, routing rules, warm-state
history, encrypted secrets, and every setting. Losing it means reconfiguring the mesh from
scratch. This page covers taking backups (built into the dashboard) and restoring one (a manual
procedure, deliberately not a dashboard button - see [Why restore isn't a button](#why-restore-isnt-a-button)).

---

## Taking backups

Settings > Backup & Restore, in the dashboard:

- **Download Backup Now** - takes an on-demand, point-in-time-consistent copy of `mesh.db` (via
  SQLite's `VACUUM INTO`, safe to run while the mesh keeps serving requests) and downloads it
  straight to your browser as `mesh-backup-<UTC timestamp>.db`.
- **Scheduled Backup** - enable it, then set:
  - **Interval (hours)** - how often a backup runs.
  - **Retention (backups kept)** - how many scheduled backup files to keep; older ones are
    deleted automatically after each successful run. Manually-downloaded backups (via the button
    above) are never touched by retention pruning - only the scheduler's own files count.
  - **Target Directory** - where scheduled backups are written on the machine running the mesh
    process. In Docker, this defaults to `/backups`, a *separate* named volume from the one
    holding `mesh.db` (see `docker-compose.yml`) - so deleting the container, or even
    `docker volume rm`-ing the data volume, doesn't take the backups down with it. On bare metal /
    systemd, it defaults to a `backups/` directory next to `mesh.db` unless you set `MESH_BACKUP_DIR`
    or change it in Settings.

The last scheduled-run outcome (timestamp of the last success, or the last error) is shown on the
same Settings card - a failed scheduled backup (bad path, full disk, permissions) is never
silently swallowed; it's retried on the next check and the error stays visible until a run
succeeds.

---

## Restoring a backup

Restore is **not** a dashboard action - it means stopping the mesh process, replacing the live
database file, and starting it again. There is no restriction to files under the scheduled
backup's target directory (`/backups` in Docker, or wherever you configured): **any** `.db` file
that was itself produced by a backup of this mesh (scheduled, manual download, or a straight file
copy taken while the process was stopped) can be restored from, regardless of where it currently
lives - a different disk, a USB drive, an email attachment you saved locally, wherever you put it.

### Bare metal / systemd

```bash
sudo systemctl stop ollama-mesh
sudo cp /path/to/mesh-backup-20260730-140000.db /opt/ollama-mesh/mesh.db
sudo systemctl start ollama-mesh
```

Replace `/opt/ollama-mesh/mesh.db` with whatever path you actually run with (`--db` flag or
`MESH_DB_PATH`) - not necessarily this exact path.

### Docker / Docker Compose

```bash
docker compose stop ollama-mesh
# Copy a backup file (from anywhere on the host, not just the mesh-backups volume)
# into the running container's data volume:
docker cp ./mesh-backup-20260730-140000.db ollama-mesh:/data/mesh.db
docker compose start ollama-mesh
```

If the backup file is already inside the container (e.g. a scheduled backup under `/backups`),
skip `docker cp` and just do the copy inside the container instead:

```bash
docker compose stop ollama-mesh
docker compose run --rm --entrypoint sh ollama-mesh -c "cp /backups/mesh-backup-20260730-140000.db /data/mesh.db"
docker compose start ollama-mesh
```

Either way, the container must be **stopped** (not just the mesh process paused) before
overwriting `/data/mesh.db`, since SQLite's WAL sidecar files (`mesh.db-wal`, `mesh.db-shm`) would
otherwise still be attached to the old database and need to be removed or reconciled - a clean
stop/start avoids that entirely.

### Kubernetes

Same idea against the PVC-backed `/data` mount: scale the deployment to 0 replicas, copy the
backup file onto the PVC (e.g. via a temporary debug pod with the same PVC mounted, or `kubectl cp`
into that pod), then scale back to 1.

---

## Why restore isn't a button

A "Restore" action in the admin UI would have to either fake success before the actual file swap
happens, or leave the browser talking to a process that's mid-shutdown while the swap occurs -
both are worse UX than a short, explicit runbook. The admin server can't safely bring down its own
host process cleanly from inside a request handler serving that same request. Stopping the process
first, swapping the file, then starting it again is the safe order every time, and it's the same
three steps regardless of how the mesh is deployed.

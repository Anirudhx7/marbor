# Backup & Restore

ollama-mesh is DB-first: `mesh.db` (SQLite) holds nodes, API keys, routing rules, warm-state
history, encrypted secrets, and every setting. Losing it means reconfiguring the mesh from
scratch. This page covers taking backups and restoring one, both built into the dashboard's
Settings > Backup & Restore card.

---

## Taking backups

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

The Settings > Backup & Restore card lists every scheduled backup file already sitting in the
target directory - pick one and click **Restore**. Behind that click:

1. The mesh validates the file (`PRAGMA quick_check`) before touching anything - a corrupt or
   unrelated file is rejected here, before the live database is ever at risk.
2. It gracefully drains in-flight requests (the same shutdown path a `SIGTERM` uses).
3. It stages the full copy alongside the live `mesh.db` first, and only swaps it in via an atomic
   rename once the copy is proven complete - a failure mid-copy leaves the *live* database
   untouched, never truncated or half-written.
4. It exits the process on purpose, with a non-zero status.

**That last step is why this requires your deployment to actually restart the process on exit** -
the mesh does not restart itself; it relies on whatever already supervises it:

| Deployment | Requirement | Status in this repo |
|---|---|---|
| Docker / Docker Compose | a `restart` policy | `docker-compose.yml` ships with `restart: unless-stopped` |
| systemd | `Restart=on-failure` (or `always`) | already in the example unit in [`PRODUCTION.md`](PRODUCTION.md) |
| Kubernetes | default `restartPolicy` | `Always` by default for a Deployment - nothing extra needed |
| A bare `./ollama-mesh` with no supervisor | none available | **will not come back on its own** - see below |

If you're running the binary directly with nothing supervising it (no systemd unit, no Docker, no
Kubernetes), a one-click restore will stop the mesh and it will stay stopped until you start it
again by hand. Either run it under one of the supervised setups above, or use the fully manual
procedure below instead, where you control every step yourself.

### Fully manual alternative

Restore doesn't have to go through the dashboard. Stopping the mesh, swapping the file, and
starting it again works identically and isn't restricted to files in the scheduled backup
directory - **any** `.db` file that was itself produced by a backup of this mesh (scheduled,
manual download, or a straight file copy taken while the process was stopped) can be restored
from, regardless of where it currently lives.

**Bare metal / systemd:**

```bash
sudo systemctl stop ollama-mesh
sudo cp /path/to/mesh-backup-20260730-140000.db /opt/ollama-mesh/mesh.db
sudo systemctl start ollama-mesh
```

Replace `/opt/ollama-mesh/mesh.db` with whatever path you actually run with (`--db` flag or
`MESH_DB_PATH`) - not necessarily this exact path.

**Docker / Docker Compose:**

```bash
docker compose stop ollama-mesh
docker cp ./mesh-backup-20260730-140000.db ollama-mesh:/data/mesh.db
docker compose start ollama-mesh
```

If the file is already inside the container (e.g. a scheduled backup under `/backups`), copy it
inside instead of using `docker cp`:

```bash
docker compose stop ollama-mesh
docker compose run --rm --entrypoint sh ollama-mesh -c "cp /backups/mesh-backup-20260730-140000.db /data/mesh.db"
docker compose start ollama-mesh
```

Either way, the container must be **stopped** first, since SQLite's WAL sidecar files
(`mesh.db-wal`, `mesh.db-shm`) would otherwise still be attached to the old database.

**Kubernetes:** scale the deployment to 0 replicas, copy the backup file onto the PVC (e.g. via a
temporary debug pod with the same PVC mounted, or `kubectl cp` into that pod), then scale back to 1.

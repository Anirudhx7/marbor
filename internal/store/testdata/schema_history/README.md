# Historical schema fixtures (V1-RELEASE-GATE B.3)

Each `.sql` file here is the literal `migrate()` DDL statement list from a real, tagged past
release of `internal/store/sqlite.go`, extracted with `git show <tag>:internal/store/sqlite.go`
and a small one-off AST tool (not committed - it just walked `migrate()`'s two `[]string{...}`
composite literals and printed each element's literal text). Statements are separated by a
`@@STMT@@` line; `internal/store/historical_migration_test.go` splits on that marker and executes
each statement in order against a fresh SQLite file, so the reconstructed database is the actual
on-disk shape a real binary at that tag would have produced - not a hand-written guess at what it
might have looked like.

| File | Tag | Why this point |
|---|---|---|
| `v0.11.0.sql` | v0.11.0 | Earliest tag with a persisted schema at all (`internal/store/sqlite.go` didn't exist before v0.11.0) - 9 tables, no `ALTER TABLE`s yet. |
| `v0.14.0.sql` | v0.14.0 | Mid-early: 20 tables, 2 `ALTER TABLE`s. |
| `v0.16.0.sql` | v0.16.0 | The last tag before commit `fe3437e` ("encrypt secrets at rest in mesh.db", 2026-07-16) shipped `secretbox.go` - `cloud_providers.api_key` and `runtime_keys.key` are still plaintext here, so this is the one point that genuinely exercises `migrateEncryptSecrets()`'s plaintext-to-ciphertext upgrade path. Also predates the `node_agent`/`marbor_agent` tables entirely. |
| `v0.19.0.sql` | v0.19.0 | Post-secretbox, has the (then-current-named) `node_agent` table - the table commit `19147ea` later renamed to `marbor_agent` in v0.20.0 without migrating any existing rows (see B.3's fix in `sqlite.go`'s `migrateRenameNodeAgentTable`). |
| `v0.20.0.sql` | v0.20.0 | Latest tag at the time this fixture set was built (`HEAD` was `v0.20.0-106-g812e7ea`) - already has the renamed `marbor_agent` table. |

Regenerating these fixtures (e.g. to add a later historical point) does **not** require re-running
the AST tool - `git show <tag>:internal/store/sqlite.go`'s `migrate()` function can be copied by
hand into the same `@@STMT@@`-delimited format; the tool just made the first pass mechanical and
comment-free.

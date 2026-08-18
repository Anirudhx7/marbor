# ollama-mesh CLI Reference

Generated from the CLI command registry (`internal/cli`) by `cmd/gen-docs` - do not edit by hand; run `make docs` after changing the registry.

## Global flags

Every command accepts these in addition to any flags listed under it:

- `--server` - Admin API base URL (default "http://localhost:8080", env MESH_SERVER)
- `--json` - output machine-readable JSON instead of a human table
- `--username` - admin username, used to log in (env MESH_USERNAME)
- `--password` - admin password, used to log in (env MESH_PASSWORD)

## Exit status

- `0` - success
- `1` - user error (bad arguments, unknown command, validation failure)
- `2` - server error (the Admin API is unreachable or returned an unexpected error)
- `3` - reserved for future partial-success reporting (batch operations); unused today
- `4` - authentication error (missing, invalid, or expired credentials)

## Environment

- `MESH_SERVER` - Admin API base URL, used when `--server` is not given
- `MESH_USERNAME` - admin username, used when `--username` is not given
- `MESH_PASSWORD` - admin password, used when `--password` is not given

## Files

The session saved by `ollama-mesh login` (mode `0600`), under the OS user config dir - e.g. `~/.config/ollama-mesh/session` on Linux, `~/Library/Application Support/ollama-mesh/session` on macOS, `%AppData%\ollama-mesh\session` on Windows.

## Commands

### `version`

print CLI and (if reachable) server version

### `status`

print mesh health/status summary

### `login`

authenticate once and save the session locally (recommended)

Authenticates once and saves the resulting session to a local file (0600,
under the OS user config dir) so other commands can omit --username/
--password afterward. Run without --username/--password in a terminal to
be prompted interactively (password input is not echoed).

### `logout`

remove the saved session

### `whoami`

show the CLI's saved identity (live-verified)

### `nodes`

list nodes known to the mesh

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

requires credentials: run "ollama-mesh login" once (recommended), or pass --username+--password (or MESH_USERNAME+MESH_PASSWORD).

#### `confirm-tls <node>`

pin a Node Agent's TLS certificate fingerprint (headless enrollment)

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

Flags:

- `--fingerprint string` - SHA-256 fingerprint the operator has independently confirmed matches the node's actual TLS certificate (see "agent service status" on the node), in the form SHA256:<64 hex characters> (required)

### `models`

fleet-wide list, or pull/delete/unload/list on one node

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

requires credentials: run "ollama-mesh login" once (recommended), or pass --username+--password (or MESH_USERNAME+MESH_PASSWORD).

#### `pull <node> <model>`

start pulling a model onto a node (async - does not wait for completion)

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

#### `delete <node> <model>`

delete a model from a node's local storage

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

#### `unload <node> <model>`

unload a model from a node's warm state

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

#### `list <node>`

list models present on a node's local storage (per-node, not the fleet-wide aggregate above)

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

### `runtime`

start/stop/restart/logs/drain/undrain/health on one node

"start|stop|restart" requires the target node to have an operator-accepted control driver (see "node control accept") - a node with none configured returns an error rather than guessing one.

"logs" is a point-in-time snapshot, not a live tail. A node whose control driver has no real log source (e.g. a bare PID-file process with no supervisor) returns a clear "not supported" error.

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

requires credentials: run "ollama-mesh login" once (recommended), or pass --username+--password (or MESH_USERNAME+MESH_PASSWORD).

#### `start <node>`

start the node's inference runtime process

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

#### `stop <node>`

stop the node's inference runtime process

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

#### `restart <node>`

restart the node's inference runtime process

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

#### `logs <node>`

fetch recent log lines from the node's runtime process

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

Flags:

- `--lines int` - number of log lines to fetch (0 = server default)

#### `drain <node>`

mark the node draining (stop routing new requests to it)

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

Flags:

- `--reason string` - reason recorded for the drain (default "manual")

#### `undrain <node>`

reverse "runtime drain"

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

#### `health <node>`

run an on-demand active liveness probe on the node

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

### `node`

node control driver operations

#### `control`

show or accept a node's control driver

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

##### `probe <node>`

show a node's control-driver status (configured + discovered)

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

##### `accept <node>`

accept a control driver + identifier for a node

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

Flags:

- `--driver string` - control driver: systemd, docker, process, launchd, or windows_service (required)
- `--identifier string` - driver-specific identifier (unit name, container name, PID file path, plist label, service name) (required)
- `--start-command string` - launch command for the process driver's Start action (only meaningful when --driver=process)

### `key`

per-API-key local/cloud routing overrides

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

#### `set-local-only <name> <true|false>`

block (or re-allow) cloud fallback for one API key

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

#### `set-allow-local-degradation <name> <true|false>`

let (or forbid) one API key receive a local alternate model

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

### `spill`

show per-key, per-provider local-vs-cloud request counts

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

requires credentials: run "ollama-mesh login" once (recommended), or pass --username+--password (or MESH_USERNAME+MESH_PASSWORD).

### `requests`

inspect routing decisions for past requests

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

#### `explain <request-id>`

show why the router picked the node it did for one request

Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.

### `completion <shell>`

_Hidden from `--help` output, but fully reachable._

generate a shell completion script (bash, zsh, or fish)

Generates a static completion script for the requested shell by walking
the current command tree. The script never contacts the mesh or
requires credentials, so it keeps working even when the mesh is
unreachable or the operator isn't logged in.

Examples:

```bash
source <(ollama-mesh completion bash)
ollama-mesh completion zsh > "${fpath[1]}/_ollama-mesh"
ollama-mesh completion fish > ~/.config/fish/completions/ollama-mesh.fish
```


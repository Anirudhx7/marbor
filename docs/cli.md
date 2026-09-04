# marbor CLI Reference

Generated from the CLI command registry (`internal/cli`) by `cmd/gen-docs` - do not edit by hand; run `make docs` after changing the registry.

## Global flags

Every command accepts these in addition to any flags listed under it:

- `--server` - Admin API base URL (default "http://localhost:8080", env MARBOR_SERVER)
- `--json` - output machine-readable JSON instead of a human table
- `--username` - admin username, used to log in (env MARBOR_USERNAME)
- `--password` - admin password, used to log in (env MARBOR_PASSWORD)

## Exit status

- `0` - success
- `1` - user error (bad arguments, unknown command, validation failure)
- `2` - server error (the Admin API is unreachable or returned an unexpected error)
- `3` - reserved for future partial-success reporting (batch operations); unused today
- `4` - authentication error (missing, invalid, or expired credentials)

## Environment

- `MARBOR_SERVER` - Admin API base URL, used when `--server` is not given
- `MARBOR_USERNAME` - admin username, used when `--username` is not given
- `MARBOR_PASSWORD` - admin password, used when `--password` is not given

## Files

The session saved by `marbor login` (mode `0600`), under the OS user config dir - e.g. `~/.config/marbor/session` on Linux, `~/Library/Application Support/marbor/session` on macOS, `%AppData%\marbor\session` on Windows.

## Commands

### `version`

print CLI and (if reachable) server version

### `status`

print marbor health/status summary

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

list nodes known to marbor

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

#### `confirm-tls <node>`

pin a marbor agent's TLS certificate fingerprint (headless enrollment)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--fingerprint string` - SHA-256 fingerprint the operator has independently confirmed matches the node's actual TLS certificate (see "agent service status" on the node), in the form SHA256:<64 hex characters> (required)

#### `patch <node>`

set deployment parallelism or per-model VRAM overrides for a node

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--parallelism-type string` - parallelism type: tp, pp, ep, dp (empty to clear)
- `--parallelism-width int` - parallelism width 1..64 (0 to clear)
- `--vram-override string` - per-model VRAM size overrides in MB, comma-separated model=mb pairs - REPLACES the whole declared list, dropping any entry not listed here (empty to clear all)

#### `add <name> <url>`

add (or update, by name) a node in the fleet

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--runtime string` - runtime: ollama (default), vllm, tgi, llamacpp, mlx
- `--gpu-model string` - GPU model label (informational)
- `--vram-total-mb int` - declared total VRAM in MB (0 = unknown)

#### `remove <node>`

remove a node from the fleet

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--yes` - confirm removal without prompting

#### `warmup`

get or set a node's proactive warmup config

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `get <node>`

show a node's proactive warmup config

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `set <node>`

set a node's proactive warmup config

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--enabled` - enable proactive warmup (omit to leave the node's current setting unchanged)
- `--models string` - comma-separated models to keep resident (omit to leave unchanged, pass empty string to clear)

#### `pinned`

get or set a node's never-evict (pinned) model list

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `get <node>`

show a node's pinned model list

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `set <node>`

set a node's pinned model list (whole-list replace)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--models string` - comma-separated models to pin (empty clears all)

#### `prewarm`

disable or re-enable predictive prewarm for a node

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `set <node>`

disable or re-enable predictive prewarm for a node

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--disabled` - disable predictive prewarm for this node

#### `fit`

show per-node VRAM fit analysis for resident/warm models

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `models`

fleet-wide list, or pull/delete/unload/list on one node

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

#### `pull <node> <model>`

start pulling a model onto a node (async - does not wait for completion)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `delete <node> <model>`

delete a model from a node's local storage

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `unload <node> <model>`

unload a model from a node's warm state

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `list <node>`

list models present on a node's local storage (per-node, not the fleet-wide aggregate above)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `fleet`

fleet residency with VRAM totals and drift (same live data as bare models, filterable)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--drifted-only` - only show models where nodes disagree on digest

#### `search`

search Hugging Face models

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--q string` - search query
- `--runtime string` - filter by runtime compatibility
- `--sort string` - downloads (default), likes, newest, or oldest

#### `repo <owner/name>`

show Hugging Face repo detail with per-node fit

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--node string` - node to check fit/downloaded-status against
- `--runtime string` - runtime to size variants for
- `--ctx int` - context window in tokens for VRAM sizing (0 = server default 8192)

#### `pull-progress <node> <model>`

show a point-in-time snapshot of an active pull

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `cancel-pull <node> <model>`

cancel an in-flight pull

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `runtime`

start/stop/restart/logs/drain/undrain/health on one node

"start|stop|restart" requires the target node to have an operator-accepted control driver (see "node control accept") - a node with none configured returns an error rather than guessing one.

"logs" is a point-in-time snapshot, not a live tail. A node whose control driver has no real log source (e.g. a bare PID-file process with no supervisor) returns a clear "not supported" error.

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

#### `start <node>`

start the node's inference runtime process

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `stop <node>`

stop the node's inference runtime process

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `restart <node>`

restart the node's inference runtime process

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `logs <node>`

fetch recent log lines from the node's runtime process

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--lines int` - number of log lines to fetch (0 = server default)

#### `drain <node>`

mark the node draining (stop routing new requests to it)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--reason string` - reason recorded for the drain (default "manual")

#### `undrain <node>`

reverse "runtime drain"

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `health <node>`

run an on-demand active liveness probe on the node

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `node`

node control driver operations

#### `control`

show or accept a node's control driver

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `probe <node>`

show a node's control-driver status (configured + discovered)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `accept <node>`

accept a control driver + identifier for a node

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--driver string` - control driver: systemd, docker, process, launchd, or windows_service (required)
- `--identifier string` - driver-specific identifier (unit name, container name, PID file path, plist label, service name) (required)
- `--start-command string` - launch command for the process driver's Start action (only meaningful when --driver=process)

##### `clear <node>`

clear the accepted control driver for a node

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--yes` - confirm without prompting

#### `agent`

manage marbor agent lifecycle for a node

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

##### `get <node>`

show a node's marbor agent config (does not display the auth token)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `enable <node>`

enable or reconfigure the marbor agent for a node

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--port int` - agent port (required) (required)
- `--scheme string` - http or https (empty = keep existing, or http on first enable)

##### `disable <node>`

disable the marbor agent for a node

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--yes` - confirm without prompting

##### `regenerate <node>`

issue a fresh token for an already-enabled marbor agent

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `key`

per-API-key local/cloud routing overrides (masked list, plaintext-once on create)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `list`

list keys (masked)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `create`

create a key (prints plaintext once)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--name string` - key name (required) (required)
- `--rate-limit int` - max requests per hour (0 = unlimited)
- `--daily-limit int` - max requests per day (0 = unlimited)
- `--monthly-limit int` - max requests per month (0 = unlimited)
- `--daily-usd-cap string` - daily cloud spend cap in USD (0 = unlimited)
- `--monthly-usd-cap string` - monthly cloud spend cap in USD (0 = unlimited)
- `--models string` - comma-separated allowed models (empty = all)
- `--expires-at string` - expiry date (2006-01-02 or RFC3339)
- `--key string` - explicit secret (default: server-generated)
- `--local-only string` - block cloud fallback: true or false
- `--allow-local-degradation string` - allow local alternate model: true or false

#### `revoke <name>`

revoke (delete) a key

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--yes` - confirm revocation without prompting

#### `patch <name>`

update key settings

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--rate-limit string` - max requests per hour (0 = unlimited)
- `--daily-limit string` - max requests per day (0 = unlimited)
- `--monthly-limit string` - max requests per month (0 = unlimited)
- `--daily-usd-cap string` - daily cloud spend cap in USD
- `--monthly-usd-cap string` - monthly cloud spend cap in USD
- `--models string` - comma-separated allowed models (empty = clear)
- `--expires-at string` - expiry date (2006-01-02 or RFC3339, empty = clear)
- `--local-only string` - block cloud fallback: true or false
- `--allow-local-degradation string` - allow local alternate model: true or false

#### `set-local-only <name> <true|false>`

block (or re-allow) cloud fallback for one API key

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `set-allow-local-degradation <name> <true|false>`

let (or forbid) one API key receive a local alternate model

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `schedules`

manage time-of-day warmup/unload/drain/undrain automations

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

#### `list`

list schedules

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `create`

create a schedule

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--action string` - warmup, unload, drain, or undrain (required) (required)
- `--node string` - target node name (required) (required)
- `--at string` - time of day, HH:MM 24h server-local (required) (required)
- `--models string` - comma-separated models (required for warmup/unload)
- `--days string` - comma-separated days 0=Sun..6=Sat (empty = every day)
- `--enabled` - enable immediately

#### `patch <id>`

update a schedule (only flags you pass are changed)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--enabled` - enable or disable
- `--action string` - warmup, unload, drain, or undrain
- `--node string` - target node name
- `--models string` - comma-separated models (empty clears)
- `--at string` - time of day, HH:MM 24h server-local
- `--days string` - comma-separated days 0=Sun..6=Sat (empty = every day)

#### `delete <id>`

delete a schedule

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--yes` - confirm deletion without prompting

### `routing`

manage routing rules and global routing strategy

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

#### `rules`

list/add/remove/toggle routing rules

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `list`

list routing rules

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `add`

add a routing rule

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--id string` - rule id (required) (required)
- `--condition string` - match condition (required) (required)
- `--target string` - target node name
- `--strategy string` - per-rule strategy override
- `--priority int` - rule priority (higher wins)
- `--enabled` - enable immediately

##### `remove <id>`

remove a routing rule

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--yes` - confirm removal without prompting

##### `toggle <id>`

toggle a routing rule's enabled state

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `strategy`

get/set the global routing strategy

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `get`

show the global routing strategy

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `set <strategy>`

set the global routing strategy

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `cloud`

manage cloud overflow providers and view budget status

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

#### `providers`

list/add/update/delete/reorder/test cloud providers

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `list`

list cloud providers (does not display the API key)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `add`

add a cloud provider

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--name string` - provider config name (required) (required)
- `--provider string` - provider type, e.g. openai, anthropic, openrouter (required) (required)
- `--base-url string` - provider API base URL (required if --enabled)
- `--api-key string` - provider API key (required if --enabled) - never echoed back by "list"
- `--default-model string` - default model for this provider
- `--cost-per-1k string` - cost per 1K tokens in USD, for savings tracking
- `--priority int` - fallback priority (lower tries first)
- `--enabled` - enable immediately

##### `update <name>`

update a cloud provider (omit --api-key to keep the stored key)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--provider string` - provider type, e.g. openai, anthropic, openrouter
- `--base-url string` - provider API base URL
- `--api-key string` - provider API key (omit to keep the currently stored key)
- `--default-model string` - default model for this provider
- `--cost-per-1k string` - cost per 1K tokens in USD, for savings tracking
- `--priority int` - fallback priority (lower tries first)
- `--enabled` - enable this provider

##### `delete <name>`

delete a cloud provider

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--yes` - confirm deletion without prompting

##### `reorder <names>`

set cloud provider fallback priority order

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `test`

verify a base-url+api-key pair authenticates, without saving it

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--provider string` - provider type (required) (required)
- `--base-url string` - provider API base URL (required) (required)
- `--api-key string` - provider API key to test (required) (required)

#### `budget-status`

show global and per-key cloud spend vs budget caps

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `favorites`

manage your starred model list

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

#### `list`

list starred model ids

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `add <model-id>`

star a model

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `remove <model-id>`

unstar a model

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `model-config`

manage per-node model parameter profiles

store.ModelConfig has ~40 optional per-runtime sampling/load-time fields, so
"set" takes a JSON body via --from-json (a literal JSON string or
@path/to/file.json) rather than dozens of individual flags - see
internal/cli/modelconfig.go for the full field list and rationale.

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

#### `get`

get a model's parameter profile on one node

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--model string` - model name (required) (required)
- `--node string` - node name (required) (required)

#### `set`

create/update a model's parameter profile (full JSON body)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--from-json string` - JSON body (literal string, or @path/to/file.json) - must include "model" and "node" (required) (required)

#### `delete`

reset a model on a node to backend defaults

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--model string` - model name (required) (required)
- `--node string` - node name (required) (required)

#### `list`

list every configured model parameter profile

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `capabilities`

show which parameter fields take effect per runtime

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `catalog`

show the fleet-aware HF/local model catalog with per-node fit

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

### `backup`

manage marbor.db backups

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

#### `now`

trigger an on-demand backup and download it

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--output string` - local file path to save to (default: server-suggested filename)

#### `list`

list backup files on the server

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `restore <filename>`

restore marbor.db from a backup file (marbor restarts)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--yes` - confirm restore without prompting

#### `upload`

upload a local .db file as a restorable backup

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--file string` - local .db file path (required) (required)

### `analytics`

hourly analytics + per-model stats

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

#### `show`

show analytics (raw JSON)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `export`

export analytics to a local file

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--type string` - hourly (default) or models
- `--format string` - csv or json (default json)
- `--output string` - local file path to save to (default: server-suggested filename)

### `savings`

show cloud-vs-local savings summary

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

### `metrics`

dashboard metrics

#### `summary`

show the dashboard summary strip (nodes, active requests, latency, tokens/min)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `pulls`

list every active model pull job across the fleet

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

### `warmup`

global warmup engine status and manual controls

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

#### `status`

show global warmup engine status

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `predictive`

enable/disable the predictive prewarm engine

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

##### `set`

enable/disable the predictive prewarm engine

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--enabled` - enable the predictive engine

#### `ping`

manually trigger a warmup cycle now

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `predictive`

show recent predictive prewarm decisions

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

#### `decisions`

show recent predictive prewarm decisions

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `system-info`

show control-plane host system info and per-node GPU summary

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

### `config`

control-plane configuration operations

#### `reload`

re-sync live router/auth state from SQLite

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `benchmark`

run/inspect in-dashboard hardware benchmark jobs

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

#### `run <node> <model>`

start a benchmark job

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--n int` - number of cold/warm samples (1-50, default 10)

#### `progress <job-id>`

show a point-in-time snapshot of a running benchmark job

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `cancel <job-id>`

cancel an in-flight benchmark job

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `runs`

show persisted benchmark run history

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `spill`

show per-key, per-provider local-vs-cloud request counts

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

### `activity`

show unified fleet activity feed (drain, agent, runtime, node, warmup, schedule, predictive, config)

Times are shown in UTC (RFC3339 Z) - the Admin API stores every audit event in UTC. The dashboard renders the same instants in the operator's configured timezone; this CLI shows the raw UTC value.

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--limit int` - max events to show (1-200, default 100)
- `--kind string` - filter by kind: drain, agent, runtime, node, warmup, schedule, predictive, config, or all (default all)
- `--from string` - filter from time (RFC3339, e.g. 2026-08-26T00:00:00Z)
- `--to string` - filter to time (RFC3339, e.g. 2026-08-26T23:59:59Z)
- `--before string` - paginate before time (RFC3339, exclusive)
- `--action string` - filter by exact action (e.g. drain_node)
- `--user string` - filter by operator username (prefix match)
- `--target string` - filter by target (substring, e.g. gpu-node-02)
- `--source_ip string` - filter by source IP (substring)

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

### `requests`

inspect routing decisions for past requests

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `explain <request-id>`

show why the router picked the node it did for one request

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `list`

show the in-memory request log, newest first

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `live`

show the same bounded request ring in its raw live-widget shape

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `audit`

inspect the persisted, filterable request audit log

Distinct from "activity", which covers operator actions (drain/agent/runtime/node/warmup); "audit" covers individual proxied requests.

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--limit int` - max entries to show (1-1000, default 100)
- `--model string` - filter by exact model name
- `--key string` - filter by exact API key name
- `--node string` - filter by exact node name
- `--status string` - filter by status category: success, client_error, or server_error
- `--cloud string` - filter by cloud fallback: true or false
- `--since string` - filter from time (RFC3339)
- `--until string` - filter to time (RFC3339)

requires credentials: run "marbor login" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD).

### `users`

manage dashboard users

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `list`

list users

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `create`

create a user (password printed once)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--user string` - username for the new user (required) (required)
- `--email string` - email for the new user
- `--role string` - role: admin or user

#### `approve <id>`

approve a pending user

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--api-key-name string` - API key name to assign
- `--create-key` - create an API key for the user
- `--key-rate-limit int` - rate limit for the new key (per hour)
- `--key-daily-limit int` - daily limit for the new key
- `--key-monthly-limit int` - monthly limit for the new key
- `--key-models string` - comma-separated allowed models for the new key

#### `suspend <id>`

suspend a user and revoke sessions

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--yes` - confirm suspension without prompting

#### `reset-password <id>`

reset a user's password (printed once)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `patch <id>`

update a user's email or role

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--email string` - new email
- `--role string` - new role: admin or user

#### `delete <id>`

delete a user

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

Flags:

- `--yes` - confirm deletion without prompting

#### `pending-count`

show the number of users awaiting approval

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `change-password`

change your own password (interactive, masked prompts)

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

#### `skip-password-change`

dismiss the forced-password-change prompt for this session only

Requires authentication - see the root README's CLI auth section, or run `marbor login`.

### `completion <shell>`

_Hidden from `--help` output, but fully reachable._

generate a shell completion script (bash, zsh, or fish)

Generates a static completion script for the requested shell by walking
the current command tree. The script never contacts marbor or
requires credentials, so it keeps working even when marbor is
unreachable or the operator isn't logged in.

Examples:

```bash
source <(marbor completion bash)
marbor completion zsh > "${fpath[1]}/_marbor"
marbor completion fish > ~/.config/fish/completions/marbor.fish
```


# marbor fleet enrollment - Ansible

Automates GPU node registration and marbor agent installation against an
marbor Admin API. This does exactly what a human does by hand on the
dashboard's **GPU Nodes** page - it wraps the same Admin API calls
(`POST /admin/nodes`, `POST /admin/nodes/{name}/agent`) plus running the
resulting install command on each GPU host. See
`docs/deploy/gpu-node-registration.md` and `docs/deploy/marbor-agent-enrollment.md`
for the full HTTP contract this automates, including a no-Ansible curl/uri
walkthrough if you'd rather script it yourself or just understand what's
happening under the hood.

This is source committed to the marbor repo for operators to copy and
run - it is **not** published to Ansible Galaxy or any external registry,
and is not intended to be. Copy the `ansible/` directory (or clone the repo)
and run it locally against your own fleet.

## Two playbooks, two independent operations

Registering a node's runtime endpoint and enrolling its marbor agent are
deliberately kept as **separate playbooks**, not one combined script:

| Playbook | Does | Inventory |
|---|---|---|
| [`playbooks/register-gpus.yml`](playbooks/register-gpus.yml) | Registers each node's runtime endpoint with the marbor (`POST /admin/nodes`) | [`inventory.example.yml`](inventory.example.yml) |
| [`playbooks/install-marbor-agent.yml`](playbooks/install-marbor-agent.yml) | Enrolls and installs the marbor agent on an already-registered node (`POST /admin/nodes/{name}/agent` + install) | [`inventory-agents.example.yml`](inventory-agents.example.yml) |

Each is independently idempotent - re-running `register-gpus.yml` never
touches agent enrollment, and re-running `install-marbor-agent.yml` never
re-registers a node's endpoint. Run `register-gpus.yml` first for a new
node (the agent playbook fails fast, naming the host, if you point it at a
name the marbor doesn't recognize yet); after that, run either one on its own
whenever you only need to change that one thing - e.g. reinstall/rotate an
agent without touching the node's registration, or re-point a node's runtime
URL without disturbing its already-healthy agent.

## Prerequisites

- Ansible core (`ansible-playbook`) on the machine you run this from. No
  external Ansible collections are required - every task uses
  `ansible.builtin` modules (`uri`, `shell`, `assert`, `set_fact`, `debug`),
  matching this project's zero-external-dependency ethos.
- Network access from wherever you run the playbook to the marbor's Admin API
  (`marbor_url`, e.g. `https://marbor.example.com`).
- SSH access from wherever you run `install-marbor-agent.yml` to every GPU
  host in your agent inventory, with a user that can run the install command
  (the marbor agent installer registers a system service - see
  `install.sh`/`install.ps1` - so that user typically needs
  `sudo`/Administrator rights, or you configure `become: true` yourself).
  Neither playbook sets `ansible_user`, `ansible_ssh_private_key_file`, or
  `become` for you - configure those the normal Ansible way (an
  `ansible.cfg`, `group_vars`, or `-e` on the command line).
  `register-gpus.yml` never connects to the GPU hosts over SSH at all - it
  only talks to the marbor's Admin API.
- An marbor admin account's username and password. There is no
  separate static "admin API key" for these routes (see
  `docs/deploy/gpu-node-registration.md`).

## Files

- [`inventory.example.yml`](inventory.example.yml) - example `gpu_nodes:`
  list for `register-gpus.yml`.
- [`inventory-agents.example.yml`](inventory-agents.example.yml) - example
  `agent_nodes:` list for `install-marbor-agent.yml`.
- [`playbooks/register-gpus.yml`](playbooks/register-gpus.yml) - registers
  runtime endpoints. Runs entirely from `localhost`; never touches the GPU
  hosts over SSH.
- [`playbooks/install-marbor-agent.yml`](playbooks/install-marbor-agent.yml) -
  enrolls and installs marbor agents. Runs the API calls from `localhost` and
  delegates only the install step to each GPU host.

Both are plain vars files, not classic Ansible host inventories - each
playbook loops over its list and reaches GPU hosts via `delegate_to`, so the
hosts don't need a pre-existing inventory entry.

## Running it

Register nodes:

```bash
cp ansible/inventory.example.yml my-fleet.yml
# edit my-fleet.yml: real hosts, ports, runtimes

ansible-playbook ansible/playbooks/register-gpus.yml \
  -e @my-fleet.yml \
  -e marbor_url=https://marbor.example.com \
  -e marbor_admin_username=admin \
  --ask-vault-pass \
  -e @secrets.vault.yml
```

Then enroll their marbor agents:

```bash
cp ansible/inventory-agents.example.yml my-agent-fleet.yml
# edit my-agent-fleet.yml: same nodes, by the exact name they registered as

ansible-playbook ansible/playbooks/install-marbor-agent.yml \
  -e @my-agent-fleet.yml \
  -e marbor_url=https://marbor.example.com \
  -e marbor_admin_username=admin \
  --ask-vault-pass \
  -e @secrets.vault.yml
```

Where `secrets.vault.yml` is an Ansible Vault-encrypted file containing at
least:

```yaml
marbor_admin_password: "<your admin password>"
```

Never hardcode `marbor_admin_password` in a plain file committed anywhere -
use `ansible-vault encrypt secrets.vault.yml` (or `--extra-vars` typed
interactively / injected by your CI secret store) so the password never sits
in plaintext on disk.

## What each `gpu_nodes` entry needs (`register-gpus.yml`)

| Field   | Required? | Default |
|---------|-----------|---------|
| `name`  | No        | `gpu-node-01`, `gpu-node-02`, ... generated from list position. Explicit name always wins. |
| `host`  | Yes       | none - the GPU node's IP or hostname |
| `port`  | Required for every runtime except `ollama` | `11434` for `ollama` only |
| `runtime` | No      | `ollama` (matches the Admin API's own default) |

**Port defaults are intentionally narrow.** The marbor codebase has no
canonical default-port table for non-Ollama runtimes - `internal/config` and
`internal/admin` don't define one (searched `isValidRuntime`,
`internal/config/config.go`, and the node config struct). A code comment
mentions vLLM commonly running on `:8000`, but that's illustrative prose,
not an enforced default anywhere in the router or admin API. Rather than
inventing a number that might silently point at the wrong port, this
playbook **requires an explicit `port` for `vllm`, `tgi`, `llamacpp`, and
`mlx`** and fails fast with a clear message if one is missing. Only
`ollama` gets a default (`11434`), because that is Ollama's own standard
port - the same number marbor's proxy deliberately listens on for
drop-in compatibility.

## What each `agent_nodes` entry needs (`install-marbor-agent.yml`)

| Field  | Required? |
|--------|-----------|
| `name` | **Yes** - must exactly match a node already registered with the marbor. Nothing is generated here (unlike `gpu_nodes` above) - a guessed name would silently target the wrong node. |
| `host` | Yes - IP or hostname, used as the SSH target for installing the agent. |
| `agent_port` | No - defaults to `9200`. This is the port the marbor **agent itself** listens on - its own port, *not* the node's runtime endpoint port (11434/8000/...). The Admin API requires a port explicitly (`POST /admin/nodes/{name}/agent` rejects an absent/invalid one), so the playbook sends this value; override per entry with an `agent_port:` field or globally with `-e agent_port=....` |

There's no `runtime` field here - agent enrollment doesn't need to know the
node's runtime, only its name and host.

## What `register-gpus.yml` does, per node

1. Logs in once (`POST /admin/login`), reusing the session cookie for every
   node in the run.
2. Resolves the node's name (explicit or generated) and port/runtime
   defaults.
3. Registers the node (`POST /admin/nodes`) - safe to always call. This
   endpoint upserts by name, confirmed idempotent (see
   `internal/router/router.go` `AddNode`, `internal/admin/admin.go:1970`
   `handleAddNode` - repeat calls with the same name update in place and
   return `200`, first call returns `201`).
4. Polls `GET /admin/nodes/{name}` (10 attempts, 5 seconds apart by default -
   tune with `-e poll_retries=... -e poll_delay=...`) until the node reports
   `health: "healthy"`, or gives up and names the host in the final failure
   message.
5. Records a per-node result fact used by the summary.

At the end, prints a plain-text summary line per node (name, host:port,
runtime, final status), then fails the play (non-zero exit) if any node
never came up healthy.

## What `install-marbor-agent.yml` does, per node

1. Logs in once (`POST /admin/login`), reusing the session cookie for every
   node in the run.
2. Fetches the current fleet (`GET /admin/nodes`) and fails fast, naming any
   host whose `name` isn't a registered marbor node yet.
3. Decides whether the marbor agent needs (re-)enrolling - see **Agent
   re-enrollment policy** below.
4. If needed, enables the marbor agent (`POST /admin/nodes/{name}/agent`) and
   captures the returned `install_command`.
5. Runs `install_command` on the GPU host itself over SSH (`delegate_to`).
6. Polls `GET /admin/nodes/{name}` until the node reports `health: "healthy"`
   and `agentPresent: true`, or gives up and names the host in the final
   failure message.
7. Records a per-node result fact used by the summary.

At the end, prints a plain-text summary line per node (name, host:port,
whether the agent was (re-)enrolled this run, final status), then fails the
play (non-zero exit) if any node's agent never came up healthy - so this is
safe to wire into CI/cron without silently succeeding on a partial fleet.

## Agent re-enrollment policy (read this before re-running against a live fleet)

`POST /admin/nodes/{name}/agent` is **not** safe to call blindly on every
run. Per `internal/admin/admin.go` `handleEnableMarborAgent`, every call:

- generates a brand-new opaque token,
- overwrites the persisted `marbor_agent` record for that host, and
- pushes the new token to the live router immediately (`SetMarborAgent`),

with no check for whether an agent is already enrolled and healthy. Calling
it repeatedly against an already-healthy node would rotate its credential
out from under it and could interrupt a live polling/agent connection for
no reason - there's no benefit and a real (if small) disruption cost.

So `install-marbor-agent.yml` checks `GET /admin/nodes` first and **only calls
`POST /admin/nodes/{name}/agent` for a node that is not already reporting
`agentPresent: true` and `health: "healthy"`**. Re-running the playbook
against a fully healthy fleet skips agent enrollment/install entirely for
every node that doesn't need it. If you deliberately want to force a token
rotation for a specific node, use `POST /admin/nodes/{name}/agent/regenerate`
directly (see `docs/deploy/marbor-agent-enrollment.md`) rather than re-running
this playbook.

## Field name reference (Admin API JSON)

`GET /admin/nodes` returns an array of node objects; the fields these
playbooks rely on (verified against `internal/admin/admin.go`
`nodeStateToResp`) are:

- `name` (string)
- `health` (string: `"healthy"`, `"degraded"`, or `"down"`)
- `agentPresent` (bool)
- `agentVersion` (string, omitted if empty)

## Not in scope

- Provisioning the GPU host's OS, drivers, or the runtime itself (Ollama,
  vLLM, etc.) - that's Ansible/Terraform territory upstream of these
  playbooks, per this project's standing architecture position.
  `register-gpus.yml` assumes the runtime is already installed and
  reachable at `host:port` before it runs.
- Windows GPU hosts. `install-marbor-agent.yml` connects over SSH and runs a
  POSIX shell install command - Windows agent installation needs the
  Admin API's `install_command_windows` run manually, or a WinRM-based
  playbook this repo doesn't provide.
- Publishing either playbook to Ansible Galaxy or any external registry.
  There is no `galaxy.yml` here and none should be added - distribution of
  this automation is explicitly out of scope for this repo right now.

# ollama-mesh fleet enrollment - Ansible

Automates GPU node registration and Node Agent installation against an
ollama-mesh Admin API. This does exactly what a human does by hand on the
dashboard's **GPU Nodes** page - it wraps the same two Admin API calls
(`POST /admin/nodes` then `POST /admin/nodes/{name}/agent`) plus running the
resulting install command on each GPU host. See
`docs/deploy/fleet-enrollment.md` for the full HTTP contract this automates,
including a no-Ansible curl/uri walkthrough if you'd rather script it
yourself or just understand what's happening under the hood.

This is source committed to the ollama-mesh repo for operators to copy and
run - it is **not** published to Ansible Galaxy or any external registry,
and is not intended to be. Copy the `ansible/` directory (or clone the repo)
and run it locally against your own fleet.

## Prerequisites

- Ansible core (`ansible-playbook`) on the machine you run this from. No
  external Ansible collections are required - every task uses
  `ansible.builtin` modules (`uri`, `shell`, `assert`, `set_fact`, `debug`),
  matching this project's zero-external-dependency ethos.
- Network access from wherever you run the playbook to the mesh's Admin API
  (`mesh_url`, e.g. `https://mesh.example.com`).
- SSH access from wherever you run the playbook to every GPU host in your
  node list, with a user that can run the install command (the Node Agent
  installer registers a system service - see `install.sh`/`install.ps1` -
  so that user typically needs `sudo`/Administrator rights, or you configure
  `become: true` yourself). This playbook does not set `ansible_user`,
  `ansible_ssh_private_key_file`, or `become` for you - configure those the
  normal Ansible way (an `ansible.cfg`, `group_vars`, or `-e` on the command
  line) for the hosts you list under `gpu_nodes`.
- An ollama-mesh admin account's username and password. There is no
  separate static "admin API key" for these routes (see
  `docs/deploy/fleet-enrollment.md`).

## Files

- `inventory.example.yml` - example node list (`gpu_nodes:`). Copy it,
  rename it, and edit the hosts/ports/runtimes for your fleet. This is a
  plain vars file, not a classic Ansible host inventory - the playbook loops
  over the list and reaches each GPU host via `delegate_to`, so the hosts
  don't need a pre-existing inventory entry.
- `playbooks/register-gpus.yml` - the playbook. Runs entirely from
  `localhost` for the API calls, and delegates only the install step to each
  GPU host.

## Running it

```bash
cp ansible/inventory.example.yml my-fleet.yml
# edit my-fleet.yml: real hosts, ports, runtimes

ansible-playbook ansible/playbooks/register-gpus.yml \
  -e @my-fleet.yml \
  -e mesh_url=https://mesh.example.com \
  -e mesh_admin_username=admin \
  --ask-vault-pass \
  -e @secrets.vault.yml
```

Where `secrets.vault.yml` is an Ansible Vault-encrypted file containing at
least:

```yaml
mesh_admin_password: "<your admin password>"
```

Never hardcode `mesh_admin_password` in a plain file committed anywhere -
use `ansible-vault encrypt secrets.vault.yml` (or `--extra-vars` typed
interactively / injected by your CI secret store) so the password never sits
in plaintext on disk.

## What each node in `gpu_nodes` needs

| Field   | Required? | Default |
|---------|-----------|---------|
| `name`  | No        | `gpu-node-01`, `gpu-node-02`, ... generated from list position. Explicit name always wins. |
| `host`  | Yes       | none - the GPU node's IP or hostname |
| `port`  | Required for every runtime except `ollama` | `11434` for `ollama` only |
| `runtime` | No      | `ollama` (matches the Admin API's own default) |

**Port defaults are intentionally narrow.** The ollama-mesh codebase has no
canonical default-port table for non-Ollama runtimes - `internal/config` and
`internal/admin` don't define one (searched `isValidRuntime`,
`internal/config/config.go`, and the node config struct). A code comment
mentions vLLM commonly running on `:8000`, but that's illustrative prose,
not an enforced default anywhere in the router or admin API. Rather than
inventing a number that might silently point at the wrong port, this
playbook **requires an explicit `port` for `vllm`, `tgi`, `llamacpp`, and
`mlx`** and fails fast with a clear message if one is missing. Only
`ollama` gets a default (`11434`), because that default already exists in
the Admin API's own `POST /admin/nodes` behavior and matches Ollama's own
standard port.

## What the playbook does, per node

1. Logs in once (`POST /admin/login`), reusing the session cookie for every
   node in the run.
2. Resolves the node's name (explicit or generated) and port/runtime
   defaults.
3. Registers the node (`POST /admin/nodes`) - safe to always call. This
   endpoint upserts by name, confirmed idempotent (see
   `internal/router/router.go` `AddNode`, `internal/admin/admin.go:1954`
   `handleAddNode` - repeat calls with the same name update in place and
   return `200`, first call returns `201`).
4. Fetches current fleet status (`GET /admin/nodes`) to decide whether the
   Node Agent needs (re-)enrolling - see **Agent re-enrollment policy**
   below.
5. If needed, enables the Node Agent (`POST /admin/nodes/{name}/agent`) and
   captures the returned `install_command`.
6. Runs `install_command` on the GPU host itself over SSH (`delegate_to`).
7. Polls `GET /admin/nodes/{name}` (10 attempts, 5 seconds apart by default -
   tune with `-e poll_retries=... -e poll_delay=...`) until the node reports
   `health: "healthy"` and `agentPresent: true`, or gives up and names the
   host in the final failure message.
8. Records a per-node result fact used by the summary.

At the end, the playbook prints a plain-text summary line per node (name,
host:port, runtime, whether the agent was (re-)enrolled this run, and final
status), then fails the whole play (non-zero exit) if any node never came
up healthy - so this is safe to wire into CI/cron without silently
succeeding on a partial fleet.

## Agent re-enrollment policy (read this before re-running against a live fleet)

`POST /admin/nodes/{name}/agent` is **not** safe to call blindly on every
run. Per `internal/admin/admin.go` `handleEnableNodeAgent`, every call:

- generates a brand-new opaque token,
- overwrites the persisted `node_agent` record for that host, and
- pushes the new token to the live router immediately (`SetNodeAgent`),

with no check for whether an agent is already enrolled and healthy. Calling
it repeatedly against an already-healthy node would rotate its credential
out from under it and could interrupt a live polling/agent connection for
no reason - there's no benefit and a real (if small) disruption cost.

So this playbook checks `GET /admin/nodes` first and **only calls
`POST /admin/nodes/{name}/agent` for a node that is not already reporting
`agentPresent: true` and `health: "healthy"`**. Re-running the playbook
against a fully healthy fleet re-confirms registration (which is genuinely
idempotent) and skips agent enrollment/install entirely for every node that
doesn't need it. If you deliberately want to force a token rotation for a
specific node, use `POST /admin/nodes/{name}/agent/regenerate` directly (see
`docs/deploy/fleet-enrollment.md`) rather than re-running this playbook.

## Field name reference (Admin API JSON)

`GET /admin/nodes` returns an array of node objects; the fields this
playbook relies on (verified against `internal/admin/admin.go`
`nodeStateToResp`) are:

- `name` (string)
- `health` (string: `"healthy"`, `"degraded"`, or `"down"`)
- `agentPresent` (bool)
- `agentVersion` (string, omitted if empty)

## Not in scope

- Provisioning the GPU host's OS, drivers, or the runtime itself (Ollama,
  vLLM, etc.) - that's Ansible/Terraform territory upstream of this
  playbook, per this project's standing architecture position. This
  playbook assumes the runtime is already installed and reachable at
  `host:port` before it runs.
- Publishing this role to Ansible Galaxy or any external registry. There is
  no `galaxy.yml` here and none should be added - distribution of this
  automation is explicitly out of scope for this repo right now.

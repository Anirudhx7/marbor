# Node Agent enrollment (Ansible or any script)

Enroll and install the Node Agent on many already-registered GPU nodes at once,
without clicking through the dashboard per node. Every step below is a plain REST
call against the Admin API - the GPU Nodes dashboard page is a thin wrapper over the
same endpoints, so anything the UI can do, a script with an authenticated admin
session can do identically.

A node must already be registered before its agent can be enrolled - see
[GPU node registration](gpu-node-registration.md) for that separate, first step.

## Authenticating a script

Same session-cookie auth as node registration - see
[GPU node registration's "Authenticating a script"](gpu-node-registration.md#authenticating-a-script)
section. Every call below reuses that same cookie jar.

## How enrollment actually works

Each node gets its own unique agent token, minted server-side and bound to that
node's host - it is never a shared fleet-wide secret. A token copied to a different
machine will not authenticate there; the mesh checks it against the specific host it
was generated for. Tokens can be rotated or revoked at any time without touching
other nodes.

The install command the API hands back does **not** embed the real permanent token
directly. It embeds a short-lived, single-use enrollment code instead (`ENROLL=`),
which the agent exchanges for the real token via `POST /admin/agent/enroll` at
install time - so the permanent bearer token never sits in shell history, SSH logs,
or chat. (`install.sh` also accepts a raw `TOKEN=` as a legacy/manual fallback, but
`ENROLL=` is what the API generates and what you should script against.)

`POST /admin/nodes/{name}/agent` - enables the Node Agent for an already-registered
node and returns a ready-to-run install command with the enrollment code embedded:

```json
{
  "node": "gpu01",
  "enabled": true,
  "port": 11434,
  "token": "admin.<opaque-permanent-token>",
  "install_command": "curl -fsSL https://raw.githubusercontent.com/Anirudhx7/marbor/main/install.sh | ROLE=agent Marbor=https://marbor.example.com ENROLL=<short-lived-code> PORT=11434 sh",
  "install_command_windows": "..."
}
```

Run the returned `install_command` on the target host (via Ansible, SSH, whatever
you use) and the agent exchanges the code for its real token and registers itself.

**This endpoint is not safe to call blindly on every run.** It unconditionally mints
a fresh token, overwrites the persisted `node_agent` record, and pushes the new
token to the live router - a repeat call against an already-healthy node rotates its
credential out from under it and could interrupt a live connection for no reason.
Check `GET /admin/nodes` first and only call this for a node that isn't already
reporting `agentPresent: true` and `health: "healthy"`.

## Scripted enrollment for N nodes

```bash
MESH=https://marbor.example.com
COOKIES=$(mktemp)

curl -sf -c "$COOKIES" -X POST "$MESH/admin/login" \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"<your admin password>"}' > /dev/null

for host in gpu01 gpu02 gpu03; do
  # Enable the agent, capture the install command
  INSTALL_CMD=$(curl -sf -b "$COOKIES" -X POST "$MESH/admin/nodes/$host/agent" \
    -H "Content-Type: application/json" \
    -d '{"port":11434}' | jq -r '.install_command')

  # Run it on the target host (SSH shown here; swap for your Ansible task)
  ssh "$host" "$INSTALL_CMD"
done
```

## Recommended path: the first-party Ansible playbook

A first-party Ansible playbook automates the sequence above - login once, check
which nodes already have a healthy agent (skipping those), enable and install the
rest, then poll until each is healthy - across an arbitrary list of already-
registered GPU hosts declared in a simple vars file. It ships as source in this repo
only; it is not published to Ansible Galaxy or any external registry.

- Playbook: [`ansible/playbooks/install-node-agent.yml`](https://github.com/Anirudhx7/ollama-mesh/blob/main/ansible/playbooks/install-node-agent.yml)
- Example inventory: [`ansible/inventory-agents.example.yml`](https://github.com/Anirudhx7/ollama-mesh/blob/main/ansible/inventory-agents.example.yml)
- Full variable reference, the re-enrollment idempotency policy, and prerequisites:
  [`ansible/README.md`](https://github.com/Anirudhx7/ollama-mesh/blob/main/ansible/README.md)

This playbook enrolls/installs agents only - it does not register a node's runtime
endpoint. Run [`ansible/playbooks/register-gpus.yml`](https://github.com/Anirudhx7/ollama-mesh/blob/main/ansible/playbooks/register-gpus.yml)
first for any node not yet registered - see [GPU node registration](gpu-node-registration.md).

## Ansible sketch (how it works under the hood / no-Ansible fallback)

```yaml
- name: Log in and capture session cookie
  uri:
    url: "https://mesh.example.com/admin/login"
    method: POST
    body_format: json
    body:
      username: admin
      password: "{{ mesh_admin_password }}"
    status_code: 200
  register: mesh_login
  delegate_to: localhost
  run_once: true

- name: Enable agent and capture install command
  uri:
    url: "https://mesh.example.com/admin/nodes/{{ inventory_hostname }}/agent"
    method: POST
    headers:
      Cookie: "{{ mesh_login.set_cookie }}"
    body_format: json
    body:
      port: 11434
    return_content: true
  register: agent_response
  delegate_to: localhost

- name: Install agent on GPU node
  shell: "{{ (agent_response.content | from_json).install_command }}"
```

Run this play against a `gpu_nodes` inventory group and every host gets its own
enrollment code from the same run - no manual UI step, no shared secret.

## Rotation and revocation

- `POST /admin/nodes/{name}/agent/regenerate` mints a new token (and a new install
  command with a fresh enrollment code) for one node - requires restarting the agent
  process on that host with the new credential.
- `DELETE /admin/nodes/{name}/agent` disables the agent for a node and revokes its
  token immediately.

Both are scoped to a single node (or, more precisely, to the physical host it shares
with any other node entries on the same machine) and never affect any other host's
token.

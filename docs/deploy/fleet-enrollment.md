# Automated fleet enrollment (Ansible or any script)

Enroll many GPU nodes at once without clicking through the dashboard per node. Every
step below is a plain REST call against the Admin API - the GPU Nodes dashboard page
is a thin wrapper over the same endpoints, so anything the UI can do, a script with
an authenticated admin session can do identically.

## Authenticating a script

There is no separate long-lived "admin API key" for these routes - they use the same
session-based admin auth as the dashboard and CLI. `POST /admin/login` with an admin
user's username/password returns the session as an `HttpOnly` cookie (the token
itself is not present in the JSON response body), valid for 30 days or until logout.
The simplest reliable way to script this is a curl cookie jar, which the routes below
also accept as `Authorization: Bearer <token>` if you prefer to extract the token
value from the cookie yourself.

```bash
MESH=https://mesh.example.com
COOKIES=$(mktemp)

curl -sf -c "$COOKIES" -X POST "$MESH/admin/login" \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"<your admin password>"}' > /dev/null
```

Every subsequent call in this guide reuses `-b "$COOKIES"`.

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

Provisioning a node is two sequential calls:

1. `POST /admin/nodes` - registers the node (`{"name", "url"}`; other fields like
   `runtime` are optional, defaulting to `ollama`). Returns `201 Created` with an
   empty body.
2. `POST /admin/nodes/{name}/agent` - enables the Node Agent for that node and
   returns a ready-to-run install command with the enrollment code embedded:

```json
{
  "node": "gpu01",
  "enabled": true,
  "port": 11434,
  "token": "admin.<opaque-permanent-token>",
  "install_command": "curl -fsSL https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.sh | ROLE=agent MESH=https://mesh.example.com ENROLL=<short-lived-code> PORT=11434 sh",
  "install_command_windows": "..."
}
```

Run the returned `install_command` on the target host (via Ansible, SSH, whatever
you use) and the agent exchanges the code for its real token and registers itself.
There is currently no single "create and enroll in one call" endpoint - it's two
calls, not one. A first-party Ansible playbook now wraps both calls (see below) -
the underlying API already supports full automation today either way.

## Scripted enrollment for N nodes

```bash
MESH=https://mesh.example.com
COOKIES=$(mktemp)

curl -sf -c "$COOKIES" -X POST "$MESH/admin/login" \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"<your admin password>"}' > /dev/null

for host in gpu01 gpu02 gpu03; do
  # 1. Register the node
  curl -sf -b "$COOKIES" -X POST "$MESH/admin/nodes" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"$host\",\"url\":\"http://$host:11434\"}"

  # 2. Enable the agent, capture the install command
  INSTALL_CMD=$(curl -sf -b "$COOKIES" -X POST "$MESH/admin/nodes/$host/agent" \
    -H "Content-Type: application/json" \
    -d '{"port":11434}' | jq -r '.install_command')

  # 3. Run it on the target host (SSH shown here; swap for your Ansible task)
  ssh "$host" "$INSTALL_CMD"
done
```

## Recommended path: the first-party Ansible playbook

A first-party Ansible playbook now exists at `ansible/playbooks/register-gpus.yml`
(see `ansible/README.md`). It automates the exact sequence in this document -
login once, register each node, decide whether the Node Agent needs
(re-)enrolling, install it, and poll until healthy - across an arbitrary list
of GPU hosts declared in a simple vars file (`ansible/inventory.example.yml`).
It ships as source in this repo only; it is not published to Ansible Galaxy
or any external registry. Use it instead of hand-rolling the loop below
unless you have a reason to script this yourself (CI pipeline without
Ansible, a language other than YAML, etc.) - the sketch below remains
correct as a reference for exactly what the playbook does under the hood,
and as a no-Ansible fallback.

## Ansible sketch (how it works under the hood / no-Ansible fallback)

Before the first-party playbook existed, the pattern was: log in once, then
wrap the two API calls with `uri` and hand the returned command to the
target host:

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

- name: Register node with mesh
  uri:
    url: "https://mesh.example.com/admin/nodes"
    method: POST
    headers:
      Cookie: "{{ mesh_login.set_cookie }}"
    body_format: json
    body:
      name: "{{ inventory_hostname }}"
      url: "http://{{ inventory_hostname }}:11434"
    status_code: 201
  delegate_to: localhost

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

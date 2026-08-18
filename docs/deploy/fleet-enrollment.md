# Automated fleet enrollment (Ansible or any script)

Enroll many GPU nodes at once without clicking through the dashboard per node. Every
step below is a plain bearer-auth REST call against the Admin API - the GPU Nodes
dashboard page is a thin wrapper over the same two endpoints, so anything the UI can
do, a script holding an admin API key can do identically.

## How enrollment actually works

Each node gets its own unique agent token, minted server-side and bound to that
node's host - it is never a shared fleet-wide secret. A token copied to a different
machine will not authenticate there; the mesh checks it against the specific host it
was generated for. Tokens can be rotated or revoked at any time without touching
other nodes.

Provisioning a node is two sequential calls:

1. `POST /admin/nodes` - registers the node (name, URL, port).
2. `POST /admin/nodes/{name}/agent` - enables the Node Agent for that node and
   returns a fresh, host-bound token plus a ready-to-run install command:

```json
{
  "install_command": "curl -fsSL https://.../install.sh | ROLE=agent MESH=https://mesh.example.com TOKEN=<token> sh",
  "install_command_windows": "..."
}
```

Run that returned command on the target host (via Ansible, SSH, whatever you use)
and the agent registers itself. There is currently no single "create and enroll in
one call" endpoint - it's two calls, not one - and no first-party Ansible role yet.
Both are on the roadmap as convenience work, not blockers: the underlying API
already supports full automation today.

## Scripted enrollment for N nodes

Example using `curl` + `jq` in a loop - adapt the same two calls into an Ansible
play, a Python script, or whatever your provisioning tooling already is:

```bash
MESH=https://mesh.example.com
ADMIN_KEY=<your admin API key>

for host in gpu01 gpu02 gpu03; do
  # 1. Register the node
  curl -sf -X POST "$MESH/admin/nodes" \
    -H "Authorization: Bearer $ADMIN_KEY" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"$host\",\"url\":\"http://$host:11434\"}"

  # 2. Enable the agent, capture the install command + token
  INSTALL_CMD=$(curl -sf -X POST "$MESH/admin/nodes/$host/agent" \
    -H "Authorization: Bearer $ADMIN_KEY" \
    -H "Content-Type: application/json" \
    -d '{"port":11434}' | jq -r '.install_command')

  # 3. Run it on the target host (SSH shown here; swap for your Ansible task)
  ssh "$host" "$INSTALL_CMD"
done
```

## Ansible sketch

No first-party role exists yet, so wrap the same two API calls with `uri`, then hand
the returned command to the target host:

```yaml
- name: Register node with mesh
  uri:
    url: "https://mesh.example.com/admin/nodes"
    method: POST
    headers:
      Authorization: "Bearer {{ mesh_admin_key }}"
    body_format: json
    body:
      name: "{{ inventory_hostname }}"
      url: "http://{{ inventory_hostname }}:11434"
  delegate_to: localhost

- name: Enable agent and capture install command
  uri:
    url: "https://mesh.example.com/admin/nodes/{{ inventory_hostname }}/agent"
    method: POST
    headers:
      Authorization: "Bearer {{ mesh_admin_key }}"
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
token from the same run - no manual UI step, no shared secret.

## Rotation and revocation

- `POST /admin/nodes/{name}/agent/regenerate` mints a new token for one node
  (requires restarting the agent process on that host with the new token).
- Disabling the agent for a node revokes its token immediately.

Both are scoped to a single node and never affect any other node's token.

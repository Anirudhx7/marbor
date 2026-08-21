# GPU node registration (Ansible or any script)

Register many GPU nodes' runtime endpoints with the mesh at once, without clicking
through the dashboard per node. Every step below is a plain REST call against the
Admin API - the GPU Nodes dashboard page is a thin wrapper over the same endpoint,
so anything the UI can do, a script with an authenticated admin session can do
identically.

This covers registering a node's runtime endpoint only. Once a node is registered,
see [marbor agent enrollment](marbor-agent-enrollment.md) for the separate step of
installing its marbor agent.

## Authenticating a script

There is no separate long-lived "admin API key" for these routes - they use the same
session-based admin auth as the dashboard and CLI. `POST /admin/login` with an admin
user's username/password returns the session as an `HttpOnly` cookie (the token
itself is not present in the JSON response body), valid for 30 days or until logout.
The simplest reliable way to script this is a curl cookie jar, which the routes below
also accept as `Authorization: Bearer <token>` if you prefer to extract the token
value from the cookie yourself.

```bash
MARBOR_SERVER=https://marbor.example.com
COOKIES=$(mktemp)

curl -sf -c "$COOKIES" -X POST "$MARBOR_SERVER/admin/login" \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"<your admin password>"}' > /dev/null
```

Every subsequent call in this guide reuses `-b "$COOKIES"`.

## Registering a node

`POST /admin/nodes` - registers the node (`{"name", "url"}`; other fields like
`runtime` are optional, defaulting to `ollama`). This endpoint upserts by name: a
repeat call with the same name updates that node's config in place (`200 OK`)
instead of creating a duplicate; the first call returns `201 Created`. Safe to
re-run.

```bash
curl -sf -b "$COOKIES" -X POST "$MARBOR_SERVER/admin/nodes" \
  -H "Content-Type: application/json" \
  -d '{"name":"gpu01","url":"http://gpu01:11434"}'
```

## Scripted registration for N nodes

```bash
MARBOR_SERVER=https://marbor.example.com
COOKIES=$(mktemp)

curl -sf -c "$COOKIES" -X POST "$MARBOR_SERVER/admin/login" \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"<your admin password>"}' > /dev/null

for host in gpu01 gpu02 gpu03; do
  curl -sf -b "$COOKIES" -X POST "$MARBOR_SERVER/admin/nodes" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"$host\",\"url\":\"http://$host:11434\"}"
done
```

Once every node is registered, install their marbor agents - see
[marbor agent enrollment](marbor-agent-enrollment.md).

## Recommended path: the first-party Ansible playbook

A first-party Ansible playbook automates the sequence above - login once, register
each node, poll until its runtime endpoint reports healthy - across an arbitrary
list of GPU hosts declared in a simple vars file. It ships as source in this repo
only; it is not published to Ansible Galaxy or any external registry. Use it instead
of hand-rolling the loop above unless you have a reason to script this yourself (CI
pipeline without Ansible, a language other than YAML, etc.) - the loop above remains
correct as a reference for exactly what the playbook does under the hood, and as a
no-Ansible fallback.

- Playbook: [`ansible/playbooks/register-gpus.yml`](https://github.com/Anirudhx7/ollama-mesh/blob/main/ansible/playbooks/register-gpus.yml)
- Example inventory: [`ansible/inventory.example.yml`](https://github.com/Anirudhx7/ollama-mesh/blob/main/ansible/inventory.example.yml)
- Full variable reference and prerequisites: [`ansible/README.md`](https://github.com/Anirudhx7/ollama-mesh/blob/main/ansible/README.md)

This playbook registers runtime endpoints only - it does not install or enroll Node
Agents. See [`ansible/playbooks/install-marbor-agent.yml`](https://github.com/Anirudhx7/ollama-mesh/blob/main/ansible/playbooks/install-marbor-agent.yml)
and [marbor agent enrollment](marbor-agent-enrollment.md) for that separate step.

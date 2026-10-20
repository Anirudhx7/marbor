# Drain a GPU node without downtime (vLLM, Ollama, llama.cpp)

Drain the node in Marbor, wait for it to go quiet, do your maintenance, then undrain it. A draining node gets no new requests while the rest of the fleet keeps serving, and Marbor refuses by default to drain the only node that holds a model, so you do not take a model offline by accident.

## 1. Preview what a drain would affect

```bash
marbor runtime drain gpu-1 --dry-run
```

This prints any model that only `gpu-1` can serve and changes nothing. For a node whose models live elsewhere too, it reports that no model is left without another server.

## 2. Drain

```bash
marbor runtime drain gpu-1 --reason "driver upgrade"
marbor nodes --signal draining
```

The node now shows `DRAINING: yes` and new requests go to the other nodes. The reason you give is recorded in the activity feed (`marbor activity`).

If `gpu-1` is the only server of some model, the command asks for confirmation on a terminal, or in a script exits with status 1 and a message such as `refusing to drain "gpu-1" without --force: it is the only node that can serve llama3.1:8b`. Pass `--force` only when you accept that model being unavailable during the work:

```bash
marbor runtime drain gpu-1 --reason "driver upgrade" --force
```

If every node is drained, clients wait for the queue timeout and then get a `503` with code `no_nodes_available` (see [When every GPU is busy](when-every-gpu-is-busy.html)), so never drain the last node of a live fleet unless you intend a full outage.

## 3. Wait until it is idle

Watch the node's active requests drop to zero before you stop anything, with `marbor_active_connections{node="gpu-1"}` in Prometheus (see [Monitor a self-hosted LLM fleet](monitor-llm-fleet-prometheus-grafana.html)) or the node's load on the dashboard's **GPU Nodes** page. The `--grace-period` flag records a drain window, but nothing enforces it yet, so decide the wait yourself.

## 4. Do the maintenance

On the node itself, restart or upgrade the runtime however you normally do:

```bash
sudo systemctl restart vllm            # or ollama, or your llama-server unit
docker restart vllm-a                  # if it runs in a container
```

Or let Marbor restart it for you. Marbor only controls a runtime after you accept a control driver for the node (it never guesses one):

```bash
marbor node control probe gpu-1
marbor node control accept gpu-1 --driver systemd --identifier vllm.service
marbor runtime restart gpu-1 --dry-run
marbor runtime restart gpu-1
```

Supported drivers are `systemd`, `docker`, `process`, `launchd` and `windows_service`. A restart runs the same only-server check as a drain and needs `--force` to proceed in that case.

## 5. Bring it back

```bash
marbor runtime health gpu-1      # on-demand liveness probe
marbor runtime undrain gpu-1
marbor nodes
```

Wait for `healthy`, then undrain. New requests flow to the node again.

## Drain on a schedule

For recurring windows, such as unloading a model overnight, use `marbor schedules create` to drain and undrain automatically instead of running the commands by hand.

## What to set up next

- Know when a node needs draining: [Monitor a self-hosted LLM fleet](monitor-llm-fleet-prometheus-grafana.html)
- What clients see when capacity is short: [When every GPU is busy](when-every-gpu-is-busy.html)
- Register the node again after a move: [Load balance multiple vLLM servers](load-balance-vllm-servers.html)

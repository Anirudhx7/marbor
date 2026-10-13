# Demo

Everything for the demo stack lives here: five mock runtimes (Ollama x2, vLLM, TGI, llama.cpp, MLX), marbor, a traffic generator, Prometheus and Grafana. No GPU and no Ollama install is needed.

| File | What it is |
|---|---|
| `docker-compose.yml` | The local demo. Builds every image from this checkout, so it tests your working tree. Run it with `make demo`. |
| `docker-compose.standalone.yml` | A self-contained copy for a machine with no checkout. It pulls published images and inlines its config, so you can copy just this one file and run `docker compose -f docker-compose.standalone.yml up -d`. The dashboard is bound to loopback; reach it through an SSH tunnel (see the comments in the file). |
| `Dockerfile.mocknode` | Image for the mock runtimes (`cmd/mocknode`). |
| `Dockerfile.demotraffic` | Image for the traffic generator (`cmd/demotraffic`). |
| `marbor.demo.db` | The pre-seeded SQLite database marbor starts from. Do not edit it by hand. |
| `seed_demo.sql` | The data that goes into `marbor.demo.db`. |

The Go programs behind these images stay in `cmd/` (`cmd/mocknode`, `cmd/demotraffic`, and `cmd/demo` for the in-process demo), like every other binary in the repo.

## Commands

```bash
make demo        # build, start, send 20 requests, print the dashboard URL
make demo-down   # stop and remove the stack
make demo-db     # regenerate marbor.demo.db from the current schema plus seed_demo.sql
make smoke       # start the stack and assert auth, routing, streaming, admin and metrics
```

After a schema change run `make demo-db` and commit the result. `scripts/smoke.sh` fails when `marbor.demo.db` has drifted from the live schema plus `seed_demo.sql`.

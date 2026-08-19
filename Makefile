.PHONY: all build ui backend clean test dev demo demo-build demo-down demo-db bench smoke man docs

all: ui backend

ui:
	cd ui && npm install && npm run build
	mkdir -p internal/admin/web/dist/grafana
	cp grafana/ollama-mesh.json internal/admin/web/dist/grafana/ollama-mesh.json

backend:
	go build -o ollama-mesh .

build: ui backend

test:
	@mkdir -p internal/admin/web/dist/assets
	@touch internal/admin/web/dist/index.html
	go test ./...

test-ci: ui test

dev-ui:
	cd ui && npm run dev

## Demo targets - spin up mock Ollama/vLLM/TGI/llama.cpp nodes + mesh, send real traffic
demo-build: ## Build demo Docker images (mocknode runtimes + mesh)
	docker compose -f docker-compose.demo.yml build

demo: demo-build ## Spin up demo stack (all 4 runtimes), send 20 real requests, show dashboard URL (Docker only, no Go needed)
	@echo "Starting demo stack (mock Ollama x2 + vLLM + TGI + llama.cpp + mesh + Prometheus + Grafana)..."
	docker compose -f docker-compose.demo.yml up -d ollama-node-a ollama-node-b vllm-node tgi-node llamacpp-node mesh prometheus grafana
	@echo "Sending demo traffic (20 requests, runs in Docker - no local Go needed)..."
	docker compose -f docker-compose.demo.yml run --rm demotraffic
	@echo ""
	@echo "Dashboard: http://localhost:8080  (credentials: admin / admin)"
	@echo "Proxy:     http://localhost:11434 (key:   demo-api-key)"
	@echo "Grafana:   http://localhost:3000"

demo-down: ## Stop and remove demo stack
	docker compose -f docker-compose.demo.yml down -v

demo-db: backend ## Regenerate mesh.demo.db from live schema (migrate()) + scripts/seed_demo.sql
	rm -f mesh.demo.db.tmp mesh.demo.db.tmp.key
	./ollama-mesh -db mesh.demo.db.tmp -seed-node "name=_schema_init,url=http://init,runtime=ollama"
	sqlite3 mesh.demo.db.tmp < scripts/seed_demo.sql
	mv mesh.demo.db.tmp mesh.demo.db
	rm -f mesh.demo.db.tmp.key
	@echo "mesh.demo.db regenerated - review with 'git diff mesh.demo.db' before committing"

smoke: ## Gate the demo path: build, run, assert auth/routing/streaming/admin/metrics, tear down
	bash scripts/smoke.sh

bench: ## Warm-vs-cold first-token latency benchmark. Pass BENCH_ARGS to override flags.
	## Example: make bench BENCH_ARGS="-endpoint http://localhost:11434 -model llama3 -runs 5"
	go run ./cmd/bench $(BENCH_ARGS)

man: ## Regenerate man pages (docs/man/*.1) + docs/cli.md + README CLI table from the CLI registry
	go run ./cmd/gen-docs

docs: man ## Alias for `make man` - CI's drift check runs the same generator and diffs the result

clean:
	rm -f ollama-mesh
	rm -rf internal/admin/web/dist
	rm -rf ui/node_modules

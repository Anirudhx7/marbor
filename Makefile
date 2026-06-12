.PHONY: all build ui backend clean test dev demo demo-build demo-down

all: ui backend

ui:
	cd ui && npm install && npm run build

backend:
	go build -o ollama-mesh .

build: ui backend

test:
	go test ./...

dev-ui:
	cd ui && npm run dev

## Demo targets — spin up two mock Ollama nodes + mesh, send real traffic
demo-build: ## Build demo Docker images (mockollama nodes + mesh)
	docker compose -f docker-compose.demo.yml build

demo: demo-build ## Spin up demo stack, send 20 real requests, show dashboard URL
	@echo "Starting demo stack (mock Ollama nodes + mesh)..."
	docker compose -f docker-compose.demo.yml up -d
	@echo "Waiting for mesh to be ready..."
	@sleep 8
	@echo "Sending demo traffic (20 requests)..."
	PROXY_URL=http://localhost:11434 API_KEY=demo-api-key REQUEST_COUNT=20 go run ./cmd/demotraffic
	@echo ""
	@echo "Dashboard: http://localhost:8080  (token: demo-admin-token)"
	@echo "Proxy:     http://localhost:11434 (key:   demo-api-key)"

demo-down: ## Stop and remove demo stack
	docker compose -f docker-compose.demo.yml down -v

clean:
	rm -f ollama-mesh
	rm -rf internal/admin/web/dist
	rm -rf ui/node_modules

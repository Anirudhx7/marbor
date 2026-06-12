.PHONY: all build ui backend clean test dev demo

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

demo: build
	@echo "Starting ollama-mesh demo (proxy :11437, admin :8082)..."
	go run ./cmd/demo

clean:
	rm -f ollama-mesh
	rm -rf internal/admin/web/dist
	rm -rf ui/node_modules

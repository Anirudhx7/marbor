.PHONY: all build ui backend clean test dev

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

clean:
	rm -f ollama-mesh
	rm -rf internal/admin/web/dist
	rm -rf ui/node_modules

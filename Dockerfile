# UI stage - vite outDir is ../internal/admin/web/dist (see ui/vite.config.ts),
# so from /app/ui the build lands in /app/internal/admin/web/dist.
FROM node:20-alpine AS ui
WORKDIR /app/ui
COPY ui/package*.json ./
RUN npm ci
COPY ui/ ./
RUN npm run build

# Build stage
FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# go:embed web/dist requires the built UI; take it from the ui stage so the image
# never depends on a pre-built dist in the checkout (clean CI checkouts have none).
COPY --from=ui /app/internal/admin/web/dist ./internal/admin/web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o ollama-mesh .

# Final stage
FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/ollama-mesh .
EXPOSE 11434 9090 8080
CMD ["./ollama-mesh"]

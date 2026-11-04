# UI stage - vite outDir is ../internal/admin/web/dist (see ui/vite.config.ts),
# so from /app/ui the build lands in /app/internal/admin/web/dist.
FROM --platform=$BUILDPLATFORM node:26-alpine AS ui
WORKDIR /app/ui
COPY ui/package*.json ./
RUN npm ci
COPY ui/ ./
COPY grafana/ ./../grafana/
RUN npm run build && \
    mkdir -p /app/internal/admin/web/dist/grafana && \
    cp /app/grafana/marbor.json /app/internal/admin/web/dist/grafana/marbor.json

# Build stage
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# go:embed web/dist requires the built UI; take it from the ui stage so the image
# never depends on a pre-built dist in the checkout (clean CI checkouts have none).
COPY --from=ui /app/internal/admin/web/dist ./internal/admin/web/dist
# Cross-compile natively on the build host for each target platform (amd64, arm64) so
# the multi-arch image never needs QEMU emulation. TARGETOS/TARGETARCH are set by buildx.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-$(go env GOARCH)} go build -a -installsuffix cgo -o marbor ./cmd/marbor
# CA bundle fetched here (build host) so the final stage needs no RUN under emulation.
RUN apk --no-cache add ca-certificates

# Final stage
FROM alpine:latest
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
WORKDIR /root/
COPY --from=builder /app/marbor .
EXPOSE 11434 9090 8080
CMD ["./marbor"]

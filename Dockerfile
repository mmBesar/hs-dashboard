# hs-dashboard — Dockerfile
# Single binary dashboard — serves static files and host metrics
# Supports: linux/amd64, linux/arm64, linux/riscv64
# github.com/mmBesar/hs-dashboard

# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.22-bookworm AS builder

ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /build

# Download dependencies first (cached layer)
COPY go.mod ./
RUN go mod download

# Build binary — static, no CGO needed for pure Go
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o hs-dashboard .

# ── Final stage ───────────────────────────────────────────────────────────────
# Use distroless/static for minimal attack surface
# No shell, no package manager, just the binary
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION="unknown"
ARG BUILD_DATE="unknown"
ARG VCS_REF="unknown"

LABEL org.opencontainers.image.title="hs-dashboard" \
      org.opencontainers.image.description="Self-hosted server dashboard — amd64/arm64/riscv64" \
      org.opencontainers.image.url="https://github.com/mmBesar/hs-dashboard" \
      org.opencontainers.image.source="https://github.com/mmBesar/hs-dashboard" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.authors="mmBesar"

COPY --from=builder /build/hs-dashboard /hs-dashboard

# Environment defaults — all overridable in compose
ENV PORT=8080 \
    HOST_PROC=/host/proc \
    HOST_SYS=/host/sys \
    CONFIG_DIR=/config \
    STATS_INTERVAL=5 \
    STATUS_INTERVAL=30

# Runs as nonroot user (distroless default) — no PUID/PGID needed
# Just works with any user since it reads /proc and /sys as read-only

EXPOSE 8080

ENTRYPOINT ["/hs-dashboard"]

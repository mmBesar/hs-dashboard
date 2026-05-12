# hs-dashboard — Dockerfile
# Single binary dashboard — serves static files and host metrics
# Supports: linux/amd64, linux/arm64, linux/riscv64
# github.com/mmBesar/hs-dashboard

# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.26-trixie AS builder

ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /build

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o hs-dashboard .

# ── Final stage ───────────────────────────────────────────────────────────────
FROM debian:trixie-slim

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

RUN useradd -r -s /sbin/nologin -M dashboard && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

COPY --from=builder /build/hs-dashboard /hs-dashboard

ENV PORT=8080 \
    HOST_PROC=/host/proc \
    HOST_SYS=/host/sys \
    CONFIG_DIR=/config \
    STATS_INTERVAL=5 \
    STATUS_INTERVAL=30

USER dashboard

EXPOSE 8080

ENTRYPOINT ["/hs-dashboard"]

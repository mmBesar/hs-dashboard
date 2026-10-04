# syntax=docker/dockerfile:1
#
# hs-dashboard image — tiny (~6 MB), a single static Go binary on "scratch".
# Builds for amd64, arm64 and riscv64 from one machine WITHOUT emulation (no QEMU):
# the Go compiler runs natively on the build machine and just targets the other CPU.

# ---- build stage: runs on the build machine's own architecture ----
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go config.jsonc ./
COPY web ./web
# CGO off = fully static binary, so it runs on scratch (no libc needed).
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/hs-dashboard .

# ---- final stage: just the binary ----
FROM scratch
COPY --from=build /out/hs-dashboard /hs-dashboard

# Defaults (override in compose). PUID/PGID/TZ are handled inside the binary:
#   PUID, PGID  -> the process switches to this user after start
#   TZ          -> timezone (database is built into the binary)
ENV PORT=8080 PUID=1000 PGID=1000 TZ=UTC

# The image has no shell or curl, so the binary checks itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/hs-dashboard", "-healthcheck"]

EXPOSE 8080
ENTRYPOINT ["/hs-dashboard"]

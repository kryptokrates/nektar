# Nektar v0.1 — Single-Binary Pure-Go Image (Zero Node.js runtime, Zero CGO, Zero Sidecars)

# Stage 1: Build Svelte 5 Dashboard assets natively on build host
FROM --platform=$BUILDPLATFORM node:24-alpine AS ui-build
WORKDIR /ui
COPY ui/package.json ui/package-lock.json* ./
RUN npm install --no-audit --no-fund
COPY ui/ ./
RUN npm run build

# Stage 2: Native Go 1.27 cross-compile for TARGETARCH (zero QEMU emulation slowdown)
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS go-build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum embed_adapters.go ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY integrations/hermes ./integrations/hermes
COPY integrations/openclaw ./integrations/openclaw
COPY skills/nektar-memory ./skills/nektar-memory
COPY --from=ui-build /ui/dist/ ./internal/server/ui_dist/
RUN CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH}" go build -trimpath -ldflags="-s -w" -o /out/nektar ./cmd/nektar

# Stage 3: Minimal Alpine runtime (~25 MB total, zero external sidecars)
FROM alpine:3.21
RUN apk add --no-cache ca-certificates wget \
 && adduser -D -u 10001 nektar \
 && mkdir -p /data /backups && chown nektar:nektar /data /backups
COPY --from=go-build /out/nektar /usr/local/bin/nektar
COPY integrations/hermes /usr/local/share/nektar/integrations/hermes
COPY integrations/openclaw /usr/local/share/nektar/integrations/openclaw
COPY skills/nektar-memory /usr/local/share/nektar/skills/nektar-memory
USER 10001:10001
ENV NEKTAR_DATA_DIR=/data NEKTAR_CONTAINER=1
EXPOSE 18420
VOLUME /data
LABEL io.modelcontextprotocol.server.name="io.github.kryptokrates/nektar"
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:18420/v1/health || exit 1
ENTRYPOINT ["/usr/local/bin/nektar"]
CMD ["serve"]

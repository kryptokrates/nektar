# Nektar v0.1 — Single-Binary Pure-Go Image (Zero Node.js runtime, Zero CGO, Zero node-gyp)

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

# Optional static restate-server binary for single-container durable workflows (pinned SHA-256)
ARG RESTATE_VERSION=1.8.1
RUN apt-get update && apt-get install -y --no-install-recommends xz-utils \
 && rm -rf /var/lib/apt/lists/* \
 && case "${TARGETARCH}" in \
      amd64) t=x86_64-unknown-linux-musl;  sha=56f12ccc54ee227657d4d8b070622849977444425184ee242a33e3ccc28bf745 ;; \
      arm64) t=aarch64-unknown-linux-musl; sha=7d30d4c73a6978f7b4e4a2c08163fbf4e5689c4534af013bb8fd37e003474857 ;; \
      *) echo "unsupported arch ${TARGETARCH}"; exit 1 ;; \
    esac \
 && name="restate-server-$t.tar.xz" \
 && base="https://github.com/restatedev/restate/releases/download/v$RESTATE_VERSION" \
 && curl -fsSL "$base/$name" -o /tmp/rs.tar.xz \
 && echo "$sha  /tmp/rs.tar.xz" | sha256sum -c - \
 && tar -xJf /tmp/rs.tar.xz -C /tmp \
 && cp "/tmp/restate-server-$t/restate-server" /out/restate-server \
 && chmod 755 /out/restate-server \
 && rm -rf /tmp/rs.tar.xz "/tmp/restate-server-$t"

# Stage 3: Minimal Alpine runtime (~35 MB total)
FROM alpine:3.21
RUN apk add --no-cache ca-certificates wget \
 && adduser -D -u 10001 nektar \
 && mkdir -p /data && chown nektar:nektar /data
COPY --from=go-build /out/nektar /usr/local/bin/nektar
COPY --from=go-build /out/restate-server /usr/local/bin/restate-server
COPY integrations/hermes /usr/local/share/nektar/integrations/hermes
COPY integrations/openclaw /usr/local/share/nektar/integrations/openclaw
COPY skills/nektar-memory /usr/local/share/nektar/skills/nektar-memory
USER 10001:10001
ENV NEKTAR_DATA_DIR=/data NEKTAR_CONTAINER=1
EXPOSE 18420 9080
VOLUME /data
LABEL io.modelcontextprotocol.server.name="io.github.kryptokrates/nektar"
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:18420/v1/health || exit 1
ENTRYPOINT ["/usr/local/bin/nektar"]
CMD ["serve"]

# Nektar v0.4 — Single-Binary Pure-Go Image (Zero Node.js runtime, Zero CGO, Zero node-gyp)

# Stage 1: Build Svelte 5 Dashboard assets
FROM node:24-alpine AS ui-build
WORKDIR /ui
COPY ui/package.json ui/package-lock.json* ./
RUN npm install --no-audit --no-fund
COPY ui/ ./
RUN npm run build

# Stage 2: Build static Go 1.27 binary with embedded UI + Rúnar covenant artifacts
FROM golang:1.27-bookworm AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY --from=ui-build /ui/dist/ ./internal/server/ui_dist/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/nektar ./cmd/nektar

# Optional static restate-server binary for single-container durable workflows
ARG RESTATE_VERSION=1.8.1
RUN arch="$(dpkg --print-architecture)" \
 && case "$arch" in amd64) t=x86_64-unknown-linux-musl ;; arm64) t=aarch64-unknown-linux-musl ;; *) echo "unsupported arch $arch"; exit 1 ;; esac \
 && name="restate-server-$t.tar.xz" \
 && base="https://github.com/restatedev/restate/releases/download/v$RESTATE_VERSION" \
 && curl -fsSL "$base/$name" -o /tmp/rs.tar.xz \
 && curl -fsSL "$base/$name.sha256" -o /tmp/rs.sha \
 && echo "$(cut -d' ' -f1 /tmp/rs.sha)  /tmp/rs.tar.xz" | sha256sum -c - \
 && tar -xJf /tmp/rs.tar.xz -C /tmp \
 && cp "/tmp/restate-server-$t/restate-server" /out/restate-server \
 && chmod 755 /out/restate-server \
 && rm -rf /tmp/rs.tar.xz /tmp/rs.sha "/tmp/restate-server-$t"

# Stage 3: Minimal Alpine runtime (~35 MB total)
FROM alpine:3.21
RUN apk add --no-cache ca-certificates wget \
 && adduser -D -u 10001 nektar \
 && mkdir -p /data && chown nektar:nektar /data
COPY --from=go-build /out/nektar /usr/local/bin/nektar
COPY --from=go-build /out/restate-server /usr/local/bin/restate-server
USER nektar
ENV NEKTAR_DATA_DIR=/data NEKTAR_HTTP_PORT=8080 NEKTAR_NETWORK=testnet
EXPOSE 8080 9080
VOLUME /data
LABEL io.modelcontextprotocol.server.name="io.github.nedzof/nektar"
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:8080/v1/health || exit 1
ENTRYPOINT ["/usr/local/bin/nektar"]
CMD ["serve"]

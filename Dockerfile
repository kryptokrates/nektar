# nektar MCP server image (offline demo chain; see docs/OPERATIONS.md for regtest).

# ZK prover (Wright §13 STARK): RISC Zero guest + host CLI, built reproducibly.
# The build FAILS if the program id differs from the pinned one.
FROM rust:1.94-bookworm@sha256:6ae102bdbf528294bc79ad6e1fae682f6f7c2a6e6621506ba959f9685b308a55 AS stark
ENV RISC0_HOME=/opt/risc0 PATH=/opt/risc0/bin:/root/.risc0/bin:$PATH
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
# the rzup installer executes with build-machine privilege — verify its bytes
# against a committed digest before running; an upstream change fails the
# build loudly instead of executing unseen code
ARG RISC0_INSTALL_SHA256=e0a0c1ab3a86ce33efc566a0c9a24ac5eba688769d124d3a510337853b051a3c
# rzup resolves risc0 releases over the GitHub API — unauthenticated calls
# share a 60/hr IP budget and GH runners flake on it (403 mid-build).
# GITHUB_TOKEN mounts as a build secret (never lands in a layer); absent
# locally → falls back to the anonymous path as before. The type=gha layer
# cache in CI makes this layer a restore on repeat runs anyway.
RUN --mount=type=secret,id=GITHUB_TOKEN,required=false \
    GITHUB_TOKEN="$(cat /run/secrets/GITHUB_TOKEN 2>/dev/null || true)" && export GITHUB_TOKEN; \
    for i in 1 2 3 4 5; do \
      curl -fsSL https://risczero.com/install -o /tmp/rzup-install.sh \
      && echo "$RISC0_INSTALL_SHA256  /tmp/rzup-install.sh" | sha256sum -c - \
      && bash /tmp/rzup-install.sh \
      && rzup install rust 1.97.0 \
      && rzup install cargo-risczero 3.0.3 \
      && rzup install r0vm 3.0.3 \
      && rm /tmp/rzup-install.sh && exit 0; \
      echo "risc0 install attempt $i failed — retrying"; sleep $((i*30)); \
    done; exit 1
WORKDIR /stark
COPY tools/stark/Cargo.toml tools/stark/Cargo.lock ./
COPY tools/stark/host ./host
COPY tools/stark/methods ./methods
COPY packages/zk/stark/image-id.json /pinned.json
RUN cargo build --release -p nektar-stark \
 && id=$(./target/release/nektar-stark image-id) \
 && grep -q "\"$id\"" /pinned.json || { echo "STARK image id $id != pinned"; exit 1; } \
 && cp ./target/release/nektar-stark /usr/local/bin/ && cp "$(readlink -f "$(which r0vm)")" /usr/local/bin/r0vm \
 && strip /usr/local/bin/nektar-stark /usr/local/bin/r0vm || true # symbols only — perf unchanged

FROM node:24-slim@sha256:dc26ae92c9fb15cf6b1d63e20f0944b2efe0f115b20e00be48fb23fceb061c54 AS build
# sqlite3 falls back to node-gyp when prebuild-install can't fetch a binary;
# slim has no toolchain and sqlite3's bundled node-gyp 8.x can't parse Node 24.
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends python3 make g++ \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g node-gyp@11.5.0
ENV npm_config_node_gyp=/usr/local/lib/node_modules/node-gyp/bin/node-gyp.js
WORKDIR /app
RUN corepack enable && corepack prepare pnpm@12.16.1 --activate
COPY pnpm-lock.yaml pnpm-workspace.yaml package.json tsconfig.base.json ./
# patchedDependencies resolve at install time — the patch files must land
# before pnpm install or the frozen-lockfile gate dies ERR_PNPM_PATCH_NOT_FOUND
COPY patches ./patches
COPY packages ./packages
COPY vendor ./vendor
COPY vectors ./vectors
COPY tools/cedar-symcc/policies ./tools/cedar-symcc/policies
RUN pnpm install --frozen-lockfile && pnpm -r build && pnpm reproduce \
 && pnpm deploy --filter @nektar/mcp --prod /prod

# slim runtime — pnpm deploy output is the mcp package + runtime-only deps:
# no devDependencies, no TypeScript, no tests, no source. The workspace's
# @nektar/* deps land inside /prod/node_modules per each package's `files`.
FROM node:24-slim@sha256:dc26ae92c9fb15cf6b1d63e20f0944b2efe0f115b20e00be48fb23fceb061c54
WORKDIR /app
COPY --from=build /prod /app
COPY --from=build /app/packages/mcp/demo /app/demo
COPY --from=stark /usr/local/bin/nektar-stark /usr/local/bin/r0vm /usr/local/bin/
ENV NEKTAR_STARK_PROVER=/usr/local/bin/nektar-stark
RUN mkdir -p /data && chown node:node /data
# hadolint ignore=DL3066
USER node
# NEKTAR_NETWORK=regtest (persistent in /data; needs NEKTAR_FUND_WIF +
# NEKTAR_RPC_PASS, Teranode reachable via host.docker.internal) | ttn | main
ENV NEKTAR_MODEL_ID=unspecified NEKTAR_BATCH=4 NEKTAR_NETWORK=regtest NEKTAR_DATA_DIR=/data NEKTAR_TOOL_PROFILE=operator
VOLUME /data
LABEL io.modelcontextprotocol.server.name="io.github.nedzof/nektar"
# health = a full scripted MCP session against a fresh server process
# hadolint ignore=DL3025
HEALTHCHECK --interval=5m --timeout=60s CMD node demo/client.mjs > /dev/null || exit 1
ENTRYPOINT ["node", "dist/src/bin.js"]

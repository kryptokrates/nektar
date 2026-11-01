# mamem MCP server image (offline demo chain; see docs/OPERATIONS.md for regtest).

# ZK prover (Wright §13 STARK): RISC Zero guest + host CLI, built reproducibly.
# The build FAILS if the program id differs from the pinned one.
FROM rust:1.94-bookworm AS stark
ENV RISC0_HOME=/opt/risc0 PATH=/opt/risc0/bin:/root/.risc0/bin:$PATH
RUN curl -fsSL https://risczero.com/install | bash && rzup install rust 1.97.0 && rzup install cargo-risczero 3.0.3 && rzup install r0vm 3.0.3
WORKDIR /stark
COPY tools/stark/Cargo.toml tools/stark/Cargo.lock ./
COPY tools/stark/host ./host
COPY tools/stark/methods ./methods
COPY packages/zk/stark/image-id.json /pinned.json
RUN cargo build --release -p mamem-stark \
 && id=$(./target/release/mamem-stark image-id) \
 && grep -q "\"$id\"" /pinned.json || { echo "STARK image id $id != pinned"; exit 1; } \
 && cp ./target/release/mamem-stark /usr/local/bin/ && cp "$(readlink -f "$(which r0vm)")" /usr/local/bin/r0vm

FROM node:24-slim AS build
WORKDIR /app
RUN corepack enable && corepack prepare pnpm@12.16.1 --activate
COPY pnpm-lock.yaml pnpm-workspace.yaml package.json tsconfig.base.json ./
COPY packages ./packages
COPY vectors ./vectors
COPY tools/cedar-symcc/policies ./tools/cedar-symcc/policies
RUN pnpm install --frozen-lockfile && pnpm -r build && pnpm reproduce

FROM node:24-slim
WORKDIR /app
COPY --from=build /app /app
COPY --from=stark /usr/local/bin/mamem-stark /usr/local/bin/r0vm /usr/local/bin/
ENV MAMEM_STARK_PROVER=/usr/local/bin/mamem-stark
RUN mkdir -p /data && chown node:node /data
USER node
# MAMEM_NETWORK=mock (zero setup) | regtest (persistent in /data; needs
# MAMEM_FUND_WIF + MAMEM_RPC_PASS, Teranode reachable via host.docker.internal)
ENV MAMEM_MODEL_ID=unspecified MAMEM_BATCH=4 MAMEM_NETWORK=mock MAMEM_DATA_DIR=/data
VOLUME /data
LABEL io.modelcontextprotocol.server.name="io.github.nedzof/mamem"
# health = a full scripted MCP session against a fresh server process
HEALTHCHECK --interval=5m --timeout=60s CMD node packages/mcp/demo/client.mjs > /dev/null || exit 1
ENTRYPOINT ["node", "packages/mcp/dist/src/bin.js"]

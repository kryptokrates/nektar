# mamem MCP server image (offline demo chain; see docs/OPERATIONS.md for regtest).
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
USER node
ENV MAMEM_MODEL_ID=unspecified MAMEM_BATCH=4
# health = a full scripted MCP session against a fresh server process
HEALTHCHECK --interval=5m --timeout=60s CMD node packages/mcp/demo/client.mjs > /dev/null || exit 1
ENTRYPOINT ["node", "packages/mcp/dist/src/bin.js"]

# Hub images — everything deploy/hub/compose.yaml used to mount from a repo
# checkout, baked into derived images so the compose is fully self-contained.
#
#   docker build -f deploy/images/hub.Dockerfile --target bbox-host .
#
# Build context is the repo ROOT (the module stage needs the workspace).
# Targets: bbox-host, shim, hub-db.

# nektar overlay module — a gitignored esbuild artifact, produced here so the
# image carries the exact module the repo pins (no local build step needed)
FROM node:24-bookworm AS module
ENV CI=true
RUN corepack enable && corepack prepare pnpm@12.16.1 --activate
WORKDIR /app
COPY pnpm-lock.yaml pnpm-workspace.yaml package.json tsconfig.base.json ./
# patchedDependencies resolve at install time — same gate as the main image
COPY patches ./patches
COPY packages ./packages
RUN pnpm install --frozen-lockfile --filter '@nektar/bstack...' \
 && pnpm --filter '@nektar/bstack...' build \
 && pnpm --filter @nektar/bstack bundle

# market host — upstream engine + the modules compose used to bind-mount
FROM ghcr.io/lightwebinc/bbox-host@sha256:9e3579b24e35e853df3cf194bee33a1355e17f3c480fb0ccf9e17cd13997d0df AS bbox-host
COPY --from=module /app/packages/bstack/bundle /app/modules/nektar
COPY deploy/bstack-sandbox/modules/bbox/bbox-module.js /app/modules/bbox/bbox-module.js
COPY deploy/bstack-sandbox/modules/blogs /app/modules/blogs
COPY deploy/bstack-sandbox/modules/finger /app/modules/finger

# header/watch shims — the two sidecars compose ran off bind-mounted files
FROM node:22-alpine AS shim
WORKDIR /shim
COPY deploy/bstack-sandbox/header-shim.mjs deploy/bstack-sandbox/watch-shim.mjs ./
CMD ["node", "header-shim.mjs"]

# overlay db — the schema init compose mounted from the checkout
FROM mysql@sha256:df90258da46f6e7ff8574ff84f17618aba0765cac749928299c69fd4e7cb8ad8 AS hub-db
COPY deploy/bstack-sandbox/initdb /docker-entrypoint-initdb.d

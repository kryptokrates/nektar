---
name: deploy-audit
description: >
  Use when auditing or changing anything a stranger runs to deploy — Helm
  charts, compose files, install scripts, .env examples, Dockerfiles,
  release workflows and the docs that name the artifacts. Trigger surfaces:
  a new service/sidecar/container in any deploy surface, a new env var or
  port, a new PVC/volume, a release asset rename, "why does it crash on a
  fresh box", "make deploy easier", "does the chart still work", "port
  collision / EADDRINUSE in a pod", "image won't pull / crashes on boot".
  The deliverable is a gate list — every check is a yes/no against the
  rendered artifacts, never the source YAML alone. For user-journey friction
  theory use friction; for runtime topology use arch-review; for the
  executor plane itself use restate.
---

# deploy-audit — audit what the stranger runs, not what we wrote

Deploy surfaces rot in **three directions the code review never sees**:
what the image defaults imply (a sidecar's default port colliding with the
app's own port inside a pod netns), what the surfaces disagree about
(compose carries the sidecar, helm doesn't), and what the release promise
says (`latest` tag = last published image, which may predate the deploy
files' assumptions entirely). Every check here runs against **rendered
artifacts** — `helm template`, `docker compose config` — because the bug
lives in the expansion, not the template.

Bug classes this skill exists because they shipped:

- two containers in one pod both wanting `:8080` (agent HTTP vs Restate
  ingress default) — renders clean, dies `EADDRINUSE` at runtime
- sidecar added to compose but not helm — a "required" component silently
  absent on one surface
- `image: latest` pointing at a release that predates the feature the
  sidecar exists for — the pod runs 2/2, dead wiring
- Dockerfile baking `ENV NEKTAR_NETWORK=regtest` — kills profile
  precedence inside every container, undetectable outside it
- release workflow renamed the asset, docs still told users the old name

## Gate 1 — render every surface

Never audit source YAML. Render all of it:

```sh
helm template nektar deploy/agent/helm --set <every required value>
docker compose -f deploy/agent/compose.yaml config
```

If a surface can't render without secrets, that's a finding only when the
fail isn't loud — `required()`/`${VAR:?}` gates are correct; a silent
default is not.

## Gate 2 — port-space collision (the netns gate)

For **each pod**: collect every port any container binds — `containerPort`,
`*_PORT`-shaped env values, AND the **known-image default table** below.
Containers in one pod share one localhost: any duplicate port across the
pod's containers is a BLOCKER. `containerPort` is documentation only — the
bind is what the process does, so image defaults and env vars count.

| image | default binds |
|---|---|
| `restatedev/restate` | ingress 8080, admin 9070, node 5122 |
| `postgres` | 5432 |
| `redis`/`valkey` | 6379 |
| `nginx`/`traefik`/`caddy` | 80, 443 |
| `grafana`/`prometheus` | 3000 / 9090 |
| nektar agent | `NEKTAR_HTTP_PORT` (default 8080), endpoint `NEKTAR_RESTATE_ENDPOINT_URI` port (9080) |

Compose is the mirror-image check: services get separate IPs so app-port
duplicates are fine, but `ports:` host publishes share the host — flag
duplicate host-side publishes.

## Gate 3 — loopback & DNS traps

`127.0.0.1`/`localhost`/`host.docker.internal` in any env/URI value:

- inside one **pod** — correct (shared netns)
- in **compose** envs pointing at another service — BLOCKER, must be the
  service name
- as an **advertised endpoint URI** the sidecar dials back — reachable only
  if both ends share the netns; `host.docker.internal` is a known-flaky
  answer (IPv6 ULA on Docker Desktop) — prefer service DNS or pod loopback

## Gate 4 — sidecar parity & wiring

Every deploy surface must carry the required sidecar AND the envs that
point the app at it — presence without wiring is dead weight (we shipped a
pod running 2/2 with an idle sidecar because the image predated the
feature). Both directions:

- container present in pod/compose
- app envs reference it (`NEKTAR_RESTATE_*`) AND the sidecar's env/args
  match what those envs expect (port, data dir)

Separate durable stores get separate volumes/PVCs — never one claim
serving two journals. StatefulSet RWO claims → `local-path` on k3s,
`hcloud-volumes` on multi-node Hetzner.

## Gate 5 — secrets & env parity

- required secrets gated (`required` / `:?`), never defaulted
- `secretKeyRef`, never env literals; `_FILE` variants where the app
  supports them
- `.env.example` ↔ every env var the surface consumes — drift both ways
  is a finding
- Dockerfile `ENV` for anything profile/network selection: B LOCKER — it
  masquerades as a user export and beats the app's own precedence

## Gate 6 — release alignment

- workflow assets ↔ what install docs/READMEs name — 1:1, no stale names
- image tags in surfaces ↔ what CI actually publishes; `latest` means
  "last tagged release", which may predate today's deploy files — pin or
  document the skew
- unpinned third-party images (`:latest` on restate/postgres) — finding;
  pin to a tested tag
- chart `version`/`appVersion` bumped when templates change

## Gate 7 — fail-fast & friction

- every required var fails at render/boot with an actionable message,
  not a stack trace (helm `required`, compose `:?`, install.sh checks)
- one-command path exists and is the FIRST thing the README says
- personas ↔ assets 1:1 (local = mcp-bundle, VPS = deploy bundle)

## Gate 8 — boot it

Static gates catch the shape; only a boot catches the rest. Cheapest
proof: `k3d cluster create` + render + `kubectl apply` + `2/2 Running` +
the app's own log line proving the wiring (e.g. `rt: deployment
registered`). `compose up` on a scratch project for the compose surface.
A gate you can't boot is a hypothesis.

## Standard findings format

`GATE n — [BLOCKER|HIGH|MED] — surface — what's wrong — the fix`. Static
gates first, boot results last. A finding without the rendered line that
proves it is a guess — cite the file:line.

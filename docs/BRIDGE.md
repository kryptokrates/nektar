# overlay-bridge — running nektar presence on the multicast plane

The rv's presence engine (`tm_presence`/`ls_presence`) already serves the
exact submit interface the bridge fronts. When a 1BSV object-plane ingress
is available, [lightwebinc/overlay-bridge](https://github.com/lightwebinc/overlay-bridge)
runs beside `serve.mjs` — our engine stays unmodified.

```
   delivery (down)
   1BSV fabric ══push══▶ overlay-bridge
       object lane ─▶ feed ─▶ POST /submit (x-topics: tm_presence) ─▶ nektar engine
       header lane ─▶ chaintracks ◀── engine's ChaintracksTracker

   publication (up)
   client ─▶ facade (POST /submit) ─┬─▶ engine (admits + indexes)
                                    └─▶ up-tunnel ══▶ fabric ══▶ every subscribed rv
```

## The contract we already implement

- `/overlay/submit {beef, topics:[tm_presence]}` — the object-plane submit shape
- `ls_presence.announce*` — signature-verified intake (the engine side)
- `NEKTAR_PEERS` — today's HTTPS federation; the bridge replaces it
- `NEKTAR_BRIDGE_INGRESS` — when set, `serve.mjs` logs that plane delivery
  arrives via the bridge feed (not HTTP submits); our emit stays compatible
- `/events/delivery?seq=` — BRC-149 delivery record (binary), the consumer shape

## What unblocks it

A 1BSV application account (early access) — the anycast ingress door + delivery
edges. Until then `NEKTAR_PEERS` federation is the transport; the artifacts and
interfaces are already plane-shaped.

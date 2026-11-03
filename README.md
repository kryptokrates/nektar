# mamem — an AI memory it can't silently rewrite

An append-only memory for AI agents, sealed on the BSV blockchain. Anyone
can later check *what the AI knew, when it knew it, and who was allowed to
see it* — without trusting whoever ran the machine.

[![ci](https://github.com/nedzof/merkle-automaton/actions/workflows/ci.yml/badge.svg)](https://github.com/nedzof/merkle-automaton/actions/workflows/ci.yml)
[![release](https://github.com/nedzof/merkle-automaton/actions/workflows/release.yml/badge.svg)](https://github.com/nedzof/merkle-automaton/actions/workflows/release.yml)
License: [Apache-2.0](LICENSE)

## The walkthrough — one question, end to end

What actually happens when someone asks your agent
*"what torque does drain port 4 need?"* — and exactly which parts of the
answer are backed by cryptography versus by trust.

**1. Earlier — the AI learned something**

```sh
remember("drain port 4 torque 45Nm", shard="manual")
```

→ an encrypted record in the local log, provenance-signed by the log key.
**Backed:** the record exists, is signed, and is tamper-evident.

**2. The log got stamped**

```sh
anchor(shard)   # one BSV transaction commits the Merkle root R
```

→ and each anchor *spends the previous anchor* (a Rúnar covenant) — one
unforkable chain per shard, enforced by consensus, not by the backend.
**Backed:** the record can no longer be edited or deleted without the
on-chain stamp breaking.

**3. The AI recalls it**

```sh
recall("manual")   # → items + a signed AnswerReceipt
```

→ the record text **plus** a receipt binding
`answer ‖ refs ‖ check ‖ model ‖ time` under the log key's signature.
Access still goes through identity, ZK clearance, Cedar and the policy
DAG first. **Backed:** what the AI claims to have read is committed.

**4. You check the receipt yourself**

```sh
prove("manual", 0)   # → proof bundle → verifyFragment()
```

→ Merkle path leaf→`R`, the anchor chain, the BEEF — verified **offline
against public block headers**, no trust in the server that served it.
Optionally a STARK proves "some leaf is in the anchored tree" without
revealing which. **Backed:** the record was recorded, unaltered, before
its block time.

**5. The answer itself gets gated**

```sh
groundedAnswer(claims=[{text:"45Nm", cites:[{shard:"manual",seq:0}]}])
```

→ every claim must cite a **verified** fragment and be a literal quote or
Datalog-entailed. Whatever fails is **stripped** — an underivable answer
is refused, not published with a disclaimer. The surviving answer is
anchored as a receipt. **Backed:** per-claim derivation from sealed
memory — Wright's "no fragment may be hallucinated."

**6. If someone disputes the proof**

→ the prover posts a small **bonded claim** on-chain; a watcher bot
re-verifies the STARK off-chain for free and challenges lies; any dispute
bisects the fraud to one Poseidon2 step judged by an on-chain contract
(~10 cents, paid only by liars). Settled claims mint a **verdict token**
that a sale contract can require — **pay-iff-proven**.

## The honest cut — what's backed and what isn't

```
someone's question
    └─► the AI's answer
        ├─ cited claim "45Nm"        BACKED — derivable from a sealed record
        ├─ receipts, manifest, chain  BACKED — anchored, signed, verifiable
        ├─ the prose between cites    NOT — the gate checks claims, not style
        ├─ "the model produced this"  DECLARED — manifest anchored; the
        │                                    inference itself is unproven
        └─ truth of the record        NEVER — sealing proves recorded, not right
```

**One sentence:** everything the AI *quotes* can be proven to come from an
untamperable log; how it *phrases* it and whether the *record was true*
remain unproven — and the system says so explicitly rather than implying
otherwise.

**Still open, honestly:** proving the weights actually produced a token
needs zkML/TEE (a research field, deliberately not built — `modelManifest`
anchors the *declared* identity instead); agents use the gate by
instruction, not by force; and a clever paraphrase can fail grounding and
get stripped — that's the safety feature, not a bug.

## What this enables that doesn't exist today

- **Auditable AI** — "what did it know, and when" checkable years later
  with no server access. Today's audit trail is a mutable database + a promise.
- **Agents trusting strangers** — two agents owned by *different people*
  share memory; each reader verifies inclusion + authorization itself.
- **Selling verified answers** — payment releases only if the math checks
  out. No escrow company, no disputes.
- **Zero-knowledge clearance** — prove "a record satisfying policy P
  exists" without revealing the record.
- **Court-grade evidence** — a sealed timestamped chain vs. screenshots
  of logs.
- **Model/data integrity** — anchor weights+dataset digests: "which model
  produced this answer" becomes provable.

## Quickstart — see it work in 3 minutes

Prereqs: Docker Desktop with the local Teranode regtest stack
(`infra/teranode-quickstart`), Node ≥ 22, pnpm.

```sh
git clone https://github.com/nedzof/merkle-automaton.git && cd merkle-automaton
pnpm install && pnpm -r build

# the full story through the real MCP server — remember → anchor →
# STARK prove → bonded claim → watcher → settle → verdict → gated sale:
node packages/mcp/demo/e2e-mcp-regtest.mjs        # prints E2E MCP PASS

docker compose up -d watcher                       # the honest challenger
node packages/e2e/src/e2e-watcher-regtest.mjs      # fraud demo: a liar gets caught
```

**Don't want Docker?** The testnet path below needs no local node.

## Run your own instance (public testnet, ~10 min)

No Docker, no node, no indexers — public broadcaster + headers + tx fetch.

```sh
export MAMEM_NETWORK=testnet
export MAMEM_KEY_PASSPHRASE='<pick one — encrypts keys at rest>'
export MAMEM_DATA_DIR=$PWD/.mamem-testnet

node packages/mcp/dist/src/bin.js     # prints your fee address, e.g. mnKcrZ…
```

Fund that address — [faucet.bananablocks.com](https://faucet.bananablocks.com)
or any tBSV wallet → get a **txid** → internalize it (tracked-set wallet:
no scanning, no address index — you hand it the funding tx once):

```sh
MAMEM_FUND_TXIDS=<txid> node packages/mcp/dist/src/bin.js
```

**Wire Claude Code:**

```sh
claude mcp add mamem --scope user \
  --env MAMEM_NETWORK=testnet --env MAMEM_KEY_PASSPHRASE=<pw> \
  --env MAMEM_DATA_DIR=<abs>/.mamem-testnet \
  -- node <abs>/packages/mcp/dist/src/bin.js
```

Then just ask: *"remember X, anchor it, recall it, prove it."* The first
`prove` returns `{ok:true, settled:false, anchorState:'broadcast'}` —
anchored at 0-conf, upgrades to `mined` by itself.

Other clients (Claude Desktop, Cursor, remote HTTP + bearer token,
`mamem-init` zero-touch wallet, Docker MCP Toolkit, local models) →
**[docs/INTEGRATION.md](docs/INTEGRATION.md)**.

## Find each other — the rendezvous directory

Agents aren't findable on-chain by design; "finding" = knowing an
endpoint. A shared rendezvous (`tm_presence`/`ls_presence` overlay topic)
lets agents publish a signed "I'm here" ad `{pub, endpoint, shards}` —
the signature binds endpoint to the agent's key, so listings can't be
forged.

```sh
# host one (any always-on box):
MAMEM_OVERLAY_HOST=0.0.0.0 MAMEM_OVERLAY_PORT=8090 pnpm rendezvous

# announce your agent (free — no UTXO, the signature is the auth):
MAMEM_AGENT_WIF=<key> MAMEM_AGENT_ENDPOINT=http://<you>:8080/mcp \
  MAMEM_AGENT_SHARDS=manual,tnet MAMEM_OVERLAY_URL=http://<rendezvous>:8090 \
  pnpm rendezvous:announce

# discover peers:
curl http://<rendezvous>:8090/directory?shard=tnet
```

Announce → get a peer's endpoint+pubkey → connect via
`claude mcp add --transport http … --header "Authorization: Bearer …"` →
verify its receipts against that pubkey yourself. **The directory finds
peers; it never vouches for them** — trust comes from the receipts.

## Honest status

- ✅ Live on regtest + public testnet with real transactions (anchor
  chains, STARK prove/verify, claims, verdicts, gated sales, rendezvous).
- ⚠️ One liveness assumption: a false claim stands only if **no honest
  watcher** challenges it in the window. Everything else is math.
- ⚠️ Mainnet is gated (`MAMEM_MAINNET_CONFIRM` + spend caps), not yet a
  pilot. No security audit.
- Proofs cover *inclusion, ordering, authorization, existence-by-time* —
  never that a memory's content is **true**.
- **Per-answer receipts**: `groundedAnswer` strips every claim that isn't
  entailed by a verified fragment and anchors an answer receipt (Wright
  §8). "Which model answered" is a *declared* manifest (`modelManifest`),
  not a proof of inference — that needs zkML/TEE, deliberately not built.

## Documentation

| Doc | Contents |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | layers, packages, diagrams, ADRs |
| [docs/CLAIMS.md](docs/CLAIMS.md) | the claim layer in full — contracts, bisection, watcher |
| [docs/INTEGRATION.md](docs/INTEGRATION.md) | every MCP client option, transports, runtime modes |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | deployment, funding, gates, troubleshooting |
| [docs/PROOFS.md](docs/PROOFS.md) | what each proof does and doesn't prove |
| [docs/THREAT-MODEL.md](docs/THREAT-MODEL.md) | STRIDE per component, residual risks |
| [docs/GOAL-v9.md](docs/GOAL-v9.md) | current goal state + honest deferrals |
| [BENCHMARKS.md](BENCHMARKS.md) | measured performance |

Packages: `core` `chain` `log` `contracts` `anchor` `overlay` `verify`
`crypto` `identity` `zk` `datalog` `graph` `policy` `automaton` `mcp`
`reasoning` `watcher` `fuzz` `e2e` — each under `packages/`.

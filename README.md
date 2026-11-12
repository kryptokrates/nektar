<p align="center">
  <img src="logo.png" width="96" alt="nektar"/>
</p>

# nektar — the computational data market

**The knowledge moves. The money moves. The providers compete.**

[![ci](https://github.com/kryptokrates/nektar/actions/workflows/ci.yml/badge.svg)](https://github.com/kryptokrates/nektar/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/kryptokrates/nektar)](https://github.com/kryptokrates/nektar/releases)
[![license](https://img.shields.io/badge/license-MIT-green)](LICENSE)
[![ghcr](https://img.shields.io/badge/ghcr.io-kryptokrates%2Fnektar-f5b301)](https://github.com/kryptokrates/nektar/pkgs/container/nektar)

Your agent solved the problem. Another agent is about to pay to solve it again.

Nektar gives agents a way to keep useful knowledge, offer it for sale, buy it from each other, and check the evidence behind the exchange. Memory becomes a product with an identity, provenance, a price, and a delivery record.

**Keep what your agent learns. Sell what another agent needs. Carry the proceeds into the next transaction.**

> **MVP:** memory, receipts, paid disclosure, and policy-controlled trading are implemented. The wider economy of interchangeable storage, compute, and verification providers is the market vision. A common payment asset makes cross-service spending possible where providers accept it; it does not make that ecosystem exist by itself.

[Start locally](#start-locally) · [Use with an agent](#use-with-an-agent) · [What works today](#what-works-today) · [Operating guide](docs/OPERATIONS.md)

## Knowledge should outlive the session

A coding agent discovers how to fix a broken migration. Today, that discovery can end with the chat. The next agent starts over.

With Nektar, the first agent can retain the finding and offer it to a buyer. The buyer sets a maximum price. Sellers respond. The runtime checks offers, selects within the approved policy, locks the payment, and checks the disclosed record against its commitment.

The seller's payment path reveals the committed record. If the seller never completes, the buyer has a refund path after the deadline, provided the payment remains unspent.

The buyer now has a record it can inspect and import with provenance. The seller has been paid in BSV. Both have evidence of the exchange.

```mermaid
flowchart LR
    A[Agent keeps a useful finding] --> B[Buyer publishes a need and budget]
    B --> C[Sellers submit offers]
    C --> D[Buyer selects under approved policy]
    D --> E[Payment locked on BSV]
    E --> F[Seller reveals committed record and collects]
    F --> G[Buyer verifies the disclosed record]
    E --> H[No completion: refund path after deadline]
```

Verification establishes what was committed and delivered. It does not establish that the finding is true, useful, or the best available answer.

## One product. Many competing services.

A commodity market needs a defined product. For agent memory, that means identifying the record or pack, its origin, its terms, and the evidence available to a buyer. Different knowledge remains different knowledge.

Once that product can travel, the services around it can compete:

| Service | What the customer can compare |
|---|---|
| Knowledge creation | Relevance, originality, freshness, evidence |
| Storage | Price, retention, availability, retrieval |
| Discovery | Coverage, matching, useful offers |
| Delivery | Reach, speed, reliability |
| Verification | Checks performed, assurance, cost |
| Compute | Capability, performance, privacy, price |

This is the direction: an agent earns from knowledge, then spends those proceeds on storage, compute, or verification supplied by another business. Different providers can accept the same settlement asset. Changing supplier need not require changing money.

Nektar settles its current memory trades in BSV. The broader service market requires participating providers, compatible interfaces, and explicit service terms. Provider migration can still involve moving data and changing contracts.

## What works today

| Capability | What the implementation provides |
|---|---|
| Persistent memory | Encrypted, provenance-signed records with commitments and BSV anchoring; export/import paths that recheck available evidence. |
| Answers with receipts | Claims tied to cited records; unsupported claims are stripped by `groundedAnswer`; receipts can be independently checked. |
| Autonomous memory trades | Signed wants and offers, competing quotes, policy-based selection, funded disclosure sales, delivery verification, and deadline refunds. |
| Operator-controlled budgets | Approved domains, prices, fees, and exposure limits for the automation runtime; exceptions go to operator approval. |
| Several delivery hosts | Signed public records and recipient-encrypted messages sent to configured hosts and checked locally on read; identity records can advertise counterparty hosts. |
| Buyer and seller recovery | Encrypted inbox notes can rebuild escrowed purchases and quoted sales after loss of the local deal database, with the required keys, memory, policies, and notes retained. |
| Deal journals | Numbered, linked batches; missing batches can be fetched from another host, unresolved gaps reported, and recorded intervals checked against mined commitments. |
| Evidence storage | Optional S3-compatible storage for redacted trade evidence and public packs, with downloaded content checked against its recorded digest. |

The agent profile exposes nine tools: `remember`, `recall`, `groundedAnswer`, `verifyReceipt`, `want`, `sell`, `deals`, `fund`, and `doctor`. Additional operator tools are available through the operator profile.

## Start locally

Requires **Node.js 22.12 or newer** and **pnpm**.

```bash
git clone https://github.com/kryptokrates/nektar.git
cd nektar
pnpm install
pnpm -r build
pnpm demo
```

The demo uses an offline mock chain. It stores example memory, anchors it, removes an unsupported claim from an answer, and prints a receipt. It does not demonstrate public-network settlement.

For an instance of your own, run the configuration wizard:

```bash
pnpm init:nektar
```

The wizard writes configuration and prints the next command for the selected network. Your agent's data directory and passphrase are part of its identity; preserve them.

## Use with an agent

Start a local HTTP instance on port 8080 as described by the wizard, then connect your client:

```bash
pnpm setup:codex
# or
pnpm setup:claude
```

Memory and receipt tools work without a marketplace. Trading additionally needs a funded instance, reachable market hosts, the automation runtime, paid disclosure enabled, and an operator-approved policy. Follow the [automation setup](docs/AUTOMATION.md#autonomous-marketplace--approve-a-policy-once-then-publish-wants--enable-offers).

Once configured, the market surface is small. These are MCP tool calls, not shell commands:

**Seller — offer a shard you already hold:**

```json
{"tool":"sell","arguments":{"shard":"migration-fixes","domains":["libfoo"],"priceSats":200,"desc":"libfoo migration findings"}}
```

**Buyer — publish a need and a price ceiling:**

```json
{"tool":"want","arguments":{"domains":["libfoo"],"question":"Why does libfoo.connect fail after the 3.0 upgrade?","maxSats":500}}
```

**Either agent — inspect the resulting operations:**

```json
{"tool":"deals","arguments":{}}
```

The runtime advances the trade. The operator defines the limits and handles exceptions. See [automation](docs/AUTOMATION.md) for configuration, funding, recovery, and approval details.

## Run the scenarios

```bash
# Separate agent processes and two local hosts; simulated chain:
pnpm scenarios -- --profile mock --scenario all

# Four production instances, three sellers, two reference hosts;
# requires the Teranode regtest stack, host setup, and funded treasury:
pnpm scenarios:real
```

The regtest runner lives in [`real-stack.mjs`](packages/e2e/src/scenarios/real-stack.mjs); read its prerequisites before running it. A local regtest run and a public testnet run establish different things. The [operating guide](docs/OPERATIONS.md) covers the chain and host setup; the [automation notes](docs/AUTOMATION.md) record verification status and limitations.

## The boundaries matter

- **Evidence is not truth.** Signatures, commitments, inclusion proofs, and derivations establish recorded structure and attribution. Model identity is declared unless separately attested; a receipt is not proof of correct inference.
- **Payment conditions are specific.** Disclosure binds payment to revealing committed data. It does not guarantee a useful answer, and a completed valid sale is not refundable merely because the buyer dislikes it. Broadcast acceptance and mined confirmation are distinct states.
- **Recovery has prerequisites.** Recovery notes do not restore lost keys, source memory, policies, or budgets. Notes must remain available and unexpired. Unknown payment states and repeated refused refunds can require intervention.
- **Budget scope is explicit.** Automation policy caps govern automation spending. Manual owner tools are recorded but are not all constrained by those caps.
- **Hosts remain services.** Current automation uses HTTP(S) submissions to configured hosts. Verification does not guarantee availability, anonymity, or a complete view of every possible seller. Encrypted content still has metadata.
- **A journal covers recorded history.** A visible internal gap can be detected. Unrecorded events and a withheld latest tail are different problems.
- **Storage integration is not a storage market.** Nektar stores and checks evidence objects; provider possession challenges, competitive storage payouts, and gateway availability are not guarantees supplied by this integration.

## Build, operate, contribute

- [Automation and verification status](docs/AUTOMATION.md)
- [Deployment and operations](docs/OPERATIONS.md)
- [Proofs](docs/PROOFS.md) and [threat model](docs/THREAT-MODEL.md)
- [Architecture](docs/ARCHITECTURE.md) and [developer guide](docs/DEVELOPER.md)

For maintenance: `pnpm doctor:fix`, `pnpm config:nektar`, `pnpm update:nektar`, and `pnpm service install`. Inspect the operating guide before changing a live instance. For development: `pnpm -r build` and `pnpm -r test`.

The research foundations include Craig Wright's [*On Immutable Memory Systems for Artificial Agents*](https://arxiv.org/abs/2506.13246) and [*One Token, Many Markets*](https://singulargrit.substack.com/p/one-token-many-markets). Nektar applies persistent, verifiable memory to agent exchange; the portable-service economy above is its market thesis.

[MIT](LICENSE).

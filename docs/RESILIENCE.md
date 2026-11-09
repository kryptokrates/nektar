# Nektar resilience — no single point, no central capture

How the architecture answers "one DNS name + one port = DoS target + traffic
capture point." Agents are outbound-only; every signed object is verifiable
through any surviving host.

## The architecture

```mermaid
flowchart TB
    subgraph AGENTS["Agents — outbound connections only, no inbound port"]
        AL["Alice<br/>MCP server (local)<br/>identity key + mailbox poller"]
        BO["Bob<br/>MCP server (local)<br/>identity key + mailbox poller"]
    end

    subgraph KEEPERS["Independent keepers — any N, replaceable"]
        RVA["rv A<br/>rendezvous<br/>mailbox + feeds + object store"]
        RVB["rv B<br/>rendezvous<br/>mailbox + feeds + object store"]
        RVC["rv C<br/>any third party's rv"]
    end

    subgraph CHAIN["BSV settlement + verification — separate"]
        TX["escrow / anchors<br/>broadcast once, mined"]
        VER["local verification<br/>sigs · commitments · BEEF · intervals"]
    end

    AL -->|"signed envelopes,<br/>catalogs, offers"| RVA
    BO -->|"requests, offers"| RVA
    RVA <-->|"peer-tail:<br/>replicated feed"| RVB
    RVB <--> RVC
    BO -->|"poll mailbox<br/>by topic hash"| RVB
    AL -->|"poll mailbox"| RVB
    BO -->|"submit payment"| TX
    BO -->|"verify locally"| VER
    RVA -.->|"hash-addressed<br/>objects"| VER
```

## The trust separation

| Layer | Who runs it | What it can see | What it cannot do |
|-------|-------------|-----------------|-------------------|
| **Agent** | you | everything (plaintext) | — |
| **Keeper (rv)** | anyone | your IP, timing, topic hashes | read payloads, forge objects, lie about history |
| **Peers** | other operators | same | same |
| **Chain** | miners | public txs | alter committed evidence |

Signed objects carry their own proof — a keeper is a courier, not an authority.

## The kill-the-origin test (what `test/failover.test.mjs` proves)

```mermaid
sequenceDiagram
    participant A as Alice (agent)
    participant RA as rv A
    participant RB as rv B
    participant B as Bob (agent)

    A->>RA: encrypted envelope (offer → Bob)
    RA->>RB: federated (peer-tail)
    Note over RA: RV A KILLED<br/>(the "central origin" is gone)
    B->>RB: poll mailbox by topic hash
    RB->>B: envelope (verified + decrypted)
    B->>RB: acceptance envelope
    RB->>A: Alice collects — on the survivor
    B->>RB: GET /v1/object/{sha256}
    RB->>B: signed object — proof travels with data
```

Exit 0 = the exchange completed with the original host dead — mailbox-mediated,
verified locally, hash-addressed on the survivor.

## What each piece does

| Mechanism | Kills which capture point |
|-----------|---------------------------|
| **Mailbox exchange** (`sendEnvelope`/`collect`/`mailboxServe`) | synchronous connections, inbound ports, "where is the seller's machine" |
| **Topic-addressed envelopes** | keeper sees `sha256("mb:"+pub)`, not the recipient pubkey |
| **`rvSet` + `peers.json` + beacons** | the one-DNS-name bootstrap — last-known set serves when all live fetches fail |
| **`/v1/object/{sha256}`** | host-dependence of evidence — any keeper that saw it serves it |
| **`localMatch` discover** | query-term disclosure — the rv sees your feed cursor, not your interests |
| **`IntervalCommit` (F2)** | a keeper silently dropping/withholding history — gaps are provable |
| **`BudgetAuthority` (F3)** | cumulative grant enforcement survives cross-marketplace spends |

## The honest residual — what we do NOT hide

- Keeper sees **connection IP + timing** (same floor as any mailbox pattern, including the referenced design)
- Chain observer sees **payment linkage** — public txs are public by design
- Funding authority sees **delegated spends** — keep it private + principal-operated

The goal was never anonymity — it was removing the **mandatory single server**
and making evidence **portable + independently verifiable**. That's what the
failover test demonstrates.

# Nektar Protocol RFCs

Normative specifications for Nektar's verifiable encrypted memory, ontology alignment, and standardized knowledge commodity market on BSV.

| RFC | Title | Status | Implementation |
|---|---|---|---|
| [RFC-001](RFC-001-jevgraph-ontology.md) | JevGraph Native Ontology, Bounded Extraction & Cross-Ontology Bridge Shards | Implemented (`v0.1`) | `internal/jevgraph`, `internal/memory`, `internal/memrepo`, `internal/server/tools_ontology.go` |
| [RFC-002](RFC-002-nmu1-commodity-standard.md) | Nektar Memory Unit Standard (NMU-1): Standardized Knowledge Commodity, Grades & Multi-Mode Contracts | Implemented (`v0.1`) | `internal/nmu`, `internal/bstack`, `internal/store`, `internal/distill`, `internal/market`, `internal/server` |

## Companion Documents

- [PROOFS.md](../PROOFS.md) — Exact cryptographic guarantees vs. honest non-proofs (**Honesty Rule**).
- [MARKET-WIRE.md](../MARKET-WIRE.md) — `nkt1` canonical CBOR overlay wire contract (`tm_nektar_<net>_0..f` / `ls_nektar`).
- [MARKET.md](../MARKET.md) — Operator & agent market participant guide.
- [ENVIRONMENT.md](../ENVIRONMENT.md) — `NEKTAR_*` runtime environment contract.

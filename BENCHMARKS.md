# nektar v0.4 benchmarks (measured)

Measured on the exact same machine as v0.3 (`Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz ×12, 16 GiB`, Windows amd64) via:

```sh
go test -v ./internal/memory -run=^$ -bench=. -benchmem -benchtime=1s
```

## Go v0.4 vs. TypeScript v0.3 (Same Hardware)

| Operation | v0.3 TypeScript (Node 24) | v0.4 Go (Go 1.27, zero CGO) | Speedup |
|---|---:|---:|---:|
| **Remember** (unanchored, signed + SQLite) | `3.470 ms/op` (288 op/s) | **`0.469 ms/op`** (2,130 op/s) | **7.4× faster** |
| **Anchor append** (funded `NektarAnchor` tx + full script verify) | `55.622 ms/op` (18 op/s) | **`10.188 ms/op`** (98 op/s) | **5.5× faster** |
| **SPV `VerifyFragment`** (16-leaf Merkle → anchor → BEEF/BUMP → headers) | `0.194 ms/op` (5,154 op/s) | **`0.041 ms/op`** (24,374 op/s) | **4.7× faster** |
| **Cedar authorize** (parse + evaluate) | `0.621 ms/op` (1,611 op/s, WASM) | **`0.025 ms/op`** (40,552 op/s, native `cedar-go`) | **25× faster** |
| **Cedar authorize** (compiled `PolicySet`) | `0.621 ms/op` (1,611 op/s, WASM) | **`0.00175 ms/op`** (573,066 op/s, native `cedar-go`) | **355× faster** |
| **BRC-369 Seal + Open** (`bcommon/keyed` epoch wrap + decrypt) | — | **`0.011 ms/op`** (90,827 op/s) | new native engine |
| **Datalog evaluate** (2 transitive rules, 601 EDB facts, ~7,800 derived) | `37.233 ms/op` (custom TS loop) | **`65.6 ms/op`** eval / `76.6 ms/op` parse+eval (`mangle-go`) | 0.57× (full Google Mangle engine w/ stratification & type checks; typical 10–50 fact grounding < 1 ms) |

## System, Build & Deployment Footprint

| Metric | v0.3 TypeScript | v0.4 Pure-Go |
|---|---|---|
| **Local full build** | multi-minute `pnpm -r build` | **`2.2s`** (`CGO_ENABLED=0`) |
| **Release artifact** | Node 24 + `node_modules` + native `sqlite3` per OS | **One `27.4 MB` static binary** |
| **Full daemon + Hub RAM** | ~500 MB–1.5 GB+ (Node + MariaDB + shims) | **`22.3 MB`** WorkingSet |
| **Cold daemon boot** | multi-second + container orchestration | **`1.07s`** |
| **GitHub Actions CI** | `7m 30s` | **`3m 08s`** (UI + Go race tests + Docker) |
| **GitHub Actions Release** | `5m 43s`–`6m 32s` | **`3m 26s`** (4-OS binaries in `19s`–`63s` + GHCR image) |

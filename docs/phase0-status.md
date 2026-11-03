# Phase 0 status (partial — funding/broadcast deferred per user direction)

## Stack running (left up)

- **Teranode regtest** via `bsv-blockchain/teranode-quickstart` (cloned at `nektar/infra/teranode-quickstart`), Teranode `v0.16.0` image.
  - `network=regtest`, `COMPOSE_PROFILES=` (no p2p/legacy/monitoring/blockpersister), `listen_mode=listen_only`.
  - Services: blockchain, asset (`:8090`), propagation (`:8084`, `POST /tx` EF), rpc (`:9292`, basic auth in `.env`), subtreevalidation, blockvalidation, blockassembly, pruner, postgres, aerospike, redpanda.
  - Start: `cd infra/teranode-quickstart && ./start.sh` (then `docker exec blockchain teranode-cli setfsmstate --fsmstate CATCHINGBLOCKS` once; mining works after `generate`/`generatetoaddress` via `./rpc.sh`). Stop: `./stop.sh`.
  - Mining: `generate`/`generatetoaddress` work; FSM must be RUNNING (`setfsmstate` after first start).
  - Quirks fixed: CRLF line endings in mounted `config/entrypoint.sh`/`wait.sh` (converted to LF); `getblockcount` unimplemented (use `getblockchaininfo`); `getblock` returns no txids (coinbase recovered via `GET :8090/api/v1/block/<hash>` raw block; note `bf00` EF marker after the tx-count varint and trailing metadata).

## Worked

- Regtest chain mines on demand (~600+ blocks produced).
- Coinbase pays arbitrary regtest address (P2PKH to our keyhash confirmed on-chain).

## Blocked / deferred

- **Arcade & Chaintracks on regtest: not feasible.** `bsv-broadcaster` (arcade) force-disables embedded chaintracks on regtest (no regtest genesis header) and there is no regtest Merkle Service, so txs would stay at RECEIVED — no MINED callbacks/BUMPs. Plan: broadcast via Teranode propagation `POST /tx` (EF) + Teranode RPC/asset API for headers; deviation to record.
- Funded-wallet broadcast + chain-depth spike: deferred (needs sign/broadcast; `spikes/fund.mjs` exists and found a mature 625M-sat coinbase UTXO in `.state.json`).

## Pinned

- node v24.15.0, pnpm 12.16.1, `@bsv/sdk@4.0.1`, `@bsv/overlay@2.7.0`, `@bsv/overlay-express@2.8.0`.

## Spike A — PASSED (offline)

`spikes/overlay/` + productionised `packages/overlay`: real `Engine` + `KnexStorage` (sqlite) + mock `ChainTracker`, synthetic mined parents with real BUMPs. All cases pass:

- genesis admitted; valid successor admitted and becomes head;
- wrong `n`, wrong `R'_n`, non-head-spend, second-genesis fork: all rejected;
- `ls_nektar` returns current head and anchor-by-n (retained history).

Engine quirks found: `outputSpent` is notified even for rejected submissions (ls buffers spends until the spender is admitted); a rejected submission marks the spent topical output stale and removes it from storage (lookup hydration then returns empty — keep `ls` as the authority). `previousCoins` detection: mined parents arrive BUMP-only in the BEEF (no outputs) — tm validates against the stored head record, not the parent tx.

## Spike B / live regtest — deferred

Funded-wallet broadcast + unconfirmed-chain-depth measurement deferred per user direction. Anchor building + signing works (cached child key); 1,000 unconfirmed anchors chain in ~42 s on the mock chain. Remaining for live regtest: fund.mjs → sendrawtransaction/propagation `/tx` broadcast, depth limit measurement, header queries.

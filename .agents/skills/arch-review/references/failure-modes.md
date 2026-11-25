# failure-modes — runtime-structure lens (nektar v0.4 Pure-Go)

Load for Phase 2's runtime hunt. The static `go list` graph shows where failure
can travel; this catalog hunts where the running Go daemon breaks under crash,
restart, race, lost wakeup, partial failure, saturation, or drift.

## Runtime topology map (build before hunting)

Map these six surfaces from source before hunting:
1. **Processes & locks**: `nektar serve`, `nektar mcp`, `nektar hub`, `nektar wallet`,
   and the managed `restate-server` sidecar (`cmd/nektar/restate_runner.go`). Verify
   kernel advisory locking (`cmd/nektar/lock*.go`), orphan PID cleanup, and graceful
   shutdown order (`app.Close` → `hub.CloseSSE` → `httpSrv.Shutdown` → `broker.Stop` → `restate.Close` → `db.Close`).
2. **Goroutines & tickers**: every `go func` and `time.NewTicker` (anchor proof
   refresh, wallet `Monitor`, broker SSE/bbox/poll loops, hub federation &
   maintenance, dashboard SSE). Verify every loop exits on `ctx.Done()` / `stopCh`
   and is joined via `sync.WaitGroup`.
3. **Event subscriptions**: Hub SSE (`/hub/events`), Dashboard SSE (`/events`),
   Restate Awakeables & streaming `Attach`, Bbox store-and-forward inbox. Verify
   backlog cursor pagination and wake-on-error paths.
4. **Durable vs in-memory state**: SQLite (`nektar.db`, `wallet.sqlite`, `hub.db`),
   JSON files (`spend-guard.json`, `identity.json`, `daemon.json`, `*-token`),
   Restate journal vs in-memory maps (`publicChirp`, `bboxExpected`, `oauthClients`,
   `txCache`, `rateBuckets`). Ask: `kill -9` right now — what does boot believe?
5. **External dependencies**: Arcade/WoC broadcasters & header trackers, BSV Desktop
   `wirewallet` (`127.0.0.1:3321`), Restate server (`8080`/`9070`), peer hubs,
   OpenAI/Anthropic upstreams.
6. **The satoshi path end-to-end**: UTXO selection → `SpendGuard.Reserve` →
   transaction build (`NoSend` / `BuildBRC29Tx`) → durable pre-broadcast persistence
   (`pending_anchors`, `deals`, `pending_brc29`) → `Broadcast` / `PostFromBEEF` →
   status reconciliation (`UnFail`, `ReconcileOrphanedNoSend`, `ReconcilePendingAnchors`) →
   SPV proof verification (`VerifyFragmentCtx` + header root check).

## Evidence standard (every runtime finding)

| Field | Question |
| --- | --- |
| Code path | `file:line` read directly in this session |
| Trigger | exact runtime event (crash, 429, duplicate request, restart, network blip) |
| Failure path | what the code actually executes next |
| Blast radius | how far state/money corruption or stall travels |
| Silence reason | why operators/metrics/callers do not see it |

Then run the **challenge step**: hunt for the preventing lock, SQLite tx, CAS,
Restate step idempotency, SpendGuard guard, or test. Assign confidence:
- **High**: whole path traced; nothing prevents it.
- **Medium**: mechanism confirmed, trigger plausible, guard unclear.
- **Low**: hypothesis — list under Hypotheses with the settling test.

## F-Catalog (`F1–F12`)

- **F1. Concurrency & check-then-act**: TOCTOU races across files (`O_CREATE|O_EXCL`),
  SQLite read-then-write outside a single tx, or unsynchronized struct fields across goroutines.
- **F2. Ordering & lifecycle**: boot races (Restate registering before HTTP/h2c ready,
  client attached before/after handler), unawaited `httpSrv.Shutdown` racing deferred `db.Close()`,
  SSE handlers pinning `Shutdown` until timeout, orphaned child processes.
- **F3. Crash & restart recovery**: broadcast before durable persistence (double-spend/double-pay
  on Restate retry or crash), `submission_unknown` / `nosend` UTXOs stranded or mis-aborted,
  unmined `pending_anchors` promoted prematurely or lost on reboot.
- **F4. Lost wakeup & event gaps**: SSE backlog truncation (filtered vs raw cursor),
  `Attach` timeout/polling gaps, awakeable resolution racing approval CAS, pagination
  truncating `/sync` beyond page 1.
- **F5. Partial failure & silence**: swallowed errors (`_, _ =`) on FTS shred,
  anchor tx save, or bbox reply; mid-batch broadcast aborting already-sent txids;
  transient 429/5xx smeared into `NotFound`; Cedar `diag.Errors` ignored; corrupt
  SPV proof silently treated as amber.
- **F6. Saturation & unbounded growth**: `List*(10000)` on `/v1/metrics`, unbounded
  in-memory maps (`txCache`, `rateBuckets`, `oauthClients`), nonce table DoS before
  rate limiting, federation hitting human rate limits.
- **F7. External dependency failure**: trusting unverified Arcade/WoC/seller proofs,
  unbounded `http.Client` timeouts on streaming vs unary RPCs, client-mode wallet
  degraded fallbacks (`AdoptReceived` silent no-op).
- **F8. State machine drift**: monotonic deal states overwritten (`settled` → `failed`),
  approval replay windows (`approved` never transitioned to `consumed`), wallet
  `outputs`/`user_utxos`/`transactions` split-brain on `UnFail` or `MarkSubmissionUnknown`.
- **F9. Time & economics assumptions**: UTC midnight boundary bugs (`ReleaseSpendReservation`
  using `time.Now()` instead of reservation `created_at`), static P2PKH fallback on
  live networks, unverified payment amount before relay/broadcast.
- **F10. Idempotency & replay**: Restate step retries re-sending satoshis without
  checking persisted txid/BEEF, `PostFromBEEF` double-debiting `SpendGuard`,
  re-claiming BRC-105 payment proofs.
- **F11. State lists that drift apart**: hardcoded SQL `status IN (...)` lists across
  `wallet/engine.go` & `monitor.go` disagreeing on `'nosend'` / `'submission_unknown'` /
  `'unproven'` / `'completed'`.
- **F12. Restart-durability inventory**: every in-memory map (`publicChirp`,
  `bboxExpected`, `bboxWantByOffer`, `txCache`, `pairCodes`) audited for `kill -9`
  recovery and restart rebuild.

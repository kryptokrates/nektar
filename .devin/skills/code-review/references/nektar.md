# nektar rule catalog

Load this file before reviewing any diff that touches `packages/*/src`,
`deploy/`, `infra/`, `tools/`, or `.github/`. These rules outrank the common
catalog — they encode incidents this codebase has actually had. Rule IDs are
stable: cite them verbatim.

## Custody — "the app never holds keys"

| Rule ID | Severity | Rule |
|---|---|---|
| `nektar/custody-raw-key` | BLOCKER | New code reads, derives, generates, or stores a `PrivateKey`/WIF/seed in production paths. Signing goes through the `DigestSigner` seam (`keySigner`/`walletSigner`/`custodySigner` in `packages/core/src/signer.ts`). Test-only keys and `keys.json` legacy bootstrap reads are allowed. |
| `nektar/custody-http-key` | BLOCKER | A WIF, private key, or seed crosses an HTTP/JSON boundary (e.g. `newOwnerWif` params). Derived `keyID` rotation is the pattern — `createSignature` under a new keyID assigns a new owner without key material crossing the API. |
| `nektar/custody-sign-bypass` | MAJOR | A signing site constructs `ProtoWallet(key)`/`P2PKH().unlock(key)`/`LocalSigner` directly instead of routing through `DigestSigner`/`runarSigner`/`unlockP2PKH`. Wallet custody breaks silently otherwise. |
| `nektar/custody-identity-sign` | MAJOR | Expecting `createSignature({identityKey:true})` to produce a signature under the identity pub — sdk-4 cannot root-sign; it always derives a child. Only `getPublicKey` honors `identityKey`. |

## Money paths — escrow, payment, spending bounds

| Rule ID | Severity | Rule |
|---|---|---|
| `nektar/escrow-semantics` | BLOCKER | Changes to covenant params, `escrowWire`, accept/delivered envelope fields, `refundAfter` handling, or direct-rail settlement shape. Stop and flag for explicit review — these change what money does. (Direct rail is opt-in both sides, `directMaxSats`-bounded, final/no-refund — keep it that way.) |
| `nektar/spendguard-bypass` | BLOCKER | A broadcast or funding spend that does not route through SpendGuard/`BudgetAuthority`, or a spend path that constructs a tx the guard never sees. Every sat leaving the reserve must be policy-gated. |
| `nektar/envelope-bound` | BLOCKER | Data embedded in a market envelope unbounded — `MaxContent` is 16 KiB after encoding/encryption; plaintext ~12 KiB can already overflow. Optional `tx` wires must be droppable with a `fetchTx(txid)` receiver fallback. |
| `nektar/refund-height` | MAJOR | Refund attempts that don't check `refundAfter`/height first. nLockTime-immature rejection is a WAIT (park, reset tries) not a refusal — burning the refusal threshold on expected height lag was a real incident. |
| `nektar/change-leak` | MAJOR | A funding spend that marks inputs spent without recording the change output back to the reserve. `onSpent` must `addOwnedOut` the change vout (see the finger-publish incident — the whole UTXO remainder orphaned). |
| `nektar/double-pay` | BLOCKER | Any path that can re-broadcast/re-pay after restart without consulting the persisted journal/outbox/operation state. Restart recovery must be idempotent. |

## Automation & market state machine

| Rule ID | Severity | Rule |
|---|---|---|
| `nektar/state-skip` | MAJOR | A transition that skips a state, mutates `state` without a `market_events` row, or auto-advances without an explicit config gate (`autoUseful`, `directMaxSats` patterns). Terminal-useful transitions are policy-gated, never unconditional. |
| `nektar/outbox-regress` | MAJOR | Outbox retry logic that consumes attempts for transient/deferrable causes (`funding slot` waits defer without burning attempts) or replays a persisted payload it could shrink/rebuild. |
| `nektar/inbox-trust` | BLOCKER | `onDelivered`/`onAccept`-style inbox handlers that trust payload fields without re-verifying (sig, commitment, on-chain spend via `fetchTx`). Envelopes are untrusted input. |
| `nektar/fixed-sleep` | MAJOR | New `sleep`/`setTimeout`-style fixed waits or polling loops in production paths where an event/wake exists. Waits must be event-driven with a bounded timeout ceiling. |

## Wire format & SDK discipline

| Rule ID | Severity | Rule |
|---|---|---|
| `nektar/sdk-version` | BLOCKER | Importing `@bsv/sdk` APIs that differ across the pinned version, or loosening the syncpack pin. The image-skew incident (2.7.1 vs 4.0.1 BEEF parse) is the poster: producers and parsers must run the same sdk — the bstack bundle embeds it, never externalize `@bsv/sdk` in bundle builds. |
| `nektar/sighash-hash` | BLOCKER | Passing a sighash through a path that sha256s it again (`data:` on `createSignature`, or assuming `PrivateKey.sign` hashes — sdk-4 signs raw). Wallet sighash = `hashToDirectlySign`. |
| `nektar/wire-schema` | MAJOR | Changing a signed canonical form (`wantCanonical`, offer/direct fields, artifact shape) without versioning — older hosts/verifiers compute a different digest. Additive fields only, and update `MARKET-WIRE.md`. |
| `nektar/artifact-unverified` | BLOCKER | Accepting a carrier/artifact whose `verifyBody` equivalent isn't run — signature AND shape, both. `reason=sig` vs `reason=exp` distinctions matter. |

## Honesty & evidence (project culture — also in AGENTS.md)

| Rule ID | Severity | Rule |
|---|---|---|
| `nektar/fake-evidence` | BLOCKER | Fabricated receipt tokens, verification URLs, PR-merge claims, or "passed" results without executed evidence. Ungrounded is an acceptable answer; invented proof is not. |
| `nektar/proof-overclaim` | MAJOR | Comments/docs claiming proofs establish content truth or zkML-proven manifests — proofs establish recorded+unaltered+derivable only. |
| `nektar/secret-commit` | BLOCKER | Key material, WIFs, `.state/` artifacts, tokens, or `.env` secrets entering tracked files. gitleaks must stay green. |

## Repo mechanics

| Rule ID | Severity | Rule |
|---|---|---|
| `nektar/generated-churn` | MINOR | Contract `*.json` diffs that are timestamp-only committed, or a `script` change without `pnpm --filter @nektar/contracts compile:contract` evidence. `packages/bstack/bundle/` must be rebuilt when `src/` changes and hosts restarted before testing. |
| `nektar/leftovers` | MINOR | New `as any`, `@ts-ignore`, `TODO`, stray `console.*`, or commented-out code — the ratchet at `tools/.leftover-baseline.json` only shrinks. |
| `nektar/test-weaken` | BLOCKER | Assertions loosened, tests deleted/skipped, or fixtures broadened to make a failing suite green. Fix the code, not the test. |
| `nektar/env-gate` | MINOR | New behavior-changing env var without a default that preserves current behavior and a doc line where the other `NEKTAR_*` vars live. |

## Review heuristics for this codebase

- **Follow the money first**: any diff touching `takeFunding`, `take(`,
  `reserve`, `broadcast`, `unlockP2PKH`, `runarSigner`, covenant builders in
  `anchor/` gets the money-path catalog applied line-by-line before anything
  else.
- **Check both sides of a wire**: sender emits, receiver must parse — if a
  field is added, verify the consumer (possibly an older deployed version)
  still validates.
- **Restart thinking**: for every new write path ask "if the process dies
  after broadcast but before journal commit, what replays?" Idempotent or
  journaled — no third option.
- **Persisted-state compat**: SQLite rows written by an older version are
  inputs too. A new reader must handle the old shape (the oversized-outbox
  incident: persisted payloads outlive the code that made them).

// port.ts — the one chain-side contract the runtime assembles against.
// A ChainPort carries every per-network answer in one place: the spend path,
// the witnessed header source, proof + raw-transaction lookups, the outside
// status-hint stream, and height. Three adapters:
//
//   memoryPort   — MemoryChain (test tier): no outside voice, instant mining
//   regtestPort  — the local Teranode stack (RPC headers, asset proofs, mine)
//   livePort     — main/testnet: BsvBroadcaster + witnessed Chaintracks/WoC
//                  headers, SSE status-hint stream, SpendGuard in front
//
// openRuntime consumes only this shape — there is no second assembly path.
import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { randomBytes } from 'node:crypto'
import { type Transaction, type MerklePath } from '@bsv/sdk'
import { nektarEnv, nektarEnvFlag, setNektarEnv } from '@nektar/core'
import type { BroadcastResult, ChainTracker, TxStatusEvent } from './interfaces.js'
import type { StatusHint } from './txfacts.js'
import { SpendGuard, type SpendGuardOpts } from './spendguard.js'
import { TeranodeBroadcaster, TeranodeRPCTracker, regtestTx, regtestMerkleProof, regtestRpc, utxoStatus, type RegtestConfig } from './regtest.js'
import { WitnessedTracker } from './headers.js'
import {
  BsvBroadcaster,
  IndexTracker,
  ChaintracksTracker,
  merklePathMainnet,
  fetchRawTx,
  fetchBroadcasterFeeRate,
  statusStream,
  utxoSpendStatus,
  type MainnetConfig
} from './mainnet.js'
import { TemporalSources, type TemporalSource } from './temporal.js'
import type { TestChain } from './testtier.js'

/** every chain question the runtime asks, one object */
export interface ChainPort {
  /** network label — drives prefixes and custody defaults */
  network: 'regtest' | 'testnet' | 'main'
  /** the spend path — SpendGuard-wrapped wherever a guard is configured */
  broadcast: (tx: Transaction) => Promise<BroadcastResult>
  /** the chain-side tx view: backend answers with the sealed proof attached
   *  when mined. Absent on live ports (txLookup is the throttled fallback) */
  getTx?: (txid: string) => Transaction | undefined | Promise<Transaction | undefined>
  /** first-party definitive refusals the backend itself remembers */
  isRejected?: (txid: string) => boolean
  /** the witnessed header source — currentHeight/isValidRootForHeight/
   *  headerAtHeight/onReorg */
  tracker: ChainTracker
  /** a proof we can go pull — verified against the port's own headers before
   *  callers see it (the missed-push recovery path) */
  proofLookup?: (txid: string) => Promise<MerklePath | undefined>
  /** raw transaction bytes pull — adapter hash-checks before answering */
  txLookup?: (txid: string) => Promise<Transaction | undefined>
  /** the outside status word for txFacts.hint — live ports only */
  statusHint?: (txid: string) => Promise<StatusHint>
  /** the ChainEvents observe — how this port produces terminal evidence:
   *  a verified event, `undefined` (definitively not terminal — feeds the
   *  abandon clock), or a throw (couldn't ask — the clock must not move) */
  observe: (txid: string) => Promise<TxStatusEvent | undefined>
  /** outpoint-level spend truth — 'spent' is a burn verdict the port
   *  verified itself (own utxo set, or an index claim proven against our
   *  witnessed headers). 'unspent' is only answerable where the utxo view
   *  is first-party (regtest node, memory chain) — live indexes can never
   *  prove absence, so they answer 'spent' or 'unknown'. Callers burn on
   *  'spent' only; 'unknown' must never burn or block. */
  spendStatus?: (txid: string, vout: number) => Promise<'spent' | 'unspent' | 'unknown'>
  /** first-party utxo view — this port's spendStatus can answer 'unspent'
   *  honestly. Live indexes can never prove absence and leave it unset;
   *  consumers gate 'unspent'-oracles (Funds.liveCheck) on the flag instead
   *  of inferring endpoint behavior — a probe that can never answer true
   *  only burns retries inside the serialized take queue */
  provesUnspent?: boolean
  /** chain height — ports that mine fold mined heights in themselves */
  height: () => number | Promise<number>
  /** open the pushed-hint channel (SSE status stream) — returns its closer,
   *  or undefined where no stream applies (callback URL set, no token). The
   *  sink receives the broadcaster's raw wire JSON; the caller routes it
   *  through the status handler and the events substrate's note(). */
  openHintStream?: (sink: (j: unknown) => Promise<void>) => { close(): void } | undefined
  /** blocks a watched unproven tx may age before the abandon verdict fires */
  abandonAfterBlocks?: number
  /** the guard when the port wraps one — the /v1/wallet + metrics surface */
  guard?: SpendGuard
  /** the broadcaster-policy fee rate in sat/kB (live ports) */
  feeRateSatPerKb?: number
  /** regtest: mine a block to the funding address (autoMine hook) */
  mine?: () => Promise<void>
}

/** deps every port factory shares — the custody-facing seam is lazy because
 *  the wallet view is built AFTER the port (services need port.broadcast) */
export interface PortDeps {
  /** input-value resolver for the guard's fee check — reads the custody
   *  view lazily (assigned by the caller once custody exists) */
  valueOf?: (txid: string, vout: number) => number | undefined
  guard?: SpendGuardOpts
}

/** TemporalSources over the primary tracker + optional BTC WoC tracker
 *  (read-only). `NEKTAR_BTC_WOC_URL` overrides the BTC endpoint. */
export function temporalSourcesFor(names: string[], primary: ChainTracker, opts: { indexerApiKey?: string; fetchImpl?: typeof fetch } = {}): TemporalSources {
  const sources: TemporalSource[] = [{ name: 'bsv', tracker: primary }]
  for (const n of names) {
    if (n === 'bsv') continue
    if (n === 'btc')
      sources.push({
        name: 'btc',
        tracker: new IndexTracker({
          // bananablocks is BSV-only — BTC temporal headers still come from WoC
          indexerUrl: nektarEnv('NEKTAR_BTC_WOC_URL') ?? 'https://api.whatsonchain.com/v1/btc/main',
          indexerApiKey: opts.indexerApiKey,
          fetchImpl: opts.fetchImpl
        })
      })
  }
  return new TemporalSources(sources)
}

// --- memory adapter -----------------------------------------------------------

/** the test-tier port: a memory chain is already the whole answer — no
 *  outside voice exists, so statusHint/hintStream stay absent and the proof
 *  a seal writes is the only evidence path. */
export function memoryPort(chain: TestChain): ChainPort {
  return {
    network: 'regtest',
    broadcast: (tx) => chain.broadcast(tx),
    getTx: (txid) => chain.fetchTx(txid),
    isRejected: (txid) => chain.isRejected(txid),
    tracker: chain,
    proofLookup: (txid) => chain.merklePathOf(txid),
    txLookup: (txid) => chain.fetchTx(txid),
    observe: async (txid) => {
      const tx = await chain.fetchTx(txid)
      if (tx?.merklePath !== undefined) return { txid, status: 'mined', blockHeight: tx.merklePath.blockHeight, merklePath: tx.merklePath }
      if (chain.isRejected(txid)) return { txid, status: 'rejected' }
      return undefined
    },
    spendStatus: (txid, vout) => chain.utxoStatus(txid, vout),
    provesUnspent: true,
    height: () => chain.currentHeight(),
    abandonAfterBlocks: 100,
    mine: async () => {
      await chain.mine(1)
    }
  }
}

// --- regtest adapter ----------------------------------------------------------

export function regtestPort(rc: RegtestConfig & { fundAddress?: string }, deps?: PortDeps): ChainPort {
  const tracker = new TeranodeRPCTracker(rc)
  const guard = new SpendGuard(new TeranodeBroadcaster(rc.propagationUrl), deps?.valueOf ?? (() => undefined), deps?.guard ?? {})
  const rpc = regtestRpc(rc)
  let lastMined = 0
  return {
    network: 'regtest',
    broadcast: (tx) => guard.broadcast(tx),
    getTx: async (txid) => await regtestTx(rc, txid),
    tracker,
    observe: async (txid) => {
      const mp = await regtestMerkleProof(rc, txid, undefined, { strict: true })
      if (mp === undefined) return undefined
      let root: string
      try {
        root = mp.computeRoot(txid)
      } catch {
        return undefined // served path doesn't contain the txid — a claim, not a proof
      }
      if (!(await tracker.isValidRootForHeight(root, mp.blockHeight))) return undefined
      return { txid, status: 'mined', blockHeight: mp.blockHeight, merklePath: mp }
    },
    spendStatus: (txid, vout) => utxoStatus(rc, txid, vout),
    provesUnspent: true,
    height: async () => Math.max(lastMined, await tracker.currentHeight()),
    abandonAfterBlocks: 100,
    guard,
    mine:
      rc.fundAddress === undefined
        ? undefined
        : async () => {
            const [hash] = (await rpc('generatetoaddress', 1, rc.fundAddress)) as string[]
            const hdr = (await rpc('getblockheader', hash, true)) as { height: number }
            lastMined = Math.max(lastMined, hdr.height)
          }
  }
}

// --- live adapter (main + testnet) ---------------------------------------------

export interface LivePortDeps extends PortDeps {
  /** locally-held tx incl. any verified proof (the runtime's fetchTx) — the
   *  observe path checks it before asking the outside */
  held?: (txid: string) => Promise<Transaction | undefined>
  /** record an unverified pushed hint on the events substrate */
  note?: (ev: TxStatusEvent) => void
  /** data dir for the persisted callback token + SSE cursor */
  dataDir: string
  callbackUrl?: string
  log?: (s: string) => void
}

/** Shared live path for `main` (BSV mainnet) and `testnet` — test coins,
 *  same code. Net-dependent bits: address prefix + fee floor live in the
 *  custody/config layer, not here. */
export async function livePort(mc: MainnetConfig & { feeRateSatPerKb?: number }, net: 'main' | 'testnet', deps: LivePortDeps): Promise<ChainPort> {
  const testNet = net !== 'main'
  const log = deps.log ?? (() => undefined)
  // headers via Chaintracks (public, no key) WITNESSED by WoC: each source
  // self-authenticates its answers via header PoW (NEKTAR_HEADER_POW=0 opts a
  // private non-PoW net out), and the two must agree before a root is
  // served. NEKTAR_STRICT_HEADERS=1 fails closed when the witness can't
  // answer; default degrades to the reachable source marked uncorroborated.
  const requirePow = nektarEnvFlag('NEKTAR_HEADER_POW') !== false
  const tracker = new WitnessedTracker(
    new ChaintracksTracker(
      mc,
      mc.chaintracksUrl ?? (testNet ? `${mc.broadcasterUrl}/chaintracks/v2` : 'https://arcade-us-1.bsvblockchain.tech/chaintracks/v2'),
      undefined,
      { requirePow }
    ),
    new IndexTracker(mc, { requirePow }),
    {
      strict: nektarEnvFlag('NEKTAR_STRICT_HEADERS'),
      onDisagree: (height, primaryHash, witnessHash) =>
        log(`nektar ${net}: header sources disagree at ${height} (primary ${primaryHash}, witness ${witnessHash}) — serving no root\n`)
    }
  )
  // per-instance callback token — tags our submissions so the broadcaster can
  // route their status updates back to us (NEKTAR_CALLBACK_TOKEN wins; else a
  // persisted random token, also pushed into env so webhook auth sees it)
  if (!mc.callbackToken && !nektarEnv('NEKTAR_CALLBACK_TOKEN')) {
    const tokFile = join(deps.dataDir, 'callback-token')
    mc.callbackToken = existsSync(tokFile)
      ? readFileSync(tokFile, 'utf8').trim()
      : (() => {
          const t = randomBytes(32).toString('hex')
          writeFileSync(tokFile, t, { mode: 0o600 })
          return t
        })()
    setNektarEnv('NEKTAR_CALLBACK_TOKEN', mc.callbackToken)
  }
  const callbackUrl = deps.callbackUrl ?? mc.callbackUrl ?? nektarEnv('NEKTAR_CALLBACK_URL')
  const raw = new BsvBroadcaster(mc)
  const guard = new SpendGuard(raw, deps.valueOf ?? (() => undefined), deps.guard ?? {})
  const statusHint = (txid: string): Promise<StatusHint> => raw.statusHint(txid)
  const floor = testNet ? 1 : 100
  const feeRateSatPerKb = Math.max(mc.feeRateSatPerKb ?? floor, await fetchBroadcasterFeeRate(mc).catch(() => floor))
  return {
    network: net,
    broadcast: (tx) => guard.broadcast(tx),
    tracker,
    proofLookup: async (txid) => {
      const r = await merklePathMainnet(mc, txid)
      if (!r) return undefined
      const root = r.merklePath.computeRoot(txid)
      return (await tracker.isValidRootForHeight(root, r.merklePath.blockHeight)) ? r.merklePath : undefined
    },
    txLookup: async (txid) => await fetchRawTx(mc, txid),
    statusHint,
    observe: async (txid) => {
      const tx = await deps.held?.(txid).catch(() => undefined)
      if (tx?.merklePath !== undefined) return { txid, status: 'mined', blockHeight: tx.merklePath.blockHeight, merklePath: tx.merklePath }
      // a broadcaster rejection verdict is a claim with no possible proof
      // object — only a double-spend is provable (via the conflicting tx's
      // own BUMP). It lands as a hint (resolves waiters, keeps the floor),
      // never on the verified path — same treatment the push path gives it.
      const probe = await statusHint(txid)
      if (['REJECTED', 'DOUBLE_SPEND_ATTEMPTED'].includes(probe.status)) deps.note?.({ txid, status: 'rejected' })
      // an unanswered probe — or a 'mined' claim we hold no verified path
      // for — is not "not mined": throwing keeps the abandon clock from
      // counting a question nobody answered
      if (!probe.answered || probe.status === 'MINED' || probe.status === 'IMMUTABLE') throw new Error(`cannot establish 'not mined' for ${txid}`)
      return undefined
    },
    spendStatus: (txid, vout) => utxoSpendStatus(mc, tracker, txid, vout),
    height: () => tracker.currentHeight(),
    abandonAfterBlocks: Number(nektarEnv('NEKTAR_ABANDON_BLOCKS') ?? 6),
    openHintStream:
      callbackUrl === undefined && (mc.callbackToken ?? nektarEnv('NEKTAR_CALLBACK_TOKEN')) !== undefined
        ? (sink) => {
            const cursorFile = join(deps.dataDir, 'sse-cursor')
            const stream = statusStream(
              mc,
              async (ev) => {
                if (ev.data === undefined) return
                let j: unknown
                try {
                  j = JSON.parse(ev.data)
                } catch {
                  return // malformed data line — skip
                }
                await sink(j)
              },
              {
                cursor: {
                  get: () => {
                    try {
                      const v = readFileSync(cursorFile, 'utf8').trim()
                      return v === '' ? undefined : v
                    } catch {
                      return undefined
                    }
                  },
                  set: (id) => {
                    try {
                      writeFileSync(cursorFile, id)
                    } catch {
                      /* best effort */
                    }
                  }
                },
                onError: (e) => log(`nektar ${net}: status stream: ${e.message}\n`)
              }
            )
            log(`nektar ${net}: no callback URL — pulling tx statuses from the broadcaster /events stream\n`)
            return stream
          }
        : undefined,
    guard,
    feeRateSatPerKb
  }
}

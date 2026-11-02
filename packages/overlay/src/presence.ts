// tm_presence / ls_presence — the agent-directory topic.
// Agents "find each other" by publishing a signed OP_RETURN ad:
//   OP_RETURN 'mamem-agent' <pub 33B> <json{endpoint,shards,ts}> <sig DER>
// The signature binds endpoint+shards+ts to the advertiser's key — the same
// key that owns the anchor chain, so 'who serves shard X' is answered by the
// key that anchors X, not by whoever shouts loudest. Ads are 0-sat data
// outputs; latest-by-ts wins per pubkey.
import { PrivateKey, PublicKey, Signature, Script, Transaction } from '@bsv/sdk'
import { sha256, utf8, toHex } from '@mamem/core'
import type { Knex } from 'knex'
import type { TopicManager, AdmittanceInstructions, LookupService, LookupFormula, OutputAdmittedByTopic, OutputSpent } from '@bsv/overlay'

const TAG = 'mamem-agent'
const TABLE = 'mamem_ls_presence'

export interface PresenceAd {
  pub: string // 33B compressed pubkey hex — the agent's identity key
  endpoint: string // e.g. http://host:8080/mcp
  shards: string[] // shards it serves
  ts: number // ms epoch — latest wins
  exp?: number // ms epoch — ad expires; default none (explicit refresh recommended)
}
export interface SignedAd { ad: PresenceAd, sig: string } // sig = DER hex over the canonical digest

const canonical = (ad: PresenceAd) => `${ad.endpoint}|${[...ad.shards].sort().join(',')}|${ad.ts}|${ad.exp ?? ''}`
const digest = (ad: PresenceAd) => sha256(utf8(canonical(ad)))

export function signPresence (key: PrivateKey, fields: Omit<PresenceAd, 'pub'>): SignedAd {
  const ad: PresenceAd = { ...fields, shards: [...fields.shards].sort(), pub: key.toPublicKey().toString() }
  return { ad, sig: toHex(Uint8Array.from(key.sign(Array.from(digest(ad))).toDER() as number[])) }
}

export function verifyPresence (ad: PresenceAd, sigHex: string): boolean {
  try {
    if (typeof ad.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(ad.pub)) return false
    if (typeof ad.endpoint !== 'string' || !/^https?:\/\/\S{2,200}$/.test(ad.endpoint)) return false
    if (!Array.isArray(ad.shards) || ad.shards.length > 64 || !ad.shards.every(s => typeof s === 'string' && s.length > 0 && s.length <= 64)) return false
    if (!Number.isSafeInteger(ad.ts) || ad.ts <= 0) return false
    if (ad.exp !== undefined && (!Number.isSafeInteger(ad.exp) || ad.exp <= Date.now())) return false // expired
    return PublicKey.fromString(ad.pub).verify(Array.from(digest(ad)), Signature.fromDER(sigHex, 'hex'))
  } catch { return false }
}

export function presenceOutputScript ({ ad, sig }: SignedAd): string {
  const payload = JSON.stringify({ endpoint: ad.endpoint, shards: ad.shards, ts: ad.ts, ...(ad.exp !== undefined ? { exp: ad.exp } : {}) })
  return Script.fromASM(`OP_RETURN ${toHex(utf8(TAG))} ${ad.pub} ${toHex(utf8(payload))} ${sig}`).toHex()
}

// The SDK's Script.chunks collapses OP_RETURN data into one chunk — decode the
// pushes ourselves. Everything after the leading 0x6a must be pushdata or it
// isn't our format.
function readPushes (hex: string): Buffer[] | null {
  const b = Buffer.from(hex, 'hex')
  if (b[0] !== 0x6a) return null
  const out: Buffer[] = []
  let i = 1
  while (i < b.length) {
    const op = b[i++]!
    let len: number
    if (op === 0) { out.push(Buffer.alloc(0)); continue }
    if (op <= 75) len = op
    else if (op === 0x4c) { len = b[i] ?? -1; i += 1 }
    else if (op === 0x4d) { len = i + 2 <= b.length ? b.readUInt16LE(i) : -1; i += 2 }
    else if (op === 0x4e) { len = i + 4 <= b.length ? b.readUInt32LE(i) : -1; i += 4 }
    else return null
    if (len < 0 || i + len > b.length) return null
    out.push(Buffer.from(b.subarray(i, i + len))); i += len
  }
  return out
}

export function parsePresence (scriptHex: string): SignedAd | null {
  try {
    const pushes = readPushes(scriptHex)
    if (!pushes || pushes.length !== 4) return null
    if (pushes[0]!.toString() !== TAG) return null
    const pub = pushes[1]!.toString('hex')
    const p = JSON.parse(pushes[2]!.toString())
    const ad: PresenceAd = { pub, endpoint: p.endpoint, shards: p.shards, ts: p.ts, ...(p.exp !== undefined ? { exp: p.exp } : {}) }
    return { ad, sig: pushes[3]!.toString('hex') }
  } catch { return null }
}

export function createTmPresence (): TopicManager {
  return {
    identifyAdmissibleOutputs: async (beef: number[], _previousCoins: number[]): Promise<AdmittanceInstructions> => {
      const tx = Transaction.fromBEEF(beef)
      const admit: AdmittanceInstructions = { outputsToAdmit: [], coinsToRetain: [] }
      for (const [i, o] of tx.outputs.entries()) {
        const parsed = parsePresence(o.lockingScript.toHex())
        if (parsed && verifyPresence(parsed.ad, parsed.sig)) admit.outputsToAdmit.push(i)
      }
      return admit
    },
    identifyNeededInputs: async () => [],
    getDocumentation: async () =>
      "Admits OP_RETURN outputs 'mamem-agent' <pub> <payload> <sig> where sig is a valid signature by pub over sha256('endpoint|sortedShards|ts'). Stateless — no chain ordering required; latest-by-ts wins in the lookup service.",
    getMetaData: async () => ({ name: 'tm_presence', shortDescription: 'mamem agent-directory: signed presence ads' })
  }
}

interface Stored { ad: PresenceAd, txid: string, outputIndex: number }

export function createLsPresence (kx?: Knex): LookupService & { init: () => Promise<void>, announce: (s: SignedAd) => Promise<boolean> } {
  // latest ad per pub; shard → set of pubs advertising it
  const ads = new Map<string, Stored>()
  const shardIndex = new Map<string, Set<string>>()

  const reindex = (pub: string, ad: PresenceAd | undefined, prev?: PresenceAd) => {
    if (prev) for (const s of prev.shards) { const set = shardIndex.get(s); set?.delete(pub); if (set && set.size === 0) shardIndex.delete(s) }
    if (ad) for (const s of ad.shards) { const set = shardIndex.get(s) ?? new Set<string>(); set.add(pub); shardIndex.set(s, set) }
  }

  const persist = async (s: Stored) => {
    if (!kx) return
    const row = { pub: s.ad.pub, txid: s.txid, output_index: s.outputIndex, endpoint: s.ad.endpoint, shards: JSON.stringify(s.ad.shards), ts: s.ad.ts }
    if (await kx.schema.hasTable(TABLE)) await kx(TABLE).insert(row).onConflict('pub').merge(row)
  }
  const unpersist = async (txid: string, vout: number) => {
    if (!kx || !(await kx.schema.hasTable(TABLE))) return
    await kx(TABLE).where({ txid, output_index: vout }).del()
  }

  const formula = (s: Stored) => ({
    txid: s.txid, outputIndex: s.outputIndex,
    context: [...utf8(JSON.stringify(s.ad))]
  })

  return {
    admissionMode: 'locking-script',
    spendNotificationMode: 'txid',

    init: async () => {
      if (!kx) return
      if (!(await kx.schema.hasTable(TABLE))) {
        await kx.schema.createTable(TABLE, t => {
          t.string('pub').primary()
          t.string('txid').notNullable()
          t.integer('output_index').notNullable()
          t.string('endpoint').notNullable()
          t.string('shards').notNullable()
          t.bigInteger('ts').notNullable()
        })
      }
      for (const r of await kx(TABLE).select('*')) {
        const s: Stored = { ad: { pub: r.pub, endpoint: r.endpoint, shards: JSON.parse(r.shards), ts: Number(r.ts) }, txid: r.txid, outputIndex: r.output_index }
        ads.set(r.pub, s); reindex(r.pub, s.ad)
      }
    },

    outputAdmittedByTopic: (payload: OutputAdmittedByTopic) => {
      if (payload.mode !== 'locking-script') return
      const parsed = parsePresence(typeof payload.lockingScript === 'string' ? payload.lockingScript : payload.lockingScript?.toHex?.() ?? '')
      if (!parsed || !verifyPresence(parsed.ad, parsed.sig)) return
      const prev = ads.get(parsed.ad.pub)
      if (prev && prev.ad.ts >= parsed.ad.ts) return // stale ad — latest wins
      const s: Stored = { ad: parsed.ad, txid: payload.txid, outputIndex: payload.outputIndex }
      ads.set(parsed.ad.pub, s)
      reindex(parsed.ad.pub, parsed.ad, prev?.ad)
      void persist(s)
    },

    // Off-chain rendezvous: same verification, same latest-wins rule, no UTXO.
    // Synthetic outpoint = sha256 of the ad (32B hex — the Engine validates
    // output txids); a newer ad (on- or off-chain) replaces it by ts.
    announce: async (s: SignedAd) => {
      if (!verifyPresence(s.ad, s.sig)) return false
      const prev = ads.get(s.ad.pub)
      if (prev && prev.ad.ts >= s.ad.ts) return true
      const rec: Stored = { ad: s.ad, txid: toHex(digest(s.ad)), outputIndex: 0 }
      ads.set(s.ad.pub, rec)
      reindex(s.ad.pub, s.ad, prev?.ad)
      await persist(rec)
      return true
    },

    outputSpent: (payload: OutputSpent) => {
      if (payload.mode === 'none') return
      for (const [pub, s] of [...ads]) {
        if (s.txid === payload.txid && s.outputIndex === payload.outputIndex) {
          ads.delete(pub); reindex(pub, undefined, s.ad); void unpersist(s.txid, s.outputIndex)
        }
      }
    },
    outputEvicted: (txid: string, outputIndex: number) => {
      for (const [pub, s] of [...ads]) {
        if (s.txid === txid && s.outputIndex === outputIndex) {
          ads.delete(pub); reindex(pub, undefined, s.ad); void unpersist(s.txid, s.outputIndex)
        }
      }
    },

    lookup: async ({ query }: { query?: any }): Promise<LookupFormula> => {
      const live = (s: Stored) => s.ad.exp === undefined || s.ad.exp > Date.now()
      const q = (query ?? {}) as { all?: boolean, shard?: string, pub?: string }
      if (q.all === true) return [...ads.values()].filter(live).map(formula)
      if (typeof q.shard === 'string') return [...(shardIndex.get(q.shard) ?? [])].map(p => ads.get(p)!).filter(live).map(formula)
      if (typeof q.pub === 'string') { const s = ads.get(q.pub); return s && live(s) ? [formula(s)] : [] }
      return []
    },

    getDocumentation: async () =>
      "mamem agent-directory lookup. Queries: {all:true} → latest signed ad per agent; {shard:'x'} → ads of agents serving shard x; {pub:'<66hex>'} → one agent's ad. Context = the ad JSON.",
    getMetaData: async () => ({ name: 'ls_presence', shortDescription: 'mamem agent-directory lookup service' })
  }
}

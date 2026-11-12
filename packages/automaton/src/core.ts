// @nektar/automaton — P14 Merkle Automaton (AUT-001..008).
// Canonical def: {Q, Σ, δ:[from,sym,to,out?], q0, F, O}; deltaRoot = Merkle
// root over fixed 24B leaves num2bin8(from)‖num2bin8(sym)‖num2bin8(to),
// padded to 16 with EMPTY, depth 4. States/symbols = small ints by index.
import { dsha256, utf8, merkleRoot, merkleProof, type MerkleProof } from '@nektar/core'
import { NEKTAR_AUTOMATON_ARTIFACT } from '@nektar/contracts'
export { NEKTAR_AUTOMATON_ARTIFACT }

export class AutomatonError extends Error {}

export interface AutomatonDef {
  Q: string[]
  Sigma: string[]
  delta: [number, number, number, number?][] // [fromIdx, symIdx, toIdx, outIdx?]
  q0: number
  F: number[]
  O: string[]
}

const EMPTY = new Uint8Array(24).fill(0xff) // fixed pad entry

export function canonicalDef(def: AutomatonDef): string {
  return JSON.stringify({ Q: def.Q, Sigma: def.Sigma, delta: def.delta, q0: def.q0, F: def.F, O: def.O })
}
export function defHash(def: AutomatonDef): Uint8Array {
  return dsha256(utf8(canonicalDef(def)))
}

function checkDeterministic(def: AutomatonDef): void {
  const seen = new Set<string>()
  for (const [f, s] of def.delta) {
    const k = `${f},${s}`
    if (seen.has(k)) throw new AutomatonError(`non-deterministic δ: two entries for (${def.Q[f]},${def.Sigma[s]})`)
    seen.add(k)
  }
  if (def.delta.length > 16) throw new AutomatonError('δ exceeds 16 transitions (depth-4 table)')
}

// must equal the contract's num2bin(x, 8n): little-endian, sign bit in the
// top byte (values here are small non-negative indices)
const le8 = (n: number) => {
  const b = Buffer.alloc(8)
  b.writeBigUInt64LE(BigInt(n))
  return new Uint8Array(b)
}
export function leafEntry(f: number, s: number, t: number): Uint8Array {
  return new Uint8Array([...le8(f), ...le8(s), ...le8(t)])
}
/** F committed like δ: Merkle root over 8B leaves num2bin8(q), q ∈ F, padded
 *  to 16 with a fixed EMPTY_F entry (depth 4 — same fold the contract runs). */
const EMPTY_F = new Uint8Array(8).fill(0xff)
export interface FinalTable {
  leaves: Uint8Array[]
  root: Uint8Array
}
export function finalTable(def: AutomatonDef): FinalTable {
  if (def.F.length === 0 || def.F.length > 16) throw new AutomatonError('F must have 1..16 states')
  const leaves = [...new Set(def.F)].sort((a, b) => a - b).map(le8)
  while (leaves.length < 16) leaves.push(EMPTY_F)
  return { leaves, root: merkleRoot(leaves) }
}
export function finalPath(ft: FinalTable, q: number): { sibs: Uint8Array[]; dir: bigint } {
  const leaf = le8(q)
  const i = ft.leaves.findIndex((l) => l.every((b, k) => b === leaf[k]))
  if (i < 0) throw new AutomatonError(`state ${q} is not final — halt refused locally`)
  let dir = 0n
  for (let lvl = 0; lvl < 4; lvl++) if ((i >> lvl) & 1) dir |= 1n << BigInt(lvl)
  return { sibs: merkleProof(ft.leaves, i).siblings, dir }
}

export interface DeltaTable {
  leaves: Uint8Array[]
  root: Uint8Array
  entryIndex: Map<string, number>
}
export function deltaTable(def: AutomatonDef): DeltaTable {
  checkDeterministic(def)
  const entryIndex = new Map<string, number>()
  const leaves: Uint8Array[] = def.delta.map(([f, s, t], i) => {
    entryIndex.set(`${f},${s},${t}`, i)
    return leafEntry(f, s, t)
  })
  while (leaves.length < 16) leaves.push(EMPTY)
  return { leaves, root: merkleRoot(leaves), entryIndex }
}

export function deltaPath(tbl: DeltaTable, f: number, s: number, t: number): { proof: MerkleProof; sibs: Uint8Array[]; dir: bigint } {
  const i = tbl.entryIndex.get(`${f},${s},${t}`)
  if (i === undefined) throw new AutomatonError(`(${f},${s},${t}) not in δ`)
  const proof = merkleProof(tbl.leaves, i)
  const sibs = proof.siblings
  let dir = 0n
  // sibling on the left when index bit is 1
  for (let lvl = 0; lvl < 4; lvl++) if ((i >> lvl) & 1) dir |= 1n << BigInt(lvl)
  return { proof, sibs, dir }
}

/** canonical event leaf payload (q_i, σ_j, q_k, o_n) → fragment data string */
export function eventLeaf(def: AutomatonDef, from: number, sym: number, to: number, out = 0): string {
  return `auto:${def.Q[from]},${def.Sigma[sym]},${def.Q[to]},${def.O[out] ?? '∅'}`
}

export interface TraceEvent {
  from: number
  sym: number
  to: number
  out: number
  leaf: string
}
export interface AnchoredEvent extends TraceEvent {
  anchorTxid: string
  leafProof?: MerkleProof
  batchRoot?: Uint8Array
}

/** AUT-002 light-mode executor: applies sym, appends the event leaf via the
 * memory pipeline (structural interface like GraphAnchor), returns the event */
export class Automaton {
  q: number
  readonly tbl: DeltaTable
  events: TraceEvent[] = []
  constructor(
    public def: AutomatonDef,
    private mem?: { remember: (shard: string, d: string, prov?: any, level?: number) => Promise<{ seq: number }> },
    private shard?: string
  ) {
    checkDeterministic(def)
    this.q = def.q0
    this.tbl = deltaTable(def)
  }

  async step(sym: number): Promise<TraceEvent> {
    const tr = this.def.delta.find(([f, s]) => f === this.q && s === sym)
    if (!tr) throw new AutomatonError(`no δ(${this.def.Q[this.q]},${this.def.Sigma[sym]}) — step rejected locally`)
    const ev: TraceEvent = { from: this.q, sym, to: tr[2], out: tr[3] ?? 0, leaf: eventLeaf(this.def, this.q, sym, tr[2], tr[3] ?? 0) }
    if (this.mem && this.shard) await this.mem.remember(this.shard, ev.leaf)
    this.q = tr[2]!
    this.events.push(ev)
    return ev
  }

  /** AUT-004: word acceptance — replay the word offline, check it ends in F */
  accepts(word: number[]): boolean {
    let q = this.def.q0
    for (const s of word) {
      const tr = this.def.delta.find(([f, x]) => f === q && x === s)
      if (!tr) return false
      q = tr[2]!
    }
    return this.def.F.includes(q)
  }
}

/** AUT-003 trace validity: every event leaf proven against an anchored root,
 * events chain (to_i == from_{i+1}), each (from,sym,to) ∈ δ via entry check.
 * AUT-006: unprovable events are flagged, never surfaced as valid. */
export function verifyTrace(
  def: AutomatonDef,
  events: AnchoredEvent[],
  leafOk: (ev: AnchoredEvent) => boolean // leaf→batchRoot→anchor proof check
): { ok: boolean; reason?: string; flagged: number[] } {
  const tbl = deltaTable(def)
  const flagged: number[] = []
  for (let i = 0; i < events.length; i++) {
    const ev = events[i]
    if (!tbl.entryIndex.has(`${ev.from},${ev.sym},${ev.to}`)) {
      flagged.push(i)
      continue
    }
    if (i > 0 && events[i - 1].to !== ev.from) {
      flagged.push(i)
      continue
    }
    if (!leafOk(ev)) {
      flagged.push(i)
      continue
    }
  }
  if (flagged.length > 0) return { ok: false, reason: `${flagged.length} event(s) flagged: not anchored/chain-broken/not in δ`, flagged }
  return { ok: true, flagged }
}

/** AUT-008: off-chain mirror reconcile after a reorg — re-read live state;
 * return the mirror's decision (rollback if the live head diverges). */
export interface MirrorState {
  q: number
  n: number
  headTxid?: string
}
export function reconcile(mirror: MirrorState, live: MirrorState): { action: 'in-sync' | 'rollback' | 'rebroadcast'; reason: string } {
  if (mirror.headTxid === live.headTxid && mirror.q === live.q) return { action: 'in-sync', reason: 'mirror matches live head' }
  if (live.headTxid === undefined) return { action: 'rebroadcast', reason: 'live head lost — re-broadcast mirror state' }
  return { action: 'rollback', reason: `live head diverged (q=${live.q}) — mirror rolls back to live` }
}

// AutomatonChain — AUT-005 full mode on the production path: the same
// NektarProvider/Broadcaster/Funder stack AnchorChain uses. A fresh contract is
// rehydrated from the decoded on-chain head for every call (runar mutates
// state at prepareCall), the funding key is separate from the owner key, and
// the new head is decoded from the broadcast continuation — never recomputed.
import { Transaction, PrivateKey } from '@bsv/sdk'
import { RunarContract, LocalSigner, extractStateFromScript, type Provider, type Signer } from 'runar-sdk'
import { NEKTAR_AUTOMATON_ARTIFACT } from '@nektar/contracts'
import { NektarProvider } from '@nektar/anchor'
import { hash160, toHex, fromHex, runarSigner, signerPubHex, toSigner, type SignerLike } from '@nektar/core'
import type { Broadcaster, Funder } from '@nektar/chain'
import { AutomatonError, deltaTable, deltaPath, finalTable, finalPath, type AutomatonDef, type FinalTable } from './core.js'

export interface DecodedAutomaton {
  q: number
  deltaRoot: string
  ownerPkh: string
  n: number
  lastOut: string
  finalRoot: string
  halted: boolean
}
export interface AutomatonHead {
  tx: Transaction
  vout: number
  state: DecodedAutomaton
}

const args = (s: DecodedAutomaton) => [BigInt(s.q), s.deltaRoot, s.ownerPkh, BigInt(s.n), s.lastOut, s.finalRoot, s.halted ? 1n : 0n]

/** Decode + byte-authenticate a NektarAutomaton locking script: parse the state
 *  suffix, rebuild the expected script from it and require exact equality. */
export function decodeNektarAutomaton(scriptHex: string): DecodedAutomaton | null {
  let st: Record<string, unknown> | null
  try {
    st = extractStateFromScript(NEKTAR_AUTOMATON_ARTIFACT, scriptHex)
  } catch {
    return null
  }
  if (!st) return null
  const s: DecodedAutomaton = {
    q: Number(st.q),
    deltaRoot: String(st.deltaRoot),
    ownerPkh: String(st.ownerPkh),
    n: Number(st.n),
    lastOut: typeof st.lastOut === 'string' ? st.lastOut : '',
    finalRoot: String(st.finalRoot),
    halted: BigInt(st.halted as bigint) === 1n
  }
  const expected = new RunarContract(NEKTAR_AUTOMATON_ARTIFACT, args(s)).getLockingScript()
  return expected === scriptHex ? s : null
}

const pkhOf = (pub: string) => toHex(hash160(fromHex(pub)))

export class AutomatonChain {
  private signer: Signer
  private provider: NektarProvider
  // NektarProvider is declared against @nektar/anchor's runar-sdk type copy;
  // structurally identical, nominally distinct across the two package graphs
  private get runarProvider(): Provider {
    return this.provider
  }
  private head?: AutomatonHead
  private txById = new Map<string, Transaction>()
  readonly tbl
  readonly ftbl: FinalTable

  constructor(
    readonly def: AutomatonDef,
    private ownerKey: SignerLike,
    private funder: Funder,
    fundKey: SignerLike,
    broadcaster: Broadcaster
  ) {
    this.tbl = deltaTable(def)
    this.ftbl = finalTable(def)
    this.signer = fundKey instanceof PrivateKey ? new LocalSigner(fundKey.toHex()) : runarSigner(fundKey)
    this.provider = new NektarProvider(broadcaster, funder)
    this.provider.onSuccess = (tx) => {
      this.txById.set(tx.id('hex'), tx)
    }
  }

  getHead(): AutomatonHead | undefined {
    return this.head
  }
  /** adversarial testing only: skip local guards so the CONTRACT is the one
   *  that must refuse (never set in production code paths) */
  unguarded = false

  async deploy(): Promise<AutomatonHead> {
    if (this.head) throw new AutomatonError('automaton already deployed')
    this.provider.funderAddress = await this.signer.getAddress()
    await this.requireFunding()
    const pub = await signerPubHex(this.ownerKey)
    const c = new RunarContract(NEKTAR_AUTOMATON_ARTIFACT, [BigInt(this.def.q0), toHex(this.tbl.root), pkhOf(pub), 0n, '', toHex(this.ftbl.root), 0n])
    c.connect(this.runarProvider, this.signer)
    await c.deploy({ satoshis: 1 })
    return this.accept()
  }

  /** full-mode step: (from=q, sym, to) must be in δ — the contract enforces it
   *  on-chain; `outHash` (32B) commits the step's output event. */
  async step(sym: number, to: number, outHash: string, opts: { from?: number; pubKey?: string; sibs?: string[]; dir?: bigint } = {}): Promise<AutomatonHead> {
    const cur = this.requireHead()
    const from = opts.from ?? cur.state.q
    let sibs = opts.sibs
    let dir = opts.dir
    if (!sibs || dir === undefined) {
      const p = deltaPath(this.tbl, from, sym, to)
      sibs ??= p.sibs.map(toHex)
      dir ??= p.dir
    }
    return await this.call('step', [BigInt(from), BigInt(sym), BigInt(to), outHash, ...sibs, dir], opts.pubKey)
  }

  /** final transition: valid only when q ∈ F; the continuation records
   *  halted=1 and the contract refuses any further step. */
  async halt(opts: { pubKey?: string; asState?: number } = {}): Promise<AutomatonHead> {
    // asState (adversarial tests): present the F-path of another state
    const p = finalPath(this.ftbl, opts.asState ?? this.requireHead().state.q)
    return await this.call('halt', [...p.sibs.map(toHex), p.dir], opts.pubKey)
  }

  private async call(method: string, rest: unknown[], pubKey?: string): Promise<AutomatonHead> {
    const cur = this.requireHead()
    if (cur.state.halted && !this.unguarded) throw new AutomatonError('automaton halted: no further transitions')
    await this.requireFunding()
    const c = new RunarContract(NEKTAR_AUTOMATON_ARTIFACT, args(cur.state))
    c.connect(this.runarProvider, this.signer)
    const script = cur.tx.outputs[cur.vout].lockingScript.toHex()
    ;(c as unknown as { currentUtxo: unknown }).currentUtxo = {
      txid: cur.tx.id('hex'),
      outputIndex: cur.vout,
      satoshis: cur.tx.outputs[cur.vout].satoshis!,
      script
    }
    const pub = pubKey ?? (await signerPubHex(this.ownerKey))
    const prep = await c.prepareCall(method, [null, pub, ...rest])
    const der = Buffer.from((await toSigner(this.ownerKey).sign(Array.from(Buffer.from(prep.sighash, 'hex')))).toDER()).toString('hex') + '41'
    const sigMap: Record<number, string> = {}
    for (const i of prep.sigIndices) sigMap[i] = der
    await c.finalizeCall(prep, sigMap)
    return this.accept()
  }

  private accept(): AutomatonHead {
    const raw = this.provider.lastTx
    if (!raw) throw new AutomatonError('no broadcast tx')
    // runar-sdk builds txs with its own (older) @bsv/sdk class — normalize to
    // the root SDK before attaching parents or serializing again
    const tx = Transaction.fromHex(raw.toHex())
    this.txById.set(tx.id('hex'), tx)
    for (const inp of tx.inputs) {
      if (inp.sourceTransaction || !inp.sourceTXID) continue
      const fund = ((this.funder as unknown as { utxos?: Array<{ tx: Transaction }> }).utxos ?? []).find((u) => u.tx.id('hex') === inp.sourceTXID)?.tx
      const parent = this.txById.get(inp.sourceTXID) ?? this.provider.taken.get(inp.sourceTXID) ?? fund
      if (parent) inp.sourceTransaction = parent
    }
    const state = decodeNektarAutomaton(tx.outputs[0].lockingScript.toHex())
    if (!state) throw new AutomatonError('continuation output is not an authentic NektarAutomaton')
    this.head = { tx, vout: 0, state }
    return this.head
  }

  private requireHead(): AutomatonHead {
    if (!this.head) throw new AutomatonError('automaton not deployed')
    return this.head
  }

  private async requireFunding(): Promise<void> {
    const f = await this.provider.getUtxos(await this.signer.getAddress())
    if (f.length === 0) throw new AutomatonError('reserve dry: no funding UTXO available')
  }
}

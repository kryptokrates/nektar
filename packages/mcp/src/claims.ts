// ClaimService — the optimistic-claim layer as an operable surface.
// Thin wrapper: business logic lives in @nektar/chain (claims.ts); this
// binds it to a NektarMemory instance's provider/signer + the shared
// data-dir registry & proof store the watcher service reads.
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { Transaction, Hash } from '@bsv/sdk'
import { NektarProvider } from '@nektar/anchor'
import { deployClaim, readClaimState, callTerminal, outpointOf, p2pkh, registerClaim, storeProof } from '@nektar/chain'
import { verifyZkm2 } from '@nektar/zk'
import { LocalSigner } from 'runar-sdk'
import type { NektarMemory } from './memory.js'

export interface ClaimServiceDeps {
  dataDir: string
  contractsDir: string
  fetchTx: (txid: string) => Promise<string | undefined> // raw tx hex
}

const loadArt = (dir: string, name: string) => JSON.parse(readFileSync(join(dir, `${name}.json`), 'utf8'))

export class ClaimService {
  private cArt: any
  private vArt: any
  private aArt: any
  private sArt: any
  private provider: NektarProvider
  private signer: LocalSigner
  private pubKeyHex: string
  private pkh: string

  constructor(
    private mem: NektarMemory,
    private deps: ClaimServiceDeps
  ) {
    this.cArt = loadArt(deps.contractsDir, 'NektarZkClaim')
    this.vArt = loadArt(deps.contractsDir, 'ClaimVerdict')
    this.aArt = loadArt(deps.contractsDir, 'NektarAnchor')
    this.sArt = loadArt(deps.contractsDir, 'DisclosureSale')
    const key = mem.operatorKey()
    this.provider = new NektarProvider({ broadcast: (t: Transaction) => mem.broadcastTx(t) } as any, (mem as any).funder, key.toPublicKey().toAddress([0x6f]))
    this.signer = new LocalSigner(key.toWif())
    this.pubKeyHex = key.toPublicKey().toString()
    this.pkh = Buffer.from(Hash.hash160(Buffer.from(this.pubKeyHex, 'hex'))).toString('hex')
  }

  private get claimsFile() {
    return join(this.deps.dataDir, 'claims.json')
  }
  private get proofDir() {
    return join(this.deps.dataDir, 'proofs')
  }

  /** deploy an open claim bound to an L_ZKMem proof bundle's journal.
   *  Registers the outpoint (the watcher picks it up) + stores the bundle. */
  async deploy(lzkmemHex: string, opts: { bondSats?: number; deadline?: number; anchorOutpoint?: string } = {}) {
    const bytes = new Uint8Array(Buffer.from(lzkmemHex, 'hex'))
    const v = verifyZkm2(bytes)
    if (!v.ok) throw new Error(`proof does not verify: ${v.reason}`)
    const receiptHash = Buffer.from(Hash.sha256(bytes)).toString('hex')
    const s = {
      status: 0n,
      receiptHash,
      journalRoot: v.journal.root,
      journalSize: BigInt(v.journal.size),
      queryId: v.journal.queryId,
      bondSats: BigInt(opts.bondSats ?? 5000),
      deadline: BigInt(opts.deadline ?? 0),
      responseDeadline: 0n,
      challengerPkh: '00'.repeat(32),
      bisectLo: 0n,
      bisectHi: 4n,
      midCommit: '00'.repeat(32),
      proverPkh: this.pkh,
      maxFee: 1000n,
      window: 100n,
      stages: 4n,
      verdictCode: this.vArt.script,
      anchorOutpoint: opts.anchorOutpoint ?? ''
    }
    const ctor = [
      s.status,
      s.receiptHash,
      s.journalRoot,
      s.journalSize,
      s.queryId,
      s.bondSats,
      s.deadline,
      s.responseDeadline,
      s.challengerPkh,
      s.bisectLo,
      s.bisectHi,
      s.midCommit,
      s.proverPkh,
      s.maxFee,
      s.window,
      s.stages,
      s.verdictCode,
      s.anchorOutpoint
    ]
    const { contract, utxo } = await deployClaim(this.cArt, ctor, this.provider, this.signer, Number(s.bondSats))
    registerClaim(this.claimsFile, { txid: utxo.txid, vout: utxo.outputIndex })
    storeProof(this.proofDir, receiptHash, bytes)
    return {
      txid: utxo.txid,
      vout: utxo.outputIndex,
      receiptHash,
      journalRoot: v.journal.root,
      status: 'open',
      statement: 'claim registered; the watcher re-verifies it every poll — a fraudulent claim gets challenged automatically'
    }
  }

  /** read a deployed claim's covenant state. */
  async status(txid: string, vout = 0) {
    const raw = await this.deps.fetchTx(txid)
    if (!raw) return { found: false, txid }
    const tx = Transaction.fromHex(raw)
    const out = tx.outputs[vout]
    if (!out) return { found: false, txid, reason: `no output ${vout}` }
    const script = out.lockingScript.toHex()
    if (!script.startsWith(this.cArt.script)) return { found: false, txid, reason: 'not a NektarZkClaim output' }
    const st = readClaimState(this.cArt, script)
    const STATUS = ['open', 'challenged', 'revealed', 'bisecting']
    return {
      found: true,
      txid,
      vout,
      satoshis: out.satoshis,
      status: STATUS[Number(st.status)] ?? `status=${st.status}`,
      deadline: Number(st.deadline),
      journalRoot: st.journalRoot,
      proverPkh: st.proverPkh
    }
  }

  /** unanchored settle: emits the ClaimVerdict UTXO + bond payout.
   *  Only for claims deployed with anchorOutpoint ''. */
  async settle(txid: string, vout = 0, payoutSats = 4980, verdictSats = 10) {
    const st = await this.status(txid, vout)
    if (!st.found) throw new Error(`claim ${txid}:${vout} not found`)
    const raw = await this.deps.fetchTx(txid)
    const tx = Transaction.fromHex(raw!)
    const cUtxo = { txid, outputIndex: vout, satoshis: tx.outputs[vout]!.satoshis!, script: tx.outputs[vout]!.lockingScript.toHex() }
    const state = readClaimState(this.cArt, cUtxo.script)
    const ctor = [
      state.status,
      state.receiptHash,
      state.journalRoot,
      state.journalSize,
      state.queryId,
      state.bondSats,
      state.deadline,
      state.responseDeadline,
      state.challengerPkh,
      state.bisectLo,
      state.bisectHi,
      state.midCommit,
      state.proverPkh,
      state.maxFee,
      state.window,
      state.stages,
      state.verdictCode,
      state.anchorOutpoint
    ]
    const { RunarContract } = await import('runar-sdk')
    const c = new RunarContract(this.cArt, ctor)
    c.connect(this.provider, this.signer)
    ;(c as any).currentUtxo = cUtxo
    const verdict =
      state.verdictCode +
      '6a' +
      (await import('runar-sdk')).serializeState(this.vArt.stateFields, {
        receiptHash: state.receiptHash,
        journalRoot: state.journalRoot,
        queryId: state.queryId,
        payeePkh: state.proverPkh
      })
    const r = await callTerminal(
      c,
      'settle',
      [null, this.pubKeyHex, outpointOf(cUtxo), BigInt(payoutSats), BigInt(verdictSats)],
      [
        { scriptHex: verdict, satoshis: verdictSats },
        { scriptHex: p2pkh(state.proverPkh), satoshis: payoutSats }
      ],
      state.proverPkh,
      this.signer,
      0n
    )
    return {
      txid: r.txid,
      verdictOutpoint: outpointOf({ txid: r.txid, outputIndex: 0 }),
      statement: 'settled — the ClaimVerdict UTXO is the spendable proof of acceptance'
    }
  }

  /** deploy a DisclosureSale bound to a settled verdict — the sale can only
   *  be bought by spending BOTH inputs (pay-iff-settled, atomic). */
  async deploySale(verdictOutpoint: string, priceSats: number, wrappedHex: string) {
    const { RunarContract } = await import('runar-sdk')
    const hw = Buffer.from(Hash.sha256(Buffer.from(wrappedHex, 'hex'))).toString('hex')
    const sale = new RunarContract(this.sArt, [this.pkh, BigInt(priceSats), hw, 0n, 1000n, this.pkh, verdictOutpoint])
    sale.connect(this.provider, this.signer)
    await sale.deploy({ satoshis: priceSats + 1000 })
    const u = (sale as any).currentUtxo
    return { txid: u.txid, vout: u.outputIndex, statement: 'sale deployed — only unlockable in a tx that also spends the bound verdict' }
  }

  /** execute the gated buy: [sale, verdict] → [payment to payee, change].
   *  Both covenant legs enforce the identical output list — consensus
   *  refuses the buy without the verdict. */
  async buyGated(saleTxid: string, verdictOutpoint: string, wrappedHex: string, priceSats: number, changeSats = 900) {
    const { RunarContract } = await import('runar-sdk')
    const { buildBuyGatedTx } = await import('@nektar/chain')
    const op = Buffer.from(verdictOutpoint, 'hex')
    const vTxid = Buffer.from(op.subarray(0, 32)).reverse().toString('hex')
    const vVout = op[32]
    const sRaw = await this.deps.fetchTx(saleTxid)
    const vRaw = await this.deps.fetchTx(vTxid)
    if (!sRaw || !vRaw) throw new Error('sale or verdict tx not found')
    const sTx = Transaction.fromHex(sRaw),
      vTx = Transaction.fromHex(vRaw)
    const saleUtxo = { txid: saleTxid, outputIndex: 0, satoshis: sTx.outputs[0]!.satoshis!, script: sTx.outputs[0]!.lockingScript.toHex() }
    const verdictUtxo = { txid: vTxid, outputIndex: vVout, satoshis: vTx.outputs[vVout]!.satoshis!, script: vTx.outputs[vVout]!.lockingScript.toHex() }
    const hw = Buffer.from(Hash.sha256(Buffer.from(wrappedHex, 'hex'))).toString('hex')
    const sale = new RunarContract(this.sArt, [this.pkh, BigInt(priceSats), hw, 0n, 1000n, this.pkh, verdictOutpoint])
    // verdict ctor = its state fields (deserialize the deployed script tail)
    const { deserializeState } = await import('runar-sdk')
    const vState = deserializeState(this.vArt.stateFields, verdictUtxo.script.slice(this.vArt.script.length + 2))
    const verdict = new RunarContract(this.vArt, [vState.receiptHash, vState.journalRoot, vState.queryId, vState.payeePkh])
    let tx
    try {
      tx = await buildBuyGatedTx({
        saleArt: this.sArt,
        sale,
        saleUtxo,
        verdictArt: this.vArt,
        verdict,
        verdictUtxo,
        wrapped: wrappedHex,
        saleSats: priceSats,
        changeSats,
        changePkh: this.pkh
      })
    } catch (e) {
      throw new Error(
        `buyGated build: ${(e as Error).message} (saleUtxo.script=${saleUtxo.script.length / 2}B verdictUtxo.script=${verdictUtxo.script.length / 2}B)`,
        { cause: e }
      )
    }
    const r = await this.mem.broadcastTx(tx)
    if (r.status !== 'success') throw new Error(`buyGated: ${r.description?.slice(0, 120)}`)
    this.provider.markAccepted(tx)
    return { txid: tx.id('hex'), statement: 'gated buy executed — the verdict was consumed atomically with the payment' }
  }
}

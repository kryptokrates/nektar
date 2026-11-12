// Host adapter for the V2 NektarCheckpointBond covenant (GOAL-v7).
// Spends are built through runar-sdk primitives (buildStatefulPrefix /
// buildUnlockingScript / computeOpPushTxWithCodeSep) — no hand-written
// Bitcoin Script. The compiled covenant verifies the injected
// _changePKH/_changeAmount slots on every public method, so each spend tx
// carries the 1-sat change output to the operator.
import { Transaction, LockingScript, UnlockingScript, Hash } from '@bsv/sdk'
import { RunarContract, computeOpPushTx, serializeState, extractStateFromScript, buildP2PKHScript } from 'runar-sdk'
import { unlockP2PKH, signerPubHex, toSigner, type SignerLike } from '@nektar/core'
import { NEKTAR_CHECKPOINTBOND_ARTIFACT } from '@nektar/contracts'
import type { Broadcaster } from '@nektar/chain'

export interface BondConfig {
  operatorPkh: string // 20B hex — checkpoint writer
  bondReleaseHeight: bigint // locktime gate for close()
  maxFee: bigint
  epochLen: bigint // §12 SLA window — commit renews deadline to nLockTime+epochLen
}

export interface BondUtxo {
  txid: string
  vout: number
  satoshis: number
  script: string
}
export interface BondState {
  n: bigint
  lastRoot: string
  deadline: bigint
  seized: bigint
}

export class CheckpointBond {
  private contract: RunarContract
  constructor(private cfg: BondConfig) {
    this.contract = new RunarContract(NEKTAR_CHECKPOINTBOND_ARTIFACT, [cfg.operatorPkh, cfg.bondReleaseHeight, cfg.maxFee, cfg.epochLen])
  }

  get params(): BondConfig {
    return this.cfg
  }

  private stateHex(n: bigint, lastRoot: string, deadline: bigint, seized: bigint): string {
    const c = this.cfg
    return serializeState(NEKTAR_CHECKPOINTBOND_ARTIFACT.stateFields, {
      operatorPkh: c.operatorPkh,
      bondReleaseHeight: c.bondReleaseHeight,
      maxFee: c.maxFee,
      epochLen: c.epochLen,
      n,
      lastRoot,
      deadline,
      seized
    })
  }

  /** bond locking script = code part + OP_RETURN + serialized state.
   *  Genesis: deadline = releaseHeight (first commit must land before it). */
  lockingScript(n = 0n, lastRoot = '', deadline?: bigint, seized = 0n): string {
    return (this.contract as any).getCodePartHex() + '6a' + this.stateHex(n, lastRoot, deadline ?? this.cfg.bondReleaseHeight, seized)
  }

  /** read the full on-chain state back out of a bond script */
  stateOf(script: string): BondState {
    const d = extractStateFromScript(NEKTAR_CHECKPOINTBOND_ARTIFACT, script) as any
    if (!d) throw new Error('bond: not a stateful bond script')
    const num = (v: unknown) => (typeof v === 'bigint' ? v : BigInt(v as string))
    return {
      n: num(d.n),
      deadline: num(d.deadline),
      seized: num(d.seized),
      lastRoot: typeof d.lastRoot === 'string' ? d.lastRoot : Buffer.from(d.lastRoot).toString('hex')
    }
  }

  /** deploy: spend a P2PKH funding utxo into the genesis bond output
   *  (n=0, empty lastRoot). Plain funding tx — not a covenant call. */
  /** builds the unsigned deploy tx — `utxo` is null until signed: txid()
   *  serializes inputs and the unsigned unlock can't serialize. Sign first,
   *  then call `deployUtxo`. */
  buildDeploy(
    fundUtxo: { tx: Transaction; vout: number; satoshis: number },
    key: SignerLike,
    sats: number,
    fee = 1n
  ): { tx: Transaction; sats: number; script: string } {
    const script = this.lockingScript(0n, '')
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: fundUtxo.tx, sourceOutputIndex: fundUtxo.vout, unlockingScriptTemplate: unlockP2PKH(key) })
    tx.addOutput({ satoshis: sats, lockingScript: LockingScript.fromHex(script) })
    const change = BigInt(fundUtxo.satoshis) - BigInt(sats) - fee
    if (change > 546n) tx.addOutput({ satoshis: Number(change), lockingScript: LockingScript.fromHex(buildP2PKHScript(this.cfg.operatorPkh)) })
    return { tx, sats, script }
  }

  /** the bond utxo once the deploy tx is signed */
  deployUtxo(tx: Transaction, sats: number, script: string): BondUtxo {
    return { txid: tx.id('hex'), vout: 0, satoshis: sats, script }
  }

  /** shared spend machinery: covenant preimage + opPushTx sig + user sig for the
   *  given method, all computed over the final lockTime-bearing tx. */
  private async spend(
    utxo: BondUtxo,
    methodIndex: number,
    method: string,
    args: unknown[],
    outputs: { satoshis: number; script: string }[],
    lockTime: number,
    broadcaster: Broadcaster,
    ownerKey?: SignerLike,
    parentTx?: Transaction
  ): Promise<{ tx: Transaction }> {
    const tx = new Transaction()
    // the REAL parent tx — a synthetic one mismatches the outpoint txid in
    // the covenant's preimage and produces an invalid unlock on-chain.
    tx.addInput({
      sourceTXID: utxo.txid,
      sourceTransaction: parentTx,
      sourceOutputIndex: utxo.vout,
      unlockingScript: new UnlockingScript(),
      sequence: 0xfffffffe
    })
    for (const o of outputs) tx.addOutput({ satoshis: o.satoshis, lockingScript: LockingScript.fromHex(o.script) })
    tx.lockTime = lockTime // sequence < final so consensus enforces nLockTime
    const c = this.contract as any
    const { sigHex: opSig, preimageHex } = c.computeOpPushTxWithCodeSep(tx, 0, utxo.script, utxo.satoshis, methodIndex)
    const resolved: unknown[] = []
    for (const a of args) {
      if (a === 'PREIMAGE') resolved.push(preimageHex)
      else if (a === 'SIG') {
        const sub = c.getSubscriptForSigning(utxo.script, methodIndex)
        const pre = computeOpPushTx(tx, 0, sub, utxo.satoshis).preimageHex
        resolved.push(Buffer.from((await toSigner(ownerKey!).sign(Array.from(Hash.sha256(Buffer.from(pre, 'hex'))))).toDER()).toString('hex') + '41')
      } else resolved.push(a)
    }
    const unlock = c.buildStatefulPrefix(opSig, true) + this.contract.buildUnlockingScript(method, resolved)
    tx.inputs[0].unlockingScript = UnlockingScript.fromHex(unlock)
    const r = await broadcaster.broadcast(tx)
    if (r.status !== 'success') throw new Error(`bond ${method} refused: ${r.description ?? r.status}`)
    return { tx }
  }

  /** commit: continuation {n+1, newRoot, deadline=lockTime+epochLen} + 1-sat
   *  change to operator (the covenant verifies _changePKH/_changeAmount). */
  async buildCommit(
    utxo: BondUtxo,
    ownerKey: SignerLike,
    newRoot: string,
    lockTime: number,
    broadcaster: Broadcaster,
    fee = 1n,
    parentTx?: Transaction
  ): Promise<{ tx: Transaction; state: BondState }> {
    const cur = this.stateOf(utxo.script)
    const bondSats = BigInt(utxo.satoshis) - fee - 1n // residual = fee + 1-sat change ≤ maxFee+1
    const next: BondState = { n: cur.n + 1n, lastRoot: newRoot, deadline: BigInt(lockTime) + this.cfg.epochLen, seized: cur.seized }
    const contScript = (this.contract as any).getCodePartHex() + '6a' + this.stateHex(next.n, next.lastRoot, next.deadline, next.seized)
    const { tx } = await this.spend(
      utxo,
      0,
      'commit',
      ['SIG', await signerPubHex(ownerKey), newRoot, bondSats, next.deadline, this.cfg.operatorPkh, 1n, 'PREIMAGE'],
      [
        { satoshis: Number(bondSats), script: contScript },
        { satoshis: 1, script: buildP2PKHScript(this.cfg.operatorPkh) }
      ],
      lockTime,
      broadcaster,
      ownerKey,
      parentTx
    )
    return { tx, state: next }
  }

  /** seize: permissionless — valid only once the on-chain deadline has passed.
   *  `payTo` is the seizer's locking script; the bond (minus fee, the 1-sat
   *  inert husk and the change slot) pays out there. No signature. */
  async buildSeize(
    utxo: BondUtxo,
    payToScript: string,
    lockTime: number,
    broadcaster: Broadcaster,
    fee = 1n,
    parentTx?: Transaction
  ): Promise<{ tx: Transaction }> {
    const cur = this.stateOf(utxo.script)
    const payoutSats = BigInt(utxo.satoshis) - fee - 1n - 1n // −fee −husk −change
    const huskScript = (this.contract as any).getCodePartHex() + '6a' + this.stateHex(cur.n, cur.lastRoot, cur.deadline, 1n)
    const { tx } = await this.spend(
      utxo,
      1,
      'seize',
      [payToScript, payoutSats, this.cfg.operatorPkh, 1n, 'PREIMAGE'],
      [
        { satoshis: Number(payoutSats), script: payToScript },
        { satoshis: 1, script: huskScript },
        { satoshis: 1, script: buildP2PKHScript(this.cfg.operatorPkh) }
      ],
      lockTime,
      broadcaster,
      undefined,
      parentTx
    )
    return { tx }
  }

  /** close: terminal — whole bond (minus bounded fee) back to operator after
   *  release height. Emitted payout at vout 0, covenant change slot at vout 1. */
  async buildClose(
    utxo: BondUtxo,
    ownerKey: SignerLike,
    lockTime: number,
    broadcaster: Broadcaster,
    fee = 1n,
    parentTx?: Transaction
  ): Promise<{ tx: Transaction }> {
    const payoutSats = BigInt(utxo.satoshis) - fee - 1n // residual = fee + 1-sat change ≤ maxFee+1
    const { tx } = await this.spend(
      utxo,
      2,
      'close',
      ['SIG', await signerPubHex(ownerKey), payoutSats, this.cfg.operatorPkh, 1n, 'PREIMAGE'],
      [
        { satoshis: Number(payoutSats), script: buildP2PKHScript(this.cfg.operatorPkh) },
        { satoshis: 1, script: buildP2PKHScript(this.cfg.operatorPkh) }
      ],
      lockTime,
      broadcaster,
      ownerKey,
      parentTx
    )
    return { tx }
  }
}

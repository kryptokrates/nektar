// MemCustody — the spending delegates (CustodyOps: takeFunding/pay/send/
// provision/fundingStatus) plus the P14 full-mode automata surface (AUT-005).
import { PrivateKey } from '@bsv/sdk'
import { AutomatonChain, type AutomatonDef, type AutomatonHead } from '@nektar/automaton'
import { custodySigner, type SignerLike } from '@nektar/core'
import type { FundingOut, FundsPurpose } from '@nektar/chain'
import type { Transaction } from '@bsv/sdk'
import { MemAnchor } from './anchor.js'

export abstract class MemCustody extends MemAnchor {
  /** funding outs for covenant hosts (escrow deposits, covenant inputs) —
   *  callers record the broadcast via funds.spent() like anchorAttestation
   *  does. Purpose defaults to 'publish' — covenant callers pass their own. */
  async fundingOuts(satoshis: number, purpose: FundsPurpose = 'publish'): Promise<FundingOut[]> {
    const u = await this.takeFunding(satoshis, purpose)
    return [u]
  }

  /** build + sign the payment tx for an invoice (payQuote → invoice fields)
   *  using the instance's own fund key — the agent pays without an external
   *  wallet. §16 audit trail: settlement IS an anchored tx — the payment
   *  receipt is publicly re-verifiable, not a server-side claim. */
  async payInvoice(inv: {
    invoiceId: string
    amount: number
    lockingScriptHex: string
    opReturn: string
  }): Promise<{ payment: { invoiceId: string; transaction: string }; txid: string }> {
    return await this.custodyOps.payInvoice(inv)
  }

  /** one-call payment: build + sign + broadcast a plain spend to a payee
   *  address or locking script. Unlike payInvoice (which returns a BEEF for
   *  the payee's settlePayment), this airs the tx and returns its txid —
   *  the way to fund another agent's fee address. `buildOnly` returns the
   *  fee'd, template-unlocked tx without broadcasting — the wallet-custody
   *  path signs and submits it itself (market direct rail). `utxo` names
   *  the payment output so an accept envelope can point at it. */
  async sendSats(args: {
    amount: number
    address?: string
    scriptHex?: string
    opReturn?: string
    buildOnly?: boolean
  }): Promise<{ txid: string; paidSats: number; tx?: Transaction; txHex?: string; utxo?: { txid: string; vout: number; satoshis: number; script: string } }> {
    return await this.custodyOps.sendSats(args)
  }

  async provisionPay(args: {
    outputs: Array<{ satoshis: number; scriptHex: string }>
    address?: string
    fundsSats?: number
  }): Promise<{ txid: string; hex: string; outputs: number; fee: number }> {
    return await this.custodyOps.provisionPay(args)
  }

  /** one funding outpoint reserved for `purpose` — the Funds impl owns
   *  selection and the reservation */
  protected async takeFunding(satoshis: number, purpose: FundsPurpose = 'publish'): Promise<FundingOut> {
    return await this.custodyOps.takeFunding(satoshis, purpose)
  }

  /** fee-reserve status for the fundingStatus MCP tool — mem-side view only
   *  (address + tracked UTXOs); spend caps/runway live in ops.wallet() which
   *  the tool prefers when the runtime passes it through. */
  async fundingStatus(): Promise<{ address: string; balanceSats: number; utxos: number; funded: boolean }> {
    return await this.custodyOps.fundingStatus()
  }

  // ── P14 full-mode automata (AUT-005): one Rúnar NektarAutomaton UTXO per id,
  // funded by the instance funder, owned by its own key.
  async automatonDeploy(id: string, def: AutomatonDef, ownerKey?: SignerLike): Promise<AutomatonHead> {
    if (this.automata.has(id)) throw new Error(`automaton ${id} already deployed`)
    // wallet custody: a per-automaton derived keyID — deterministic across
    // restarts (same wallet → same owner pubkey), no stored key material.
    const owner =
      ownerKey ??
      (this.opts.wallet !== undefined
        ? custodySigner(this.opts.wallet, { protocolID: [2, 'nektar'], keyID: `nektar-automaton:${id}`, counterparty: 'self' })
        : PrivateKey.fromRandom())
    const ac = new AutomatonChain(def, owner, this.funds, this.fundSigner(), this.chain)
    const h = await ac.deploy()
    this.automata.set(id, ac)
    return h
  }
  automaton(id: string): AutomatonChain {
    const ac = this.automata.get(id)
    if (!ac) throw new Error(`no automaton ${id}`)
    return ac
  }
}

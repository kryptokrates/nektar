// paid-disclosure sale ops — extracted verbatim from the server.ts dispatchTool switch
import type { NektarMemory } from '../memory.js'
import type { ToolHandler } from './shared.js'
import { PublicKey } from '@bsv/sdk'

const SALE_OPS = new Set(['offer', 'offerMemory', 'escrow', 'complete', 'refund', 'claim'])

// the covenant's maxFee must cover the disclosure tx's relay fee AND still
// leave a spendable change for the buyer — a flat floor underpays a big
// payload into an arithmetically un-settleable quote: even zeroing change,
// utxo − price = maxFee < the tx's relay cost, so complete() can only fail.
// Sized to the payload: ~1 sat/KB (BSV relay floor) over an estimated
// wrapped+script size, plus a change margin above the dust floor (~135
// sats) so the depositor's return output is standard, not dust.
const saleFeeBound = (payloadBytes: number, floor: number): number => Math.max(floor, Math.ceil((payloadBytes + 4096) / 1000) + 150)

// the complete/refund spend should pay the relay fee its size actually
// needs — a flat cap overpays small spends and starves big ones, both
// inside the same covenant bound
const relayNeed = (payloadBytes: number): number => Math.ceil((payloadBytes + 2048) / 1000)

async function settleEscrow(mem: NektarMemory, tx: import('@bsv/sdk').Transaction): Promise<void> {
  await mem.funds.spent(tx)
}

// sale#escrow and sale#escrowBuild share one body — the build variant is
// selected by the tool name, so the handler receives it as the 4th param
const escrow: ToolHandler = async (mem, args, _d, name) => {
  const isBuild = name === 'sale#escrowBuild'
  const { DisclosureSaleHost } = await import('@nektar/anchor')
  // §16/17 delegation-sig check: refuse to fund an escrow for a cfg the
  // seller never signed — blocks MITM cfg-swap griefing (deposit lockup).
  if (args.offer) {
    const { verifyOfferSig } = await import('@nektar/identity')
    const { reconstructRoot, toHex: th, fromHex: fh } = await import('@nektar/core')
    // operator pub via the signing surface — held-key returns the fund key's
    // pub, external custody the wallet's identity pub; operatorKey() would
    // throw under NEKTAR_WALLET=client
    const buyerPub = PublicKey.fromString(await mem.operatorSigner().pubKeyHex()).toString()
    const leaf = args.offer.commitment?.leaf ?? ''
    // the sig binds whatever buyerPub the seller offered to — a public
    // offer signed with '' verifies against '', a private one against the
    // operator's own pub. Accept either; the sig covers the choice.
    if (
      !verifyOfferSig(
        { cfg: args.cfg, sellerPub: args.offer.sellerPub, offerSig: args.offer.offerSig, expiresAt: args.offer.expiresAt },
        args.offer.buyerPub ?? buyerPub,
        leaf
      ) &&
      !verifyOfferSig({ cfg: args.cfg, sellerPub: args.offer.sellerPub, offerSig: args.offer.offerSig, expiresAt: args.offer.expiresAt }, '', leaf)
    )
      throw new Error('sale op=escrow: offerSig does not verify — cfg may have been substituted')
    // π_i pre-funding check: if the commitment carries a path, the leaf
    // must rebuild the committed batch root — catch a bogus leaf early.
    const proof = args.offer.commitment?.proof
    if (proof?.path && args.offer.commitment?.batchR) {
      const root = reconstructRoot(fh(leaf), { index: proof.leafIndex, siblings: proof.path.map((h: string) => fh(h)) })
      if (th(root) !== args.offer.commitment.batchR) throw new Error('sale op=escrow: leaf path does not reach the committed batch root')
    }
    // §19 horizon: refuse to fund an offer that has lapsed — a signed
    // offer is only good until expiresAt (0 = no expiry).
    const exp = BigInt(args.offer.expiresAt ?? 0)
    if (exp > 0n && BigInt(await mem.chainHeight()) >= exp) throw new Error(`sale op=escrow: offer expired at height ${exp}`)
  }
  // §19 sanity: the escrow must not be instantly refundable — refundAfter
  // is a locktime gate (height < 500M ⇒ block height, else timestamp).
  {
    const ra = BigInt(args.cfg.refundAfter)
    const future = ra < 500_000_000n ? ra > BigInt(await mem.chainHeight()) : ra > BigInt(Math.floor(Date.now() / 1000))
    if (!future) throw new Error('sale op=escrow: cfg.refundAfter is not in the future — the escrow would be instantly refundable')
  }
  // the depositor is the fund signer — held-key custody that's the operator
  // key; external custody the wallet-derived fund key. Either way refunds
  // must pay back to whoever controls the funding coins.
  const depositorPkh = Buffer.from(PublicKey.fromString(await mem.fundSigner().pubKeyHex()).toHash()).toString('hex')
  const host = new DisclosureSaleHost({
    ...args.cfg,
    depositorPkh,
    price: BigInt(args.cfg.price),
    refundAfter: BigInt(args.cfg.refundAfter),
    maxFee: BigInt(args.cfg.maxFee)
  })
  const funding = await mem.fundingOuts(args.amount + 500, 'deal')
  const { tx, utxo, feeSats } = await host.buildEscrow(funding, mem.fundSigner(), args.amount, undefined, mem.feeRateSatPerKb)
  if (isBuild) return { tx, txHex: tx.toHex(), utxo: { ...utxo, txid: tx.id('hex') }, escrow: { ...args.cfg, depositorPkh }, feeSats }
  const r = await mem.broadcastTx(tx)
  if (r.status !== 'success') return { accepted: false, reason: r.description }
  await settleEscrow(mem, tx)
  return { accepted: true, txid: tx.id('hex'), utxo: { ...utxo, txid: tx.id('hex') }, escrow: { ...args.cfg, depositorPkh } }
}

// same pair for the direct rail: sale#pay / sale#payBuild
const payBuild: ToolHandler = async (mem, args, _d, name) => {
  // the direct rail: an outright P2PKH payment to the quote's sellerPkh —
  // payment-final, so the same delegation-sig guard applies (a swapped
  // cfg would point the payment at a substituted key)
  const isBuild = name === 'sale#payBuild'
  if (args.offer) {
    const { verifyOfferSig } = await import('@nektar/identity')
    const buyerPub = PublicKey.fromString(await mem.operatorSigner().pubKeyHex()).toString()
    const leaf = args.offer.commitment?.leaf ?? ''
    if (
      !verifyOfferSig(
        { cfg: args.cfg, sellerPub: args.offer.sellerPub, offerSig: args.offer.offerSig, expiresAt: args.offer.expiresAt },
        args.offer.buyerPub ?? buyerPub,
        leaf
      ) &&
      !verifyOfferSig({ cfg: args.cfg, sellerPub: args.offer.sellerPub, offerSig: args.offer.offerSig, expiresAt: args.offer.expiresAt }, '', leaf)
    )
      throw new Error('sale op=pay: offerSig does not verify — cfg may have been substituted')
    const exp = BigInt(args.offer.expiresAt ?? 0)
    if (exp > 0n && BigInt(await mem.chainHeight()) >= exp) throw new Error(`sale op=pay: offer expired at height ${exp}`)
  }
  const pr = await mem.sendSats({ amount: args.amount, scriptHex: args.scriptHex, buildOnly: isBuild })
  if (isBuild) return { tx: pr.tx, txHex: pr.txHex, utxo: pr.utxo }
  return { accepted: true, txid: pr.txid, utxo: pr.utxo, txHex: pr.txHex }
}

export const handlers: Record<string, ToolHandler> = {
  sale: async (mem, args, d) => {
    const callTool = d.call
    const op = String(args?.op ?? '')
    if (!SALE_OPS.has(op)) throw new Error('sale: op required — offer|offerMemory|escrow|escrowBuild|complete|refund|claim')
    // F3 — escrow funding under a grant goes through the cumulative ledger;
    // the sale's price is the all-in debit reserved before funding
    const gid = typeof args?.grantId === 'string' ? args.grantId : undefined
    if (op === 'escrow' && gid && mem.budget) {
      const actionId = `sale:escrow:${args.cfg ? JSON.stringify(args.cfg).slice(0, 60) : Date.now()}`
      const amount = Number(args.cfg?.price ?? args.amount ?? 0)
      const r = mem.budget.reserve(gid, actionId, amount)
      if (r === 'exists') throw new Error(`sale: action already recorded for grant ${gid} � idempotent replay refused`)
      if (r !== 'ok') throw new Error(`sale: grant ${gid} budget ${r}`)
      try {
        const out = await callTool(mem, `sale#${op}`, args, d.ctx)
        mem.budget.commit(gid, actionId)
        return out
      } catch (e) {
        mem.budget.release(gid, actionId)
        throw e
      }
    }
    return await callTool(mem, `sale#${op}`, args, d.ctx)
  },
  'sale#offer': async (mem, args) => {
    const { offerDisclosure } = await import('@nektar/identity')
    const { COVENANT_ID } = await import('@nektar/anchor')
    const value = new Uint8Array(Buffer.from(args.value, 'hex'))
    const r = await offerDisclosure({
      sellerKey: mem.wrapCustody(),
      value,
      price: BigInt(args.price),
      refundAfter: BigInt(args.refundAfter),
      maxFee: BigInt(saleFeeBound(value.length, Number(args.maxFee))),
      buyerPub: args.buyerPub,
      expiresAt: args.expiresAt !== undefined ? BigInt(args.expiresAt) : undefined
    })
    // JSON-RPC cannot carry BigInt — cfg crosses the wire as plain numbers
    return {
      cfg: { ...r.cfg, price: Number(r.cfg.price), refundAfter: Number(r.cfg.refundAfter), maxFee: Number(r.cfg.maxFee) },
      wrapped: r.wrapped,
      sellerPub: r.sellerPub,
      expiresAt: Number(r.expiresAt ?? 0n),
      offerSig: r.offerSig,
      artifact: COVENANT_ID
    }
  },
  'sale#offerMemory': async (mem, args) => {
    const { offerDisclosure, encodeKeySale } = await import('@nektar/identity')
    const { COVENANT_ID } = await import('@nektar/anchor')
    const { serializeFragment, computeLeaf, toHex } = await import('@nektar/core')
    const rec = (await mem.records(args.shard)).find((r) => r.seq === args.seq)
    if (!rec) throw new Error(`no record seq ${args.seq} in shard "${args.shard}"`)
    // Paper f_i = {c_i, H(d_i), π_i}: an encrypted record is sold as its
    // FRAGMENT KEY (the §7 decryption capability) + ciphertext + provenance;
    // a plaintext record is sold as its canonical serialized fragment.
    let payload: Uint8Array
    let mode: 'key' | 'fragment'
    if (rec.fragment.E && rec.fragment.E.length && !rec.fragment.d.length) {
      payload = encodeKeySale(await mem.fragmentKeyFor(rec), rec.fragment.E, rec.fragment.p)
      mode = 'key'
    } else {
      payload = serializeFragment(rec.fragment)
      mode = 'fragment'
    }
    const leafHex = toHex(rec.leaf ?? computeLeaf(rec.fragment))
    // π_i — the record's inclusion proof — goes into the commitment so the
    // buyer verifies leaf∈anchored-batch BEFORE funding (ZKCP verify-before-pay).
    const { bundle } = await mem.prove(args.shard, args.seq)
    const r = await offerDisclosure({
      sellerKey: mem.wrapCustody(),
      value: payload,
      price: BigInt(args.price),
      refundAfter: BigInt(args.refundAfter),
      maxFee: BigInt(saleFeeBound(payload.length, Number(args.maxFee))),
      buyerPub: args.buyerPub,
      leaf: leafHex,
      expiresAt: args.expiresAt !== undefined ? BigInt(args.expiresAt) : undefined,
      direct: args.direct === true
    })
    return {
      cfg: { ...r.cfg, price: Number(r.cfg.price), refundAfter: Number(r.cfg.refundAfter), maxFee: Number(r.cfg.maxFee) },
      wrapped: r.wrapped,
      sellerPub: r.sellerPub,
      expiresAt: Number(r.expiresAt ?? 0n),
      offerSig: r.offerSig,
      artifact: COVENANT_ID,
      ...(r.direct === true ? { direct: true } : {}),
      mode,
      commitment: {
        leaf: leafHex,
        anchorTxid: rec.anchorTxid ?? null,
        hdp: toHex(rec.hdp),
        shard: args.shard,
        seq: args.seq,
        authorPub: rec.authorPub ?? null,
        level: rec.level ?? 0,
        batchR: bundle?.anchor.R ?? null,
        proof: bundle ? { path: bundle.path, leafIndex: bundle.leafIndex, n: bundle.n, size: bundle.size } : null
      }
    }
  },
  'sale#escrow': escrow,
  'sale#escrowBuild': escrow,
  'sale#pay': payBuild,
  'sale#payBuild': payBuild,
  'sale#complete': async (mem, args) => {
    const { DisclosureSaleHost } = await import('@nektar/anchor')
    // depositorPkh must come from the escrow cfg (the escrow op returns it as
    // cfg.depositorPkh; the market engine stores it from the accept message).
    // The offer cfg does NOT carry it, and the caller is the SELLER — the
    // old local-key fallback produced a covenant spend that always fails
    // script eval (change paid to the wrong pkh → outputHash mismatch).
    if (args.cfg.depositorPkh === undefined)
      return { accepted: false, reason: 'sale op=complete: cfg.depositorPkh missing — pass the escrow result cfg, not the bare offer cfg' }
    const host = new DisclosureSaleHost({
      ...args.cfg,
      depositorPkh: args.cfg.depositorPkh,
      price: BigInt(args.cfg.price),
      refundAfter: BigInt(args.cfg.refundAfter),
      maxFee: BigInt(args.cfg.maxFee)
    })
    if (args.utxo?.script !== undefined && !host.matchesScript(args.utxo.script))
      return { accepted: false, reason: 'sale op=complete: cfg does not match the escrow locking script — params disagree with the on-chain covenant' }
    // covenant bound: utxo − price − change ≤ maxFee → change ≥ utxo − price − maxFee.
    // Fee sized to the wrapped payload the unlocking script carries — a flat
    // ~350 overpays small spends and still starves big ones into
    // insufficient-fee; the bound itself stays the ceiling.
    const changeSats =
      args.changeSats !== undefined
        ? BigInt(args.changeSats)
        : BigInt(args.utxo.satoshis) - host.params.price - BigInt(Math.min(Number(host.params.maxFee) - 2, Math.max(10, relayNeed(args.wrapped.length / 2))))
    // the remembered escrow is the reveal's real parent — with it attached
    // the tx broadcasts as proper EF and the buyer receives BEEF/EF, not a
    // synthetic stub that cannot carry ancestry
    const { tx } = host.buildBuy(args.utxo, args.wrapped, changeSats, await mem.fetchTx(args.utxo.txid))
    const r = await mem.broadcastTx(tx)
    if (r.status !== 'success') return { accepted: false, reason: r.description }
    return { accepted: true, txid: tx.id('hex'), sellerSats: tx.outputs[0].satoshis, txHex: tx.toHex() }
  },
  'sale#refund': async (mem, args) => {
    const { DisclosureSaleHost } = await import('@nektar/anchor')
    const depositorPkh = args.cfg.depositorPkh ?? Buffer.from(PublicKey.fromString(await mem.fundSigner().pubKeyHex()).toHash()).toString('hex')
    const host = new DisclosureSaleHost({
      ...args.cfg,
      depositorPkh,
      price: BigInt(args.cfg.price),
      refundAfter: BigInt(args.cfg.refundAfter),
      maxFee: BigInt(args.cfg.maxFee)
    })
    if (args.utxo?.script !== undefined && !host.matchesScript(args.utxo.script))
      return { accepted: false, reason: 'sale op=refund: cfg does not match the escrow locking script — params disagree with the on-chain covenant' }
    // a refund carries no payload — ~2KB worst case; pay its real size, not
    // the covenant bound (which now scales with the payload it never had)
    const changeSats = args.changeSats !== undefined ? BigInt(args.changeSats) : BigInt(args.utxo.satoshis) - BigInt(Math.min(Number(host.params.maxFee), 10))
    const { tx } = await host.buildRefund(
      args.utxo,
      mem.fundSigner(),
      args.lockTime ?? Number(host.params.refundAfter),
      changeSats,
      await mem.fetchTx(args.utxo.txid)
    )
    const r = await mem.broadcastTx(tx)
    if (r.status !== 'success') return { accepted: false, reason: r.description }
    return { accepted: true, txid: tx.id('hex'), refundSats: tx.outputs[0].satoshis }
  },
  'sale#claim': async (mem, args) => {
    const { Transaction } = await import('@bsv/sdk')
    const { claimDisclosure, claimValue } = await import('@nektar/identity')
    const { fromHex: fh2 } = await import('@nektar/core')
    // the direct rail delivers `wrapped` inside the envelope — no covenant
    // carrier to parse; verification is the same sha256→hWrapped→unwrap path
    const r =
      args.valueHex !== undefined
        ? await claimValue(fh2(args.valueHex), { hWrapped: args.hWrapped, buyerKey: mem.wrapCustody(), sellerPub: args.sellerPub })
        : await claimDisclosure(Transaction.fromHex(args.txHex), { hWrapped: args.hWrapped, buyerKey: mem.wrapCustody(), sellerPub: args.sellerPub })
    const out: any = { mode: r.mode, valueHex: Buffer.from(r.value).toString('hex') }
    // memory-bound payload? verify the bought bytes hash to the anchored leaf
    if (args.commitment?.leaf) {
      const { parseFragment, computeLeaf, toHex, fromHex } = await import('@nektar/core')
      try {
        if (args.offerMode === 'key') {
          // key-sale: payload carries the fragment key — decrypt E, rebuild
          // the fragment, then the same leaf check proves inclusion.
          const { claimKeySale } = await import('@nektar/identity')
          const { verifyProvenance } = await import('@nektar/log')
          const { d, p, E } = claimKeySale(r.value)
          const frag = { d, p, hdp: args.commitment.hdp ? fromHex(args.commitment.hdp) : undefined, E }
          out.verified = toHex(computeLeaf(frag)) === args.commitment.leaf
          if (out.verified && args.commitment.authorPub && p.signature.length) {
            out.authorVerified = verifyProvenance(args.commitment.authorPub, d, p)
          }
          out.valueHex = Buffer.from(d).toString('hex')
          out.decrypted = true
        } else {
          const frag = parseFragment(r.value)
          out.verified = toHex(computeLeaf(frag)) === args.commitment.leaf
          // §16/19: a bought memory must carry verifiable authorship —
          // check the provenance σ against the recorded author key.
          if (out.verified && args.commitment.authorPub && frag.p.signature.length) {
            const { verifyProvenance } = await import('@nektar/log')
            out.authorVerified = verifyProvenance(args.commitment.authorPub, frag.d, frag.p)
          }
        }
        if (out.verified && args.commitment.anchorTxid) out.anchorTxid = args.commitment.anchorTxid
        if (!out.verified) out.reason = 'computeLeaf(payload) != committed leaf'
      } catch (e) {
        out.verified = false
        out.reason = `payload verification failed: ${(e as Error).message}`
      }
    }
    return out
  }
}

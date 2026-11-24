// BRC-166-style P2PKH payment settlement for paid tools (x402 `exact` scheme
// profile): the caller signs a whole raw tx paying the operator address AND
// carrying an OP_RETURN invoice tag, hands it over, we verify + broadcast.
// No wallet ecosystem — anything that can sign a BSV tx can pay (a WIF is
// enough). Settlement is publicly auditable from the tx alone: (pays payTo ≥
// amount) ∧ (carries `nektarpay:<invoiceId>`) — the receipt commits the txid.
import { Transaction, PrivateKey, PublicKey, Signature, P2PKH } from '@bsv/sdk'
import { LedgerReceiver, spentOutpoints } from '@lightwebinc/bcommon/host'
import { randomBytes } from 'node:crypto'
import { existsSync, readFileSync, renameSync } from 'node:fs'
import { join } from 'node:path'
import { utf8, sha256, toHex, signerAddress, signerPubHex, toSigner, nektarEnv, nektarEnvNum, fullSourceTx } from '@nektar/core'
import { isHeaderUnavailable } from '@nektar/chain'
import type { NektarMemory } from './memory.js'

export const paymentsEnabled = (): boolean => /^(1|true|yes)$/i.test(nektarEnv('NEKTAR_PAYMENTS') ?? '')

/** tools that cost sats when NEKTAR_PAYMENTS=1 */
export const PAID_TOOLS = new Set([
  'anchor',
  'groundedAnswer',
  'commitReceipt',
  'proveMembership',
  'proveSubset',
  'proveDerivation',
  'attestMembership',
  'attestInference'
])
/** prover-side tools: real CPU (STARK ~35-50s) and sometimes a chain tx — priced above answers. The verify-side tools stay free: a verifier never pays to check. */
const PROVER_TOOLS = new Set(['proveMembership', 'proveSubset', 'proveDerivation', 'attestMembership', 'attestInference'])

const networkTag = (): string => {
  const n = nektarEnv('NEKTAR_NETWORK') ?? 'regtest'
  return n === 'main' ? 'bsv:mainnet' : n === 'testnet' ? 'bsv:testnet' : n === 'regtest' ? 'bsv:regtest' : 'bsv:regtest'
}

/** the operator's settlement key when NEKTAR_OPERATOR_WIF is set — its
 *  ADDRESS receives payments and it signs attributable quotes. Absent under
 *  external-wallet custody (NEKTAR_WALLET=client): the instance's operator
 *  signer does both (payments settle to the wallet's identity address). */
let opKey: PrivateKey | undefined
let opAddr: string | undefined
const opWif = (): PrivateKey | undefined => {
  const wif = nektarEnv('NEKTAR_OPERATOR_WIF')
  if (opKey === undefined && wif !== undefined) {
    opKey = PrivateKey.fromWif(wif)
    opAddr = opKey.toPublicKey().toAddress(nektarEnv('NEKTAR_NETWORK') === 'main' ? [0x00] : [0x6f])
  }
  return opKey
}

/** settle address + quote signer: the operator WIF when set (legacy custody),
 *  else the instance's fund signer (derived `nektar-fund` under client custody,
 *  which CAN root-sign spends — `identityKey` cannot on sdk-4). */
const operatorAddress = async (mem: NektarMemory): Promise<string> =>
  opWif() !== undefined ? opAddr! : await signerAddress(mem.fundSigner(), nektarEnv('NEKTAR_NETWORK') === 'main' ? [0x00] : [0x6f])

/** startup check: paid tools require a settle destination BEFORE the first
 *  invoice is served — either the operator WIF (legacy) or NEKTAR_WALLET=client
 *  (the wallet's identity address; quoteSigs sign through the wallet). */
export const requireOperatorWallet = (): void => {
  if (!nektarEnv('NEKTAR_OPERATOR_WIF') && nektarEnv('NEKTAR_WALLET') !== 'client')
    throw new Error(
      'NEKTAR_PAYMENTS=1 requires NEKTAR_OPERATOR_WIF or NEKTAR_WALLET=client at startup — without a settle destination an invoice can never be paid'
    )
  if (nektarEnv('NEKTAR_OPERATOR_WIF'))
    try {
      opWif()
    } catch (e) {
      throw new Error(`NEKTAR_OPERATOR_WIF is not a valid WIF — ${(e as Error).message}`, { cause: e })
    }
}

/** per-tool price: NEKTAR_PRICE_<TOOL>_SATS, falling back to NEKTAR_PRICE_SATS, then defaults */
export const priceFor = (tool: string): number =>
  Number(
    nektarEnv(`NEKTAR_PRICE_${tool.toUpperCase()}_SATS`) ?? nektarEnv('NEKTAR_PRICE_SATS') ?? (tool === 'anchor' ? 100 : PROVER_TOOLS.has(tool) ? 500 : 250)
  )

/** walk the tx's carried ancestry after TxStore.read verified/stripped BUMPs:
 *  unproven ancestry may run at most `NEKTAR_PAY_ANCESTRY` hops (default 4)
 *  and at most `NEKTAR_PAY_ANCESTRY_NODES` total parents (default 64) —
 *  absent parents don't count (the broadcaster validates them), a carried-but-
 *  unproven one does, and any surviving merklePath already verified against
 *  our tracker inside TxStore.read. */
const carriedAncestryOk = (tx: Transaction): boolean => {
  const maxHops = Math.max(0, nektarEnvNum('NEKTAR_PAY_ANCESTRY') ?? 4)
  const maxNodes = Math.max(1, nektarEnvNum('NEKTAR_PAY_ANCESTRY_NODES') ?? 64)
  const requireCarried = nektarEnv('NEKTAR_NETWORK') === 'main' || /^(1|true|yes)$/i.test(nektarEnv('NEKTAR_PAY_REQUIRE_BEEF') ?? '')
  if (tx.merklePath === undefined && requireCarried && tx.inputs.some((i) => fullSourceTx(i) === undefined)) return false
  const seen = new Set([tx.id('hex')])
  let frontier = tx.inputs.map(fullSourceTx).filter((p): p is Transaction => p !== undefined)
  for (let hops = 0; frontier.length > 0; hops++) {
    if (hops >= maxHops) return false
    const next: Transaction[] = []
    for (const p of frontier) {
      const id = p.id('hex')
      if (seen.has(id)) continue
      seen.add(id)
      if (seen.size > maxNodes) return false
      if (p.merklePath !== undefined) continue // verified by TxStore.read — terminates branch
      if (p.inputs.length === 0) return false
      for (const i of p.inputs) {
        const src = fullSourceTx(i)
        if (src !== undefined) next.push(src)
        else if (requireCarried) return false
      }
    }
    frontier = next
  }
  return true
}

interface Invoice {
  invoiceId: string
  tool: string
  amount: number
  payTo: string
  lockingScriptHex: string
  /** ascii the payment tx must carry in an OP_RETURN output — binds the tx to THIS call */
  opReturn: string
  expires: number
  /** operator pubkey + signature over the canonical quote — the invoice is an
   *  attributable commitment: a buyer can hold the seller to what it quoted
   *  (and compare competing signed quotes) instead of trusting a 402 blob. */
  pub: string
  quoteSig: string
}

const invoices = new Map<string, Invoice>()
const INVOICE_TTL_MS = 10 * 60_000
// expiry is lazy (evaluated per settle attempt) — minted-but-never-settled
// invoices would otherwise pin the map for the process lifetime. Sweep on
// mint and hard-cap size.
const INVOICE_MAX = 4096
const sweepInvoices = () => {
  const now = Date.now()
  for (const [id, inv] of invoices) if (inv.expires <= now) invoices.delete(id)
  while (invoices.size > INVOICE_MAX) {
    const oldest = invoices.keys().next().value
    if (oldest === undefined) break
    invoices.delete(oldest)
  }
}

/** canonical string the quote signature covers — anything a cheaper-signed
 *  quote could be doctored on is inside the digest */
const quoteCanonical = (q: { invoiceId: string; tool: string; amount: number; payTo: string; expires: number }): string =>
  `${q.invoiceId}|${q.tool}|${q.amount}|${q.payTo}|${q.expires}|${networkTag()}`

/** verify a signed quote from a 402 accepts[].extra (or a payQuote invoice) */
export const verifyQuoteSig = (q: {
  invoiceId?: string
  tool?: string
  amount?: number | string
  payTo?: string
  expires?: number | string
  expiresMs?: number
  pub?: string
  quoteSig?: string
}): boolean => {
  try {
    if (typeof q.pub !== 'string' || typeof q.quoteSig !== 'string') return false
    const expires = typeof q.expiresMs === 'number' ? q.expiresMs : typeof q.expires === 'number' ? q.expires : Date.parse(String(q.expires))
    if (!Number.isSafeInteger(expires)) return false
    const canon = quoteCanonical({ invoiceId: String(q.invoiceId), tool: String(q.tool), amount: Number(q.amount), payTo: String(q.payTo), expires })
    return PublicKey.fromString(q.pub).verify(Array.from(sha256(utf8(canon))), Signature.fromDER(q.quoteSig, 'hex'))
  } catch {
    return false
  }
}

/** consumed payments — the stock payee engine (LedgerReceiver): payments.jsonl
 *  in the data dir, one stock ledger line each, claimed atomically — a txid
 *  replays as 'replayed', a second tx spending a coin an accepted payment
 *  spent refuses as 'conflict'. The pre-ledger payments.json txid list is
 *  migrated into the ledger at first use. */
const payDir = (): string => nektarEnv('NEKTAR_DATA_DIR') ?? join(process.cwd(), '.nektar')
const legacyPayFile = (): string => join(payDir(), 'payments.json')
let receiver: LedgerReceiver | undefined
let legacyMigrated = false
const getReceiver = (): LedgerReceiver => {
  receiver ??= new LedgerReceiver(payDir())
  if (!legacyMigrated) {
    legacyMigrated = true
    const legacyFile = legacyPayFile()
    try {
      if (existsSync(legacyFile)) {
        for (const t of (JSON.parse(readFileSync(legacyFile, 'utf8')).consumed ?? []) as string[])
          receiver.claim({
            txid: String(t),
            beef: [],
            outputIndex: 0,
            satoshis: 0,
            derivationPrefix: '',
            derivationSuffix: '',
            senderIdentityKey: '',
            class: 'x402',
            inputs: [],
            decision: 'fast'
          })
        try {
          renameSync(legacyFile, `${legacyFile}.migrated`)
        } catch {
          /* rename failed — migration is idempotent, next boot retries */
        }
      }
    } catch {
      /* legacy file unreadable — txid-only protection survives in memory */
    }
  }
  return receiver
}

/** issue an invoice for a paid tool call — signed so the quote travels as an
 *  attributable offer (a doctored amount/payTo fails quoteSig verification) */
const issueInvoice = async (mem: NektarMemory, tool: string, amount?: number): Promise<Invoice> => {
  sweepInvoices()
  const signer = toSigner(opWif() ?? mem.fundSigner())
  const payTo = await operatorAddress(mem)
  const inv: Invoice = {
    invoiceId: randomBytes(8).toString('hex'),
    tool,
    amount: amount ?? priceFor(tool),
    payTo,
    lockingScriptHex: new P2PKH().lock(payTo).toHex(),
    opReturn: '',
    expires: Date.now() + INVOICE_TTL_MS,
    pub: await signerPubHex(signer),
    quoteSig: ''
  }
  inv.opReturn = `nektarpay:${inv.invoiceId}`
  inv.quoteSig = toHex(Uint8Array.from((await signer.sign(Array.from(sha256(utf8(quoteCanonical(inv)))))).toDER() as number[]))
  invoices.set(inv.invoiceId, inv)
  return inv
}

/** the 402 payload — x402 v2 PaymentRequired shape carrying the invoice in accepts[].extra */
export const paymentRequired = async (mem: NektarMemory, tool: string, amount?: number): Promise<{ invoice: Invoice; required: Record<string, unknown> }> => {
  const invoice = await issueInvoice(mem, tool, amount)
  const required = {
    x402Version: 2,
    error: `payment required — ${tool} costs ${invoice.amount} sats`,
    resource: { url: `tool://${tool}`, description: `nektar ${tool}` },
    accepts: [
      {
        scheme: 'exact',
        network: networkTag(),
        amount: String(invoice.amount),
        payTo: invoice.payTo,
        extra: {
          invoiceId: invoice.invoiceId,
          payTo: invoice.payTo,
          lockingScriptHex: invoice.lockingScriptHex,
          opReturn: invoice.opReturn,
          expires: new Date(invoice.expires).toISOString(),
          expiresMs: invoice.expires,
          tool,
          pub: invoice.pub,
          quoteSig: invoice.quoteSig,
          howToPay:
            'build + sign a tx with (1) an output paying lockingScriptHex (or payTo) ≥ amount sats and (2) an OP_FALSE OP_RETURN output carrying the ascii `opReturn`, then retry the SAME call with payment:{invoiceId, transaction:<raw or BEEF tx hex>} (BEEF preferred — it carries the funding parents)'
        }
      }
    ]
  }
  return { invoice, required }
}

export interface PaymentRecord {
  txid: string
  amountSats: number
  payTo: string
  invoiceId: string
}

/** verify + broadcast the caller's payment tx; returns the settlement record a receipt commits */
export const settlePayment = async (mem: NektarMemory, tool: string, payment: { invoiceId?: string; transaction?: string }): Promise<PaymentRecord> => {
  const inv = invoices.get(String(payment.invoiceId ?? ''))
  if (!inv || inv.tool !== tool) throw new Error(`unknown or wrong-tool invoiceId "${payment.invoiceId}" — call payQuote for a fresh invoice`)
  if (Date.now() > inv.expires) {
    invoices.delete(inv.invoiceId)
    throw new Error('invoice expired — call payQuote for a fresh one')
  }
  const recv = getReceiver()
  // raw hex, EF, or BEEF (v1/v2/Atomic): TxStore.read parses all wire forms,
  // verifies carried BUMPs against our tracker (rethrowing HeaderUnavailableError),
  // and counts any unverified/forged BUMPs it stripped.
  let tx: Transaction
  let strippedBumps = 0
  try {
    const h = String(payment.transaction ?? '').replace(/^0x/, '')
    if (h.length === 0) throw new Error('empty transaction')
    const ingested = await mem.readTxWire(h)
    tx = ingested.tx
    strippedBumps = ingested.strippedBumps ?? 0
  } catch (e) {
    if (isHeaderUnavailable(e)) throw e
    throw new Error(`payment.transaction is not a parseable raw/BEEF tx (${(e as Error).message.slice(0, 80)})`, { cause: e })
  }
  const tagHex = Buffer.from(utf8(inv.opReturn)).toString('hex')
  const paid = tx.outputs.some((o) => o.lockingScript.toHex() === inv.lockingScriptHex && (o.satoshis ?? 0) >= inv.amount)
  const tagged = tx.outputs.some((o) => {
    const h = o.lockingScript.toHex()
    return h.startsWith('006a') && h.includes(tagHex)
  })
  if (!paid) throw new Error(`payment tx does not pay ${inv.payTo} ≥ ${inv.amount} sats`)
  if (!tagged) throw new Error(`payment tx is missing the invoice tag output — needs OP_FALSE OP_RETURN "${inv.opReturn}"`)
  // counterparty-carried ancestry: every BUMP in the bundle must verify against
  // OUR tracker (strippedBumps === 0), and the unproven chain it hangs off is
  // bounded — a carried parent is our risk surface (double-spend room grows
  // with unconfirmed depth); an absent one is the network's — broadcast rejects it
  if (strippedBumps > 0 || !carriedAncestryOk(tx))
    throw new Error('payment ancestry fails verification or exceeds the unproven-depth cap — provide proven parents')
  const maxZeroConf = nektarEnvNum('NEKTAR_PAY_MAX_ZEROCONF_SATS')
  if (tx.merklePath === undefined && maxZeroConf !== undefined && inv.amount > maxZeroConf)
    throw new Error(`payment exceeds zero-conf cap (${inv.amount} > ${maxZeroConf} sats) — provide a mined payment tx with BUMP`)
  const txid = tx.id('hex')
  // stock claim check BEFORE broadcast: replayed txid, or a conflict with a
  // coin an accepted payment already spent — two verifying txs sharing a coin
  // can never both settle
  const inputs = spentOutpoints(tx)
  const claim = recv.refusal({ txid, inputs })
  if (claim === 'replayed') throw new Error(`payment txid ${txid.slice(0, 16)}… already consumed — one tx pays for one call`)
  if (claim === 'conflict') throw new Error(`payment tx spends a coin an accepted payment already spent — conflict, only one can settle`)
  // serialize BEFORE broadcast so a missing sourceTransaction on a raw tx can
  // never throw after the tx already aired and leave the payment unclaimed
  const beef = tx.inputs.every((i) => i.sourceTransaction !== undefined)
    ? Array.from(Uint8Array.from(tx.toAtomicBEEF()))
    : Array.from(Uint8Array.from(tx.toBinary()))
  const r = await mem.broadcastTx(tx)
  if (r.status !== 'success') throw new Error(`payment broadcast failed: ${r.description}`)
  recv.claim({
    txid,
    beef,
    outputIndex: 0,
    satoshis: inv.amount,
    derivationPrefix: '',
    derivationSuffix: '',
    senderIdentityKey: '',
    class: 'x402',
    inputs,
    decision: 'fast'
  })
  // when payments settle directly to our fund address (no separate operator WIF),
  // adopt the received output(s) into the coin book so they are spendable
  if (opWif() === undefined) {
    for (const [vout, o] of tx.outputs.entries()) {
      if (o.lockingScript.toHex() === inv.lockingScriptHex) void mem.funds.adopt(tx, vout)
    }
  }
  invoices.delete(inv.invoiceId)
  return { txid, amountSats: inv.amount, payTo: inv.payTo, invoiceId: inv.invoiceId }
}

export const _testReset = (): void => {
  invoices.clear()
  receiver?.close()
  receiver = undefined
  legacyMigrated = false
  opAddr = undefined
  opKey = undefined
}

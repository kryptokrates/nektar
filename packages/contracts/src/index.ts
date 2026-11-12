// @nektar/contracts — Rúnar NektarAnchor artifact + state codec.
// The compiled artifact is committed (packages/contracts/NektarAnchor.json);
// `pnpm compile:contract` regenerates it and must produce identical bytes
// (covered by the reproduce gate).
//
// ANC-006 / GOAL-v6 S6.2 (Metanet flag): v3 anchors gain an OPTIONAL leading
// flag — the state payload's first push may be 0x4D455441 ("META"). New
// deployments emit it (metaFlag state field); flag-absent anchors produced by
// the pre-flag artifact (NektarAnchorV3.json) stay decodable byte-for-byte.
import { createRequire } from 'node:module'
import type { AnchorData, Bytes } from '@nektar/core'
import { sha256, utf8, toHex, fromHex, chainRp, TAG_RPRIME as TAG_RPRIME_BYTES, hash160 } from '@nektar/core'

const require = createRequire(import.meta.url)
export const NEKTAR_ANCHOR_ARTIFACT = require('../../NektarAnchor.json')
/** the pre-flag (v3, flag-absent) artifact — kept so every anchor ever
 *  deployed stays decodable and spendable */
export const NEKTAR_ANCHOR_V3_ARTIFACT = require('../../NektarAnchorV3.json')
export const NEKTAR_AUTOMATON_ARTIFACT = require('../../NektarAutomaton.json')
/** GOAL-v7 V1: covenant spend guard — caps as consensus rules. */
export const NEKTAR_FEEVAULT_ARTIFACT = require('../../NektarFeeVault.json')
/** GOAL-v7 V2: unforkable sequential checkpoint chain + time-locked bond. */
export const NEKTAR_CHECKPOINTBOND_ARTIFACT = require('../../NektarCheckpointBond.json')
/** GOAL-v7 V3: atomic payment-for-disclosure escrow. */
export const DISCLOSURESALE_ARTIFACT = require('../../DisclosureSale.json')

/** ANC-006 Metanet flag: the first push of the anchor output's data payload */
export { META_FLAG } from '@nektar/core'
export const META_FLAG_HEX = '4d455441' // toHex(META_FLAG)
/** wire form: a 4-byte push — <0x04> 'META' — leading the OP_RETURN state */
const META_PUSH_HEX = `04${META_FLAG_HEX}`

// 'nektar-rprime' — same domain tag the contract pushes into R' computations.
export { TAG_RPRIME } from '@nektar/core'

/** R' computation identical to the contract's hash256(tag‖Rp‖R‖size_le8). */
// Contract R' computation: contract's hash256(tag‖Rp‖R‖size_le8) ==
// core chainRp byte-for-byte (proved by the vectors test).
export const contractRp = chainRp

export interface DecodedAnchor extends AnchorData {
  ownerPub: string
  ownerPkh: Bytes
  tag: Bytes
  /** ANC-006: the data payload's first push is the META flag */ metanet: boolean
}

/** tag must equal core TAG_RPRIME (the deployer chooses it; readers must check). */
export function isValidAnchor(d: Pick<DecodedAnchor, 'tag'>): boolean {
  return d.tag.length === TAG_RPRIME_BYTES.length && d.tag.every((b, i) => b === TAG_RPRIME_BYTES[i])
}

/**
 * Decode a NektarAnchor v3 locking script. Authenticated byte-for-byte: parse
 * the state suffix after OP_RETURN ([flag?] owner|n|size|R|Rp|len|tag|len|pkh),
 * then rebuild the EXPECTED script via runar — flagged against the current
 * artifact, flag-absent against the pre-flag artifact — and require exact
 * equality. Anything else (including v2 anchors with a poseidonRoot field)
 * returns null.
 */
export function decodeNektarAnchor(scriptHex: string): DecodedAnchor | null {
  const hex = scriptHex.startsWith('0x') ? scriptHex.slice(2) : scriptHex
  // 1. state suffix: the last OP_RETURN whose suffix parses and ends the
  // script — layout: [flag(4B)?] genesisOwner(33B)|n(8)|size(8)|R(32)|Rp(32)|tagLen|tag|pkhLen|pkh
  let state: string | null = null
  let metanet = false
  for (let p = hex.length - 2; p >= 0; p -= 2) {
    if (hex.slice(p, p + 2) !== '6a') continue
    let cand = hex.slice(p + 2)
    const flagged = cand.startsWith(META_PUSH_HEX)
    if (flagged) cand = cand.slice(META_PUSH_HEX.length)
    if (cand.length < 230) continue
    if (!/^0[23]/.test(cand.slice(0, 66))) continue
    const tagLen = parseInt(cand.slice(226, 228), 16)
    if (!isFinite(tagLen)) continue
    const pkhOff = 228 + tagLen * 2
    const pkhLen = parseInt(cand.slice(pkhOff, pkhOff + 2), 16)
    if (!isFinite(pkhLen) || pkhOff + 2 + pkhLen * 2 !== cand.length) continue
    state = cand
    metanet = flagged
    break
  }
  if (state === null) return null
  const ownerPub = state.slice(0, 66)
  const n = Number(readLe8(state.slice(66, 82)))
  const size = Number(readLe8(state.slice(82, 98)))
  const R = fromHex(state.slice(98, 162))
  const Rp = fromHex(state.slice(162, 226))
  const tagLen = parseInt(state.slice(226, 228), 16)
  const tag = fromHex(state.slice(228, 228 + tagLen * 2))
  const pkhOff = 228 + tagLen * 2
  const pkhLen = parseInt(state.slice(pkhOff, pkhOff + 2), 16)
  const ownerPkh = fromHex(state.slice(pkhOff + 2, pkhOff + 2 + pkhLen * 2))
  // 2. byte-auth: rebuild the expected script (flagged → current artifact,
  // flag-absent → pre-flag artifact) and require exact equality
  try {
    const expected = buildScript(
      ownerPub,
      toHex(ownerPkh),
      BigInt(n),
      BigInt(size),
      state.slice(98, 162),
      state.slice(162, 226),
      state.slice(228, 228 + tagLen * 2),
      metanet
    )
    if (expected !== hex) return null
  } catch {
    return null
  }
  return { shard: toHex(hash160(fromHex(ownerPub))), n, size, R, Rp, tag, ownerPub, ownerPkh, metanet }
}

/** Appendix D σ_n: the anchor-authorship signature as a discrete artifact.
 *  The covenant enforces `checkSig(sig, ownerPub)` inside `append`/`rotate` —
 *  the sig is over sha256(txPreimage), and the preimage commits to the entire
 *  spend (including the continuation output carrying R_n, R'_n, n+1, t_n's
 *  funding), so `Sign_owner(preimage)` ≡ the paper's `Sign(R_n‖t_n)`: the new
 *  state cannot exist without this signature, and it is now extractable from
 *  the spend tx alone — no covenant internals required. */
export interface AnchorAuthorship {
  ownerPub: string // the owner key that authorized this state transition
  sig: string // DER + sighash-scope byte, as pushed by the spend
  sighashScope: number // trailing byte of sig (0x41 = ALL|FORKID)
  preimage: string // the txPreimage chunk the signature commits to
  inputIndex: number // which input spent the anchor UTXO
  verified: boolean // sig verifies over sha256(preimage) for ownerPub
}
/** Extract + verify σ_n from an anchor spend. `spentAnchorScript` is the
 *  locking script of the anchor UTXO being spent (decoded to recover ownerPkh
 *  and prove the input really spends a NektarAnchor). Returns null when the
 *  input doesn't carry a recognizable owner sig/pubKey pair. */
export function extractAnchorAuthorship(
  spendTx: { inputs: Array<{ unlockingScript?: { chunks: Array<{ op: number; data?: number[] | Uint8Array }> } }> },
  spentAnchorScript: string,
  inputIndex = 0
): AnchorAuthorship | null {
  const dec = decodeNektarAnchor(spentAnchorScript)
  if (!dec) return null
  const chunks = spendTx.inputs[inputIndex]?.unlockingScript?.chunks
  if (!chunks || chunks.length < 3) return null
  const push = (i: number): Uint8Array | null => {
    const d = chunks[i]?.data
    return d ? Uint8Array.from(d) : null
  }
  // txPreimage = the last data push before the trailing method-index chunk
  const pre = push(chunks.length - 2)
  if (!pre || pre.length < 100) return null
  const digest = sha256(pre)
  const pkhHex = toHex(dec.ownerPkh)
  for (let i = 1; i < chunks.length; i++) {
    const d = push(i)
    if (!d || d.length !== 33 || (d[0] !== 2 && d[0] !== 3)) continue
    if (toHex(hash160(d)) !== pkhHex) continue
    const sigD = push(i - 1)
    if (!sigD || sigD.length < 70 || sigD.length > 73 || sigD[0] !== 0x30) return null
    const scope = sigD[sigD.length - 1]
    const der = toHex(sigD.slice(0, -1))
    let verified = false
    try {
      // dynamic require (peer dep): sha256+secp256k1 verify via @bsv/sdk
      const { PublicKey, Signature } = require('@bsv/sdk')
      verified = PublicKey.fromString(toHex(d)).verify(Array.from(digest), Signature.fromDER(der, 'hex'))
    } catch {
      verified = false
    }
    return { ownerPub: toHex(d), sig: toHex(sigD), sighashScope: scope, preimage: toHex(pre), inputIndex, verified }
  }
  return null
}

let RC: any = null
function buildScript(pub: string, pkh: string, n: bigint, size: bigint, R: string, Rp: string, tag: string, metanet = true): string {
  if (!RC) RC = require('runar-sdk')
  const args = metanet ? [pub, pkh, META_FLAG_HEX, n, size, R, Rp, tag] : [pub, pkh, n, size, R, Rp, tag]
  const c = new RC.RunarContract(metanet ? NEKTAR_ANCHOR_ARTIFACT : NEKTAR_ANCHOR_V3_ARTIFACT, args)
  return c.getLockingScript()
}
function readLe8(hex8bytes: string): bigint {
  let v = 0n
  for (let i = 7; i >= 0; i--) v = (v << 8n) | BigInt(parseInt(hex8bytes.slice(i * 2, i * 2 + 2), 16))
  return v
}

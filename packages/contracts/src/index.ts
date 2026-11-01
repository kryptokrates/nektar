// @mamem/contracts — Rúnar MamemAnchor artifact + state codec.
// The compiled artifact is committed (packages/contracts/MamemAnchor.json);
// `pnpm compile:contract` regenerates it and must produce identical bytes
// (covered by the reproduce gate).
//
// ANC-006 / GOAL-v6 S6.2 (Metanet flag): v3 anchors gain an OPTIONAL leading
// flag — the state payload's first push may be 0x4D455441 ("META"). New
// deployments emit it (metaFlag state field); flag-absent anchors produced by
// the pre-flag artifact (MamemAnchorV3.json) stay decodable byte-for-byte.
import { createRequire } from 'node:module'
import type { AnchorData, Bytes } from '@mamem/core'
import { sha256, dsha256, utf8, toHex, fromHex, chainRp, TAG_RPRIME as TAG_RPRIME_BYTES, hash160, META_FLAG } from '@mamem/core'

const require = createRequire(import.meta.url)
export const MAMEM_ANCHOR_ARTIFACT = require('../../MamemAnchor.json')
/** the pre-flag (v3, flag-absent) artifact — kept so every anchor ever
 *  deployed stays decodable and spendable */
export const MAMEM_ANCHOR_V3_ARTIFACT = require('../../MamemAnchorV3.json')
export const MAMEM_AUTOMATON_ARTIFACT = require('../../MamemAutomaton.json')

/** ANC-006 Metanet flag: the first push of the anchor output's data payload */
export { META_FLAG } from '@mamem/core'
export const META_FLAG_HEX = '4d455441' // toHex(META_FLAG)
/** wire form: a 4-byte push — <0x04> 'META' — leading the OP_RETURN state */
const META_PUSH_HEX = `04${META_FLAG_HEX}`

// 'mamem-rprime' — same domain tag the contract pushes into R' computations.
export { TAG_RPRIME } from '@mamem/core'

/** R' computation identical to the contract's hash256(tag‖Rp‖R‖size_le8). */
// Contract R' computation: contract's hash256(tag‖Rp‖R‖size_le8) ==
// core chainRp byte-for-byte (proved by the vectors test).
export const contractRp = chainRp

export interface DecodedAnchor extends AnchorData { ownerPub: string, ownerPkh: Bytes, tag: Bytes, /** ANC-006: the data payload's first push is the META flag */ metanet: boolean }

/** tag must equal core TAG_RPRIME (the deployer chooses it; readers must check). */
export function isValidAnchor (d: Pick<DecodedAnchor,'tag'>): boolean {
    return d.tag.length === TAG_RPRIME_BYTES.length && d.tag.every((b,i)=>b===TAG_RPRIME_BYTES[i])
}

/**
 * Decode a MamemAnchor v3 locking script. Authenticated byte-for-byte: parse
 * the state suffix after OP_RETURN ([flag?] owner|n|size|R|Rp|len|tag|len|pkh),
 * then rebuild the EXPECTED script via runar — flagged against the current
 * artifact, flag-absent against the pre-flag artifact — and require exact
 * equality. Anything else (including v2 anchors with a poseidonRoot field)
 * returns null.
 */
export function decodeMamemAnchor (scriptHex: string): DecodedAnchor | null {
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
    const expected = buildScript(ownerPub, toHex(ownerPkh), BigInt(n), BigInt(size), state.slice(98, 162), state.slice(162, 226), state.slice(228, 228 + tagLen * 2), metanet)
    if (expected !== hex) return null
  } catch { return null }
  return { shard: toHex(hash160(fromHex(ownerPub))), n, size, R, Rp, tag, ownerPub, ownerPkh, metanet }
}

let RC: any = null
function buildScript (pub: string, pkh: string, n: bigint, size: bigint, R: string, Rp: string, tag: string, metanet = true): string {
  if (!RC) RC = require('runar-sdk')
  const args = metanet ? [pub, pkh, META_FLAG_HEX, n, size, R, Rp, tag] : [pub, pkh, n, size, R, Rp, tag]
  const c = new RC.RunarContract(metanet ? MAMEM_ANCHOR_ARTIFACT : MAMEM_ANCHOR_V3_ARTIFACT, args)
  return c.getLockingScript()
}
function readLe8 (hex8bytes: string): bigint {
  let v = 0n
  for (let i = 7; i >= 0; i--) v = (v << 8n) | BigInt(parseInt(hex8bytes.slice(i * 2, i * 2 + 2), 16))
  return v
}

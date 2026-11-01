// @mamem/contracts — Rúnar MamemAnchor artifact + state codec.
// The compiled artifact is committed (packages/contracts/MamemAnchor.json);
// `pnpm compile:contract` regenerates it and must produce identical bytes
// (covered by the reproduce gate).
import { createRequire } from 'node:module'
import type { AnchorData, Bytes } from '@mamem/core'
import { sha256, dsha256, utf8, toHex, fromHex, chainRp, TAG_RPRIME as TAG_RPRIME_BYTES, hash160 } from '@mamem/core'

const require = createRequire(import.meta.url)
export const MAMEM_ANCHOR_ARTIFACT = require('../../MamemAnchor.json')
export const MAMEM_AUTOMATON_ARTIFACT = require('../../MamemAutomaton.json')

// 'mamem-rprime' — same domain tag the contract pushes into R' computations.
export { TAG_RPRIME } from '@mamem/core'

/** R' computation identical to the contract's hash256(tag‖Rp‖R‖size_le8). */
// Contract R' computation: contract's hash256(tag‖Rp‖R‖size_le8) ==
// core chainRp byte-for-byte (proved by the vectors test).
export const contractRp = chainRp

export interface DecodedAnchor extends AnchorData { ownerPub: string, ownerPkh: Bytes, tag: Bytes }

/** tag must equal core TAG_RPRIME (the deployer chooses it; readers must check). */
export function isValidAnchor (d: Pick<DecodedAnchor,'tag'>): boolean {
    return d.tag.length === TAG_RPRIME_BYTES.length && d.tag.every((b,i)=>b===TAG_RPRIME_BYTES[i])
}

/**
 * Decode a MamemAnchor locking script. Authenticated byte-for-byte: parse
 * the state suffix after OP_RETURN (n|size|R|Rp|poseidon|len|tag|len|pkh),
 * then rebuild the EXPECTED script via runar for every pubkey candidate in
 * the code and require exact equality. Anything else returns null.
 */
export function decodeMamemAnchor (scriptHex: string): DecodedAnchor | null {
  const hex = scriptHex.startsWith('0x') ? scriptHex.slice(2) : scriptHex
  // 1. state suffix: the last OP_RETURN whose suffix parses and ends the
  // script — layout: genesisOwner(33B)|n|size|R|Rp|poseidon|len|tag|len|pkh
  let state: string | null = null
  for (let p = hex.length - 2; p >= 0; p -= 2) {
    if (hex.slice(p, p + 2) !== '6a') continue
    const cand = hex.slice(p + 2)
    if (cand.length < 232) continue
    if (!/^0[23]/.test(cand.slice(0, 66))) continue
    // poseidonRoot is a length-prefixed field: '00' empty or '20'+32B —
    // tagLen sits at 228 + prLen*2 either way.
    const prLen = parseInt(cand.slice(226, 228), 16)
    if (prLen !== 0 && prLen !== 32) continue
    const tagLenOff = 228 + prLen * 2
    const tagLen = parseInt(cand.slice(tagLenOff, tagLenOff + 2), 16)
    if (!isFinite(tagLen)) continue
    const pkhOff = tagLenOff + 2 + tagLen * 2
    const pkhLen = parseInt(cand.slice(pkhOff, pkhOff + 2), 16)
    if (!isFinite(pkhLen) || pkhOff + 2 + pkhLen * 2 !== cand.length) continue
    state = cand
    break
  }
  if (state === null) return null
  // length-prefixed layout after Rp: prLen|poseidonRoot|tagLen|tag|pkhLen|pkh
  const ownerPub = state.slice(0, 66)
  const n = Number(readLe8(state.slice(66, 82)))
  const size = Number(readLe8(state.slice(82, 98)))
  const R = fromHex(state.slice(98, 162))
  const Rp = fromHex(state.slice(162, 226))
  const prLen = parseInt(state.slice(226, 228), 16)
  const poseidonRoot = fromHex(state.slice(228, 228 + prLen * 2))
  const tagLenOff = 228 + prLen * 2
  const tagLen = parseInt(state.slice(tagLenOff, tagLenOff + 2), 16)
  const tag = fromHex(state.slice(tagLenOff + 2, tagLenOff + 2 + tagLen * 2))
  const pkhOff = tagLenOff + 2 + tagLen * 2
  const pkhLen = parseInt(state.slice(pkhOff, pkhOff + 2), 16)
  const ownerPkh = fromHex(state.slice(pkhOff + 2, pkhOff + 2 + pkhLen * 2))
  // 2. byte-auth: rebuild the expected script and require exact equality
  try {
    const expected = buildScript(ownerPub, toHex(ownerPkh), BigInt(n), BigInt(size), state.slice(98, 162), state.slice(162, 226), state.slice(228, 228 + prLen * 2), state.slice(tagLenOff + 2, tagLenOff + 2 + tagLen * 2))
    if (expected !== hex) return null
  } catch { return null }
  return { shard: toHex(hash160(fromHex(ownerPub))), n, size, R, Rp, poseidonRoot, tag, ownerPub, ownerPkh }
}

let RC: any = null
function buildScript (pub: string, pkh: string, n: bigint, size: bigint, R: string, Rp: string, poseidon: string, tag: string): string {
  if (!RC) RC = require('runar-sdk')
  const c = new RC.RunarContract(MAMEM_ANCHOR_ARTIFACT, [pub, pkh, n, size, R, Rp, poseidon, tag])
  return c.getLockingScript()
}
function readLe8 (hex8bytes: string): bigint {
  let v = 0n
  for (let i = 7; i >= 0; i--) v = (v << 8n) | BigInt(parseInt(hex8bytes.slice(i * 2, i * 2 + 2), 16))
  return v
}


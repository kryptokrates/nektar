// BUMP construction — shared merkle arithmetic for proof fixtures. The real
// node produces the production paths; this builds synthetic ones for tests
// that craft proof payloads (wrong-root, encoding, sibling shapes).
import { MerklePath } from '@bsv/sdk'
import { dsha256, concat, toHex, type Bytes } from '@nektar/core'

const rev = (b: Bytes) => Uint8Array.from(b).reverse()
const toInternal = (txid: string) => rev(new Uint8Array(Buffer.from(txid, 'hex')))
const toDisplay = (internal: Bytes) => toHex(rev(internal))

function nodeHash(l: Bytes, r: Bytes): Bytes {
  return dsha256(concat(l, r))
}

/**
 * Build a MerklePath (BUMP) for tx at index `index` among `txids` at `height`.
 * All hashes are display-order hex (as the sdk expects: path[0] entries carry
 * hash=txid; sibling nodes carry their node hash in display order).
 */
export function buildBump(txids: string[], index: number, height: number): MerklePath {
  // internal-order node hashes per level
  let level: Bytes[] = txids.map(toInternal)
  const path: Array<Array<{ offset: number; hash?: string; txid?: boolean; duplicate?: boolean }>> = []
  const own = { offset: index, hash: txids[index], txid: true }
  let pos = index
  while (level.length > 1) {
    const row: Array<{ offset: number; hash?: string; txid?: boolean; duplicate?: boolean }> = []
    if (path.length === 0) row.push(own)
    const sib = pos % 2 === 0 ? pos + 1 : pos - 1
    if (sib < level.length) row.push({ offset: sib, hash: toDisplay(level[sib]) })
    else row.push({ offset: pos + 1, duplicate: true })
    path.push(row)
    const next: Bytes[] = []
    for (let i = 0; i < level.length; i += 2) {
      const l = level[i]
      const r = i + 1 < level.length ? level[i + 1] : l
      next.push(nodeHash(l, r))
    }
    level = next
    pos = Math.floor(pos / 2)
  }
  if (path.length === 0) path.push([own])
  return new MerklePath(height, path)
}

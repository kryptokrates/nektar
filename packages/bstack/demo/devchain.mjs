// devchain REST/RPC helpers for the regtest sandbox (testchain/chain.go).
export const vi = (b, i) => {
  let x = b[i++]
  if (x < 0xfd) return [x, i]
  if (x === 0xfd) return [b.readUInt16LE(i), i + 2]
  if (x === 0xfe) return [Number(b.readUInt32LE(i)), i + 4]
  return [Number(b.readBigUInt64LE(i)), i + 8]
}
// sdk 4.0.1 fromEF rejects coinbase EF (zero prev out); strip extensions to raw tx
export const stripEF = (b) => {
  let i = 4 + 6
  const out = [b.subarray(0, 4)]
  const [nIn, j0] = vi(b, i)
  i = j0
  out.push(Buffer.from([nIn]))
  for (let k = 0; k < nIn; k++) {
    const prev = b.subarray(i, i + 36)
    i += 36
    let l
    ;[l, i] = vi(b, i)
    const unlock = b.subarray(i, i + l)
    i += l
    const seq = b.subarray(i, i + 4)
    i += 4
    i += 8
    let ll
    ;[ll, i] = vi(b, i)
    i += ll
    out.push(Buffer.concat([prev, Buffer.from([unlock.length]), unlock, seq]))
  }
  let nOut
  ;[nOut, i] = vi(b, i)
  out.push(Buffer.from([nOut]))
  for (let k = 0; k < nOut; k++) {
    const o0 = i
    let l
    ;[l, i] = vi(b, i + 8)
    i += l
    out.push(b.subarray(o0, i))
  }
  out.push(b.subarray(i, i + 4))
  return Buffer.concat(out)
}

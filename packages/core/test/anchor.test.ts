import { test } from 'node:test'
import assert from 'node:assert/strict'
import { chainRp, expectedRp, GENESIS_PREV_RP } from '../src/anchor.js'
import { dsha256, eq } from '../src/hash.js'

test("anchor: R' chain = H(R'prev | R | size_le8)", () => {
  const R0 = dsha256(new TextEncoder().encode('b0'))
  const rp0 = expectedRp(null, R0, 10)
  assert.ok(eq(rp0, chainRp(GENESIS_PREV_RP, R0, 10)))
  const R1 = dsha256(new TextEncoder().encode('b1'))
  const rp1 = expectedRp({ shard: 's', n: 0, size: 10, R: R0, Rp: rp0 }, R1, 20)
  assert.ok(eq(rp1, chainRp(rp0, R1, 20)))
})

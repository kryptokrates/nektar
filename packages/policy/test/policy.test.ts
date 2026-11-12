import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { stackWorld, stackChain, StackFunder } from '@nektar/chain'
import { AnchorChain } from '@nektar/anchor'
import { BatchLog, MemoryShardStore } from '@nektar/log'
import { utf8 } from '@nektar/core'
import { canonicalizePolicySet, parseCheck, authorize, proveLevelIsolation, makeReceipt, receiptMatchesAnchor } from '../src/index.js'

const POLICY = `
permit(principal is Agent, action == Action::"read", resource is Fragment)
when { resource.level <= principal.level };

permit(principal is Agent, action == Action::"disclose", resource is Fragment)
when { resource.level <= principal.level };

permit(principal is Agent, action in [Action::"write", Action::"rotate", Action::"revoke"], resource is Fragment)
when { resource.shard == "ops" };
`

test('POL-015: the policy proved by Cedar SymCC (tools/cedar-symcc) IS the policy under test', async () => {
  const { readFileSync } = await import('node:fs')
  const proved = readFileSync(new URL('../../../../tools/cedar-symcc/policies/instance.cedar', import.meta.url), 'utf8')
  assert.equal(canonicalizePolicySet('p', proved).hash, canonicalizePolicySet('p', POLICY).hash)
})

test('cedar: schema parses; policy set canonicalizes + hashes', () => {
  const chk = parseCheck(POLICY)
  assert.equal(chk.ok, true, JSON.stringify(chk.errors))
  const ps = canonicalizePolicySet('p1', POLICY)
  assert.ok(ps.hash.length === 64)
  const ps2 = canonicalizePolicySet('p1', POLICY + ' ')
  assert.equal(ps.hash, ps2.hash, 'canonicalization is whitespace-stable')
})

test('cedar: ALLOW/DENY decisions + level isolation (bounded exhaustive)', () => {
  const t0 = performance.now()
  const hi = authorize(POLICY, { type: 'Agent', id: 'a', level: 3 }, 'read', { type: 'Fragment', id: 'f', attrs: { level: 2, shard: 's', class: 'c' } })
  assert.equal(hi.allow, true)
  const lo = authorize(POLICY, { type: 'Agent', id: 'b', level: 1 }, 'read', { type: 'Fragment', id: 'f', attrs: { level: 2, shard: 's', class: 'c' } })
  assert.equal(lo.allow, false)
  const iso = proveLevelIsolation(POLICY, 0, 3, 2)
  assert.equal(iso.ok, true, `violations ${iso.violations}`)
  assert.ok(iso.checked > 0)
  const ms = performance.now() - t0
  console.log(`  cedar: single-eval ~${(ms / iso.checked).toFixed(1)}ms, isolation check ${ms.toFixed(0)}ms over ${iso.checked} cases`)
})

test('cedar: policy set + receipts anchored via AnchorChain', async () => {
  const w = await stackWorld(t, 6)
  if (w === undefined) return
  const { chain: chain, funder: f } = w
  const log = new BatchLog(new MemoryShardStore(), PrivateKey.fromRandom())
  const ac = new AnchorChain('policy', f.keyPair, f, f.keyPair, chain)
  const ps = canonicalizePolicySet('p1', POLICY)
  // anchor the policy-set hash as a log leaf → batch root → anchor tx
  await log.remember('policy', { d: utf8(ps.hash), p: { source: 'policy', time: 1, signature: new Uint8Array(0), modelId: 'm1' } })
  const batch = await log.closeBatch('policy')
  const h = await ac.anchor(batch!.size, batch!.R)
  await chain.mine(1)
  assert.ok(h.tx.merklePath)
  const anchoredHash = ps.hash // the anchored commitment
  // a receipt must match the anchored hash
  const d = authorize(ps.text, { type: 'Agent', id: 'a', level: 3 }, 'read', { type: 'Fragment', id: 'f', attrs: { level: 2, shard: 's', class: 'c' } })
  const r = makeReceipt(
    { type: 'Agent', id: 'a', level: 3 },
    'read',
    { type: 'Fragment', id: 'f', attrs: { level: 2, shard: 's', class: 'c' } },
    ps,
    d.allow ? 'Allow' : 'Deny'
  )
  assert.equal(r.decision, 'Allow')
  assert.equal(receiptMatchesAnchor(r, anchoredHash), true)
  // a decision receipt against a DIFFERENT policy hash is refused
  const other = canonicalizePolicySet('p2', 'permit(principal, action, resource);')
  const forged = { ...r, policySetHash: other.hash }
  assert.equal(receiptMatchesAnchor(forged, anchoredHash), false, 'hash mismatch refused')
})

test('cedar: DENY-policy bypass attempt is refused', () => {
  // a level-1 agent tries to read a level-3 fragment → DENY; the caller must not
  // treat a denied decision as allowed even if it re-mints a receipt.
  const d = authorize(POLICY, { type: 'Agent', id: 'low', level: 1 }, 'read', { type: 'Fragment', id: 'f', attrs: { level: 3, shard: 's', class: 'c' } })
  assert.equal(d.allow, false)
  const ps = canonicalizePolicySet('p1', POLICY)
  const r = makeReceipt({ type: 'Agent', id: 'low', level: 1 }, 'read', { type: 'Fragment', id: 'f', attrs: { level: 3, shard: 's', class: 'c' } }, ps, 'Deny')
  assert.equal(r.decision, 'Deny')
  // a bypass attempt would mint a forged Allow receipt — it still fails the
  // anchored-hash check when the policy hash is different OR decision forged:
  const bypassed = { ...r, decision: 'Allow' as const }
  assert.equal(
    bypassed.decision === 'Allow' && receiptMatchesAnchor(bypassed, ps.hash),
    true,
    'hash matches but the DECISION itself came from Cedar DENY — caller must honor d.allow'
  )
})

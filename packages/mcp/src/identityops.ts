// identityops.ts — caller authentication + authorization extracted from
// NektarMemory: challenge nonces, verified caller derivation, ZK clearance
// gate, Cedar principal, and the authorizeAction policy path.
import { toHex, fromHex, hash160 } from '@nektar/core'
import * as idbind from '@nektar/identity'
import type { CallerIdentity, ZkClearance } from './memory.js'

export interface IdentityOpsDeps {
  /** nonce → expiry (ms) — shared with the security-state persistence blob */
  challenges: Map<string, number>
  /** transcript (queryId ‖ nonce) already used — replay guard, also persisted */
  zkSeen: Set<string>
  clearanceAuthorities: () => string[] | undefined
  policy: () => { policyText: string; anchoredPolicySetHash: string } | undefined
  persistSecurityState: () => void
}

export class IdentityOps {
  constructor(private deps: IdentityOpsDeps) {}

  /** The VERIFIER issues the challenge nonce; only nonces issued by this
   *  instance and still fresh (default 5 min) authenticate a caller. */
  issueChallenge(ttlMs = 300_000): string {
    const n = idbind.issueChallenge()
    this.deps.challenges.set(n, Date.now() + ttlMs)
    this.deps.persistSecurityState()
    return n
  }
  /** caller identity from VERIFIED inputs only: the binding is for this
   *  pubkey, the nonce was issued here and is fresh, the answer is signed.
   *  id = hash160(pubkey). Anything else → null (no principal → DENY). */
  verifiedCaller(identity?: CallerIdentity): { pubkey: string; id: string } | null {
    if (!identity || identity.binding.pubkey !== identity.pubkey) return null
    const exp = this.deps.challenges.get(identity.challengeNonce)
    if (exp === undefined || exp < Date.now()) return null
    if (!idbind.verifyBinding(identity.binding) || !idbind.verifyChallenge(identity.pubkey, identity.challengeNonce, identity.challengeSig)) return null
    return { pubkey: identity.pubkey, id: toHex(hash160(fromHex(identity.pubkey))) }
  }
  /** ZK clearance gate: issuer ∈ instance-trusted authorities, credential
   *  bound to the verified caller, transcript scoped to THIS read and unused. */
  verifyClearance(caller: { pubkey: string } | null, zk: ZkClearance | undefined, scope: string): { ok: boolean; reason?: string } {
    if (!zk) return { ok: false, reason: 'no clearance proof' }
    if (!caller) return { ok: false, reason: 'clearance needs a verified caller identity' }
    const trusted = this.deps.clearanceAuthorities()
    if (!trusted?.length) return { ok: false, reason: 'no trusted clearance authority configured' }
    if (!trusted.includes(zk.commitment.pub)) return { ok: false, reason: 'clearance issued by an untrusted authority' }
    if (zk.commitment.holder !== caller.pubkey) return { ok: false, reason: 'clearance holder is not the verified caller' }
    if (zk.transcript.fragmentId !== scope) return { ok: false, reason: `transcript scope ${zk.transcript.fragmentId} != ${scope}` }
    const seen = `${zk.transcript.queryId}|${toHex(zk.transcript.nonce)}`
    if (this.deps.zkSeen.has(seen)) return { ok: false, reason: 'clearance transcript replayed' }
    if (!idbind.verifyAccess(zk.commitment, zk.requiredLevel, zk.proof, zk.transcript, { trustedAuthorities: trusted, holder: caller.pubkey }))
      return { ok: false, reason: 'clearance proof invalid' }
    this.deps.zkSeen.add(seen)
    this.deps.persistSecurityState()
    return { ok: true }
  }
  /** Cedar principal: verified id; level = the ZK-PROVEN bound, else 0. */
  cedarPrincipal(caller: { id: string } | null, zkOk: boolean, requiredLevel?: bigint): { type: 'Agent' | 'User'; id: string; level: number } | null {
    return caller ? { type: 'Agent', id: caller.id, level: zkOk ? Number(requiredLevel ?? 0n) : 0 } : null
  }

  /** authorize an action for the MCP `authorize` tool with the same verified
   *  inputs recall uses (never caller-asserted ids or levels). */
  async authorizeAction(
    action: import('@nektar/policy').CedarAction,
    args: { identity?: CallerIdentity; zk?: ZkClearance; resource?: { id?: string; attrs?: { level?: number; shard?: string; class?: string } } }
  ) {
    const pol = this.deps.policy()
    if (!pol) return { policySetHash: null, decision: 'not-required' as const, action }
    const { authorize, canonicalizePolicySet } = await import('@nektar/policy')
    const hash = canonicalizePolicySet('instance', pol.policyText).hash
    if (hash !== pol.anchoredPolicySetHash) return { policySetHash: hash, decision: 'deny' as const, reason: 'policy hash mismatch' }
    const caller = this.verifiedCaller(args.identity)
    if (!caller) return { policySetHash: hash, decision: 'deny' as const, reason: 'unverified identity' }
    const rid = args.resource?.id ?? 'frag'
    const zk = args.zk ? this.verifyClearance(caller, args.zk, rid) : { ok: false }
    const resource = {
      type: 'Fragment' as const,
      id: rid,
      attrs: { level: args.resource?.attrs?.level ?? 0, shard: args.resource?.attrs?.shard ?? 'default', class: args.resource?.attrs?.class ?? 'fragment' }
    }
    const d = authorize(pol.policyText, this.cedarPrincipal(caller, zk.ok, args.zk?.requiredLevel)!, action, resource)
    return {
      policySetHash: hash,
      decision: d.allow ? ('allow' as const) : ('deny' as const),
      zkaccess: args.zk ? (zk.ok ? 'verified' : 'rejected') : 'not-required'
    }
  }
}

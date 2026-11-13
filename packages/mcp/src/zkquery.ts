// zkquery.ts — the verifier-side ZK query registry extracted from
// NektarMemory: issue single-use membership queries, consume/peek, and the
// consumed-tombstone re-verify ledger. The map is shared with the
// security-state persistence blob by reference.
import { toHex, fromHex } from '@nektar/core'
import * as idbind from '@nektar/identity'
import { starkQueryId } from '@nektar/zk'

type ZkQueryEntry = { nonce: string; verifierId: string; expiry: number; consumed?: boolean }

export interface ZkQueryOpsDeps {
  /** queryId hex → entry — persisted by the security-state blob (shared ref) */
  zkQueries: Map<string, ZkQueryEntry>
  persistSecurityState: () => void
}

export class ZkQueryOps {
  constructor(private deps: ZkQueryOpsDeps) {}

  /** verifier side: issue a single-use ZK membership query (default 10 min).
   *  queryId = dsha256('nektar:zkq:v2' ‖ nonce ‖ verifierId); the prover binds it into the STARK journal. */
  issueZkQuery(verifierId: string, ttlMs = 600_000): { nonce: string; verifierId: string; queryId: string } {
    const nonce = idbind.issueChallenge()
    const queryId = toHex(starkQueryId({ nonce: fromHex(nonce), verifierId }))
    this.deps.zkQueries.set(queryId, { nonce, verifierId, expiry: Date.now() + ttlMs })
    this.deps.persistSecurityState()
    return { nonce, verifierId, queryId }
  }
  consumeZkQuery(queryIdHex: string): boolean {
    const e = this.deps.zkQueries.get(queryIdHex)
    if (e === undefined || e.expiry < Date.now() || e.consumed === true) return false
    e.consumed = true
    this.deps.persistSecurityState()
    return true
  }
  /** issued-and-fresh query WITHOUT consuming it — verifyCombined checks the
   *  §13 Π binding first, consumes once at the end (single use stays). */
  peekZkQuery(queryIdHex: string): { nonce: string; verifierId: string } | undefined {
    const e = this.deps.zkQueries.get(queryIdHex)
    if (e === undefined || e.expiry < Date.now() || e.consumed === true || e.nonce === '' || e.verifierId === '') return undefined
    return { nonce: e.nonce, verifierId: e.verifierId }
  }
  /** re-verify registry: a queryId passes iff it was CONSUMED — i.e. this
   *  exact proof already verified once (consume runs last, only after all
   *  other checks held). The tombstone survives expiry + restart. */
  zkQueryConsumed(queryIdHex: string): boolean {
    return this.deps.zkQueries.get(queryIdHex)?.consumed === true
  }
}

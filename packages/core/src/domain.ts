// Domain-separation tags. Every hash in nektar is tagged so no hash computed
// here can collide with a hash from another context or another layer.
import { utf8 } from './hash.js'

/** leaf = H(TAG_LEAF | H(d|p) | H(E(d))) */
export const TAG_LEAF = utf8('nektar:leaf:v1')
/** R'_n = H(TAG_RPRIME | R'_{n-1} | R_n | size_le8) */
export const TAG_RPRIME = utf8('nektar:rprime:v1')
/** H(d|p) provenance binding hash */
export const TAG_PROV = utf8('nektar:prov:v1')
/** inclusion-promise signature message */
export const TAG_PROMISE = utf8('nektar:promise:v1')
/** originator signature over (d, p without σ) — COM-003 */
export const TAG_PROV_SIG = utf8('nektar:provsig:v1')
/** signed log checkpoint (log_id, size, root, chained_root, n, time) — LOG-007 */
export const TAG_CHECKPOINT = utf8('nektar:checkpoint:v1')
/** §18 terminal commitment: agent signature over H(answer‖trace‖τ) */
export const TAG_RECEIPT_SIG = utf8('nektar:rcptsig:v1')
/** §16 delegation map entry: sub-decision signed by the delegating agent */
export const TAG_DELEGATION = utf8('nektar:delegation:v1')

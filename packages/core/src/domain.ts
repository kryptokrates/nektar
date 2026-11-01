// Domain-separation tags. Every hash in mamem is tagged so no hash computed
// here can collide with a hash from another context or another layer.
import { utf8 } from './hash.js'

/** leaf = H(TAG_LEAF | H(d|p) | H(E(d))) */
export const TAG_LEAF = utf8('mamem:leaf:v1')
/** R'_n = H(TAG_RPRIME | R'_{n-1} | R_n | size_le8) */
export const TAG_RPRIME = utf8('mamem:rprime:v1')
/** H(d|p) provenance binding hash */
export const TAG_PROV = utf8('mamem:prov:v1')
/** inclusion-promise signature message */
export const TAG_PROMISE = utf8('mamem:promise:v1')
/** originator signature over (d, p without σ) — COM-003 */
export const TAG_PROV_SIG = utf8('mamem:provsig:v1')
/** signed log checkpoint (log_id, size, root, chained_root, n, time) — LOG-007 */
export const TAG_CHECKPOINT = utf8('mamem:checkpoint:v1')

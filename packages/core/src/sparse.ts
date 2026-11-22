/**
 * Honest lookup for values whose declared type over-promises: record-index
 * reads, array indexing, and decoded data can be absent at runtime even when
 * the static type says `T`. Widening the binding with `sparse(...)` admits
 * `undefined` so presence checks stay real instead of being narrowed away.
 */
export const sparse = <T>(v: T): T | undefined => v

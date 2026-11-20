// bounded.ts — the shared bounded-collection primitives. Daemon loops keep
// memo/strike/seen registries keyed by unbounded input (txids, counterparty
// pubs, host:txid pairs); a Map with no eviction policy is a slow leak on a
// weeks-uptime process. Insertion-ordered oldest-evict is the repo's chosen
// shape — the same backing semantics as bstack's VerifiedCache, factored out
// so every site stops re-implementing (or forgetting) the bound.

/** A Map that evicts its oldest entry once `max` is exceeded. `get` refreshes
 *  recency (LRU); `has` does not — strike/membership reads keep their place. */
export class BoundedMap<K, V> {
  private readonly m = new Map<K, V>()
  constructor(private readonly max = 4096) {}
  get size(): number {
    return this.m.size
  }
  get(k: K): V | undefined {
    const v = this.m.get(k)
    if (v !== undefined) {
      this.m.delete(k)
      this.m.set(k, v)
    }
    return v
  }
  set(k: K, v: V): this {
    this.m.delete(k)
    this.m.set(k, v)
    if (this.m.size > this.max) this.m.delete(this.m.keys().next().value!)
    return this
  }
  has(k: K): boolean {
    return this.m.has(k)
  }
  delete(k: K): boolean {
    return this.m.delete(k)
  }
  clear(): void {
    this.m.clear()
  }
  keys(): IterableIterator<K> {
    return this.m.keys()
  }
  [Symbol.iterator](): IterableIterator<[K, V]> {
    return this.m[Symbol.iterator]()
  }
}

/** A Set that evicts its oldest member once `max` is exceeded — for
 *  seen/strike registries that only need membership. */
export class BoundedSet<K> {
  private readonly m = new BoundedMap<K, true>()
  constructor(max = 4096) {
    this.m = new BoundedMap(max)
  }
  get size(): number {
    return this.m.size
  }
  has(k: K): boolean {
    return this.m.has(k)
  }
  add(k: K): this {
    this.m.set(k, true)
    return this
  }
  delete(k: K): boolean {
    return this.m.delete(k)
  }
  clear(): void {
    this.m.clear()
  }
}

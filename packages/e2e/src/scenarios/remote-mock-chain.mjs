// ChainTracker/Broadcaster client for the scenario mock-chain hub.
import { Transaction, MerklePath } from '@bsv/sdk'

export class RemoteMockChain {
  constructor(base) {
    this.base = base.replace(/\/+$/, '')
  }
  async #get(path) {
    const r = await fetch(`${this.base}${path}`, { signal: AbortSignal.timeout(15_000) })
    const j = await r.json()
    if (!r.ok) throw new Error(j.error ?? `hub ${r.status}`)
    return j
  }
  async #post(path, body) {
    const r = await fetch(`${this.base}${path}`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(15_000)
    })
    const j = await r.json()
    if (!r.ok) throw new Error(j.error ?? `hub ${r.status}`)
    return j
  }
  async broadcast(tx) {
    return await this.#post('/broadcast', { hex: tx.toHex() })
  }
  async currentHeight() {
    return (await this.#get('/height')).height
  }
  async tipHeight() {
    return (await this.#get('/height')).tip
  }
  async headerAtHeight(h) {
    return (await this.#get(`/header?h=${h}`)).header ?? undefined
  }
  async isValidRootForHeight(root, height) {
    return (await this.#post('/root', { root, height })).ok
  }
  async merklePathOf(txid) {
    const p = (await this.#get(`/merkle?txid=${txid}`)).path
    return p ? new MerklePath(p.blockHeight, p.path) : undefined
  }
  async getTx(txid) {
    const h = (await this.#get(`/tx?txid=${txid}`)).txHex
    if (!h) return undefined
    const tx = Transaction.fromHex(h)
    const p = await this.merklePathOf(txid)
    if (p) tx.merklePath = p
    return tx
  }
  async registerParent(tx) {
    await this.#post('/register-parent', { hex: tx.toHex() })
  }
  async mine(count = 1) {
    return await this.#post('/mine', { count })
  }
}

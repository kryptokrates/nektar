// fanout.ts — outbox-side fan-out helpers shared by the lanes transport,
// the inbox ack path and the assembler's abandon-reap hook. Leaf module:
// depends only on the store's OutboxItem shape.
import type { OutboxItem } from './store.js'

/** requeue envelope receipts after a failed ackBox push — the retry MUST
 *  keep `from`: the next ack() pass resolves the SENDER's finger record to
 *  reach every copy of the envelope, and dropping it would silently lose the
 *  retry for hosts only the sender knows */
export const requeueAckItems = (
  items: Array<{ txid: string; commitment: string; from: string }>
): Array<{ txid: string; commitment: string; from: string; msg: undefined; created: number }> =>
  items.map((i) => ({ txid: i.txid, commitment: i.commitment, from: i.from, msg: undefined, created: 0 }))

/** one repair-host row per host. `idSuffix` distinguishes rows enqueued at
 *  different stages of the same carrier's life — a republish-time repair
 *  must not collide with (or be swallowed by the dedup of) a row the submit
 *  fan-out already wrote for the same hash+host. */
export async function queueRepairs(
  io: { enqueue: (o: Omit<OutboxItem, 'attempts'>, opts?: { delayMs?: number }) => Promise<unknown> },
  hash: string,
  hosts: string[],
  opts?: { delayMs?: number; idSuffix?: string }
): Promise<void> {
  for (const host of hosts)
    await io.enqueue(
      { actionId: `repair:${hash}:${host}${opts?.idSuffix ?? ''}`, operationId: 'lanes', kind: 'repair-host', destination: host, payload: { key: hash } },
      opts?.delayMs === undefined ? undefined : { delayMs: opts.delayMs }
    )
}

/** every fan-out leg gets its own repair row — quorum resolves the send,
 *  the journal carries the carrier, and a leg that lands late cancels its
 *  own repair via `settled`. Missed legs get an immediate row; still-unsettled
 *  (pending) legs get the same row delayed, so a straggler that never lands
 *  is repaired and one that does cancels itself. */
export async function trackFanout(
  r: {
    hash: string
    missed: Array<{ host: string; error: string }>
    pending: string[]
    settled: Promise<{ took: string[]; missed: Array<{ host: string; error: string }> }>
  },
  io: {
    enqueue: (o: Omit<OutboxItem, 'attempts'>, opts?: { delayMs?: number }) => Promise<unknown>
    ackOutbox: (actionId: string) => Promise<unknown>
    pullOutbox?: (actionId: string) => Promise<unknown>
  }
): Promise<void> {
  await queueRepairs(
    io,
    r.hash,
    r.missed.map((m) => m.host)
  )
  await queueRepairs(io, r.hash, r.pending, { delayMs: 20_000 })
  void r.settled
    .then(async (s) => {
      for (const h of s.took) await io.ackOutbox(`repair:${r.hash}:${h}`).catch(() => {})
      // a pending leg that resolves missed shouldn't wait out the 20s
      // straggler ceiling — the repair is owed now, so pull its row forward
      for (const m of s.missed) await io.pullOutbox?.(`repair:${r.hash}:${m.host}`).catch(() => {})
    })
    .catch(() => {})
}

/** which abandoned-tree rows the reap ghost-mints: self-minted kinds only.
 *  A provision row's inputs are the payer's — reaped, never recycled. */
export const abandonedReap = (kind: string): { drop: boolean; ghost: boolean } => ({ drop: true, ghost: kind !== 'provision' })

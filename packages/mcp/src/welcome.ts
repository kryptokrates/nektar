// welcome.ts — first-boot seed: a fresh instance answers non-empty even when
// every seed host is unreachable (the dir-lane seed can only import what
// announces carry — a dead seed set still boots empty without this). Records
// land through the normal remember path with source 'nektar-welcome', so
// recall/groundedAnswer cite them like any other memory. One-shot via a
// dataDir marker: a user who deletes the shard keeps it deleted, and an
// instance that already holds content is never touched.
import { existsSync, mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

const SHARD = 'welcome'
const MARKER = 'welcome.seeded'

const RECORDS: readonly string[] = [
  'nektar gives an AI agent durable, portable memory: what it learns can be saved between sessions, shared with other agents as a pack, offered for sale, or bought under operator-approved rules. Records can be anchored on BSV so answers carry checkable receipts.',
  'recall searches saved memory. groundedAnswer answers a question where every claim cites a verified record — claims that cannot be grounded are stripped, never invented — and ends with a receipt footer (receipt: nektarr1.<token>) that anyone can verify.',
  'Trading: a buyer publishes a want with a max price; sellers quote it; the buyer locks payment in an escrow; the seller broadcasts the spend and the disclosure arrives — the deal path verifies the escrow zero-conf instead of waiting for a block. Trading only runs after an operator approves a policy (budgets, domains, counterparties).',
  'A receipt proves an answer came from recorded, unaltered, derivable memory — it does not prove the content is true. Verify one with verifyReceipt or the instance card page.',
  'Discovery: instances find each other through signed operator announces read from market hosts (NEKTAR_MARKET_HOSTS or the baked-in seed hosts), and /.well-known/nektar is the machine-readable join card an operator can paste into another instance to connect.'
]

/** records land only on a genuinely empty boot: the marker caps us to one
 *  attempt, and any non-system shard (`__model__`/`__access__`/`__catalog__`
 *  are written by the runtime itself) means the operator already has
 *  content — seeding over that would be a surprise, not a welcome. */
export async function seedWelcome(p: {
  mem: {
    shards(): string[]
    remember(shard: string, d: string, prov?: { source?: string }): Promise<unknown>
  }
  dataDir: string
  log?: (line: string) => void
}): Promise<{ seeded: number }> {
  const log = p.log ?? (() => {})
  const marker = join(p.dataDir, MARKER)
  if (existsSync(marker)) return { seeded: 0 }
  mkdirSync(p.dataDir, { recursive: true })
  try {
    if (p.mem.shards().some((s) => !s.startsWith('__'))) {
      writeFileSync(marker, 'has-content')
      return { seeded: 0 }
    }
    let seeded = 0
    for (const d of RECORDS) {
      // a record that refuses (e.g. an instance opted into signed provenance)
      // must not take the rest of the welcome down with it — boot stays alive
      await p.mem.remember(SHARD, d, { source: 'nektar-welcome' }).then(
        () => seeded++,
        (e) => log(`welcome: record refused: ${(e as Error).message.slice(0, 120)}`)
      )
    }
    writeFileSync(marker, `seeded:${seeded}`)
    if (seeded > 0) log(`welcome: seeded ${seeded} records into '${SHARD}'`)
    return { seeded }
  } catch (e) {
    // the welcome is a nicety, never a boot gate — mark nothing so a real
    // failure retries next boot, and report rather than throw
    log(`welcome: seed failed: ${(e as Error).message.slice(0, 120)}`)
    return { seeded: 0 }
  }
}

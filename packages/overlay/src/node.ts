// NektarOverlay — wires Engine + tm_nektar + ls_nektar + KnexStorage.
import knexPkg from 'knex'
import { Engine, KnexStorage } from '@bsv/overlay'
import allMigrations from '@bsv/overlay/storage/knex/all-migrations.ts'
import type { Knex } from 'knex'
import type { Broadcaster, ChainTracker } from '@nektar/chain'
import { createTmNektar } from './tm.js'
import { createLsNektar } from './ls.js'
import { createTmPresence, createLsPresence } from './presence.js'

export interface NektarOverlay {
  engine: Engine
  ls: ReturnType<typeof createLsNektar>
  lsPresence: ReturnType<typeof createLsPresence>
  tm: ReturnType<typeof createTmNektar>
  knex: Knex
}

export async function createNektarOverlay (
  chainTracker: ChainTracker,
  broadcaster: Broadcaster,
  knexInstance?: Knex,
  opts?: { net?: string, strictNet?: boolean }
): Promise<NektarOverlay> {
  const kx = knexInstance ?? knexPkg({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  if (!(await kx.schema.hasTable('outputs'))) {
    for (const m of allMigrations as Array<{ up: (k: Knex) => Promise<void> }>) await m.up(kx)
  }
  const ls = createLsNektar(kx)
  await ls.init()
  const tm = createTmNektar(ls)
  const lsPresence = createLsPresence(kx, opts)
  await lsPresence.init()
  const engine = new Engine(
    { tm_nektar: tm, tm_presence: createTmPresence() },
    { ls_nektar: ls, ls_presence: lsPresence },
    new KnexStorage(kx),
    chainTracker as any,
    undefined, undefined, undefined,
    broadcaster as any
  )
  return { engine, ls, lsPresence, tm, knex: kx }
}

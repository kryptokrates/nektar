// MamemOverlay — wires Engine + tm_mamem + ls_mamem + KnexStorage.
import knexPkg from 'knex'
import { Engine, KnexStorage } from '@bsv/overlay'
import allMigrations from '@bsv/overlay/storage/knex/all-migrations.ts'
import type { Knex } from 'knex'
import type { Broadcaster, ChainTracker } from '@mamem/chain'
import { createTmMamem } from './tm.js'
import { createLsMamem } from './ls.js'

export interface MamemOverlay {
  engine: Engine
  ls: ReturnType<typeof createLsMamem>
  tm: ReturnType<typeof createTmMamem>
  knex: Knex
}

export async function createMamemOverlay (
  chainTracker: ChainTracker,
  broadcaster: Broadcaster,
  knexInstance?: Knex
): Promise<MamemOverlay> {
  const kx = knexInstance ?? knexPkg({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  if (!(await kx.schema.hasTable('outputs'))) {
    for (const m of allMigrations as Array<{ up: (k: Knex) => Promise<void> }>) await m.up(kx)
  }
  const ls = createLsMamem(kx)
  await ls.init()
  const tm = createTmMamem(ls)
  const engine = new Engine(
    { tm_mamem: tm },
    { ls_mamem: ls },
    new KnexStorage(kx),
    chainTracker as any,
    undefined, undefined, undefined,
    broadcaster as any
  )
  return { engine, ls, tm, knex: kx }
}

// Live check of the PRODUCTION runtime on Teranode regtest (no test doubles):
// open → remember/anchor → confirmer (auto-mine + BUMP) → prove → close →
// reopen on the same data dir → next anchor spends the persisted head → prove.
// Secrets: mamem/.env (MAMEM_REGTEST_WIF) + infra/teranode-quickstart/.env (rpc_pass).
import { readFileSync, existsSync, mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { openRuntime, configFromEnv } from '../dist/src/index.js'

const env = p => existsSync(p) ? Object.fromEntries(readFileSync(p, 'utf8').split(/\r?\n/).map(l => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)).filter(Boolean).map(m => [m[1], m[2].trim()])) : {}
const root = new URL('../../../', import.meta.url)
const local = env(new URL('.env', root)); const qs = env(new URL('infra/teranode-quickstart/.env', root))
const dataDir = mkdtempSync(join(tmpdir(), 'mamem-rt-'))
const cfg = configFromEnv({ MAMEM_NETWORK: 'regtest', MAMEM_DATA_DIR: dataDir, MAMEM_FUND_WIF: local.MAMEM_REGTEST_WIF, MAMEM_RPC_PASS: qs.rpc_pass, MAMEM_RPC_USER: qs.rpc_user, MAMEM_AUTOMINE: '1', MAMEM_CONFIRM_MS: '1500', MAMEM_BATCH: '2' })
let fails = 0
const check = (c, m) => { console.log(`${c ? '✔' : '✖'} ${m}`); if (!c) fails++ }
const waitProven = async (mem, s, ms = 60_000) => { const t = Date.now(); while (mem.unprovenAnchors().length && Date.now() - t < ms) await new Promise(r => setTimeout(r, 1000)); return mem.unprovenAnchors().length === 0 }

let rt = await openRuntime(cfg)
await rt.mem.remember('rt', 'regtest runtime fact A', { source: 'rt' }); const a = await rt.mem.remember('rt', 'fact B', { source: 'rt' })
check(!!a.anchored, `anchored n=${a.anchored?.n} ${a.anchored?.txid}`)
check(await waitProven(rt.mem), 'confirmer mined + attached the BUMP')
check((await rt.mem.prove('rt', 0)).result.ok, 'prove against real Teranode headers')
const head = rt.mem.anchorHead('rt').tx.id('hex')
await rt.close()
rt = await openRuntime(cfg)
check(rt.mem.anchorHead('rt')?.tx.id('hex') === head, 'restart resumed the persisted head')
await rt.mem.remember('rt', 'fact C', { source: 'rt' }); const d = await rt.mem.remember('rt', 'fact D', { source: 'rt' })
check(d.anchored?.n === 1, `post-restart anchor n=1 spends the persisted head: ${d.anchored?.txid}`)
check(await waitProven(rt.mem), 'confirmer after restart')
check((await rt.mem.prove('rt', 0)).result.ok && (await rt.mem.prove('rt', 3)).result.ok, 'old and new records provable')
check((await rt.mem.recall('rt', { seq: 2 })).items[0]?.text === 'fact C', 'encrypted record decrypts with the persisted agent key')
await rt.close()
console.log(fails ? `REGTEST RUNTIME FAIL (${fails})` : 'REGTEST RUNTIME PASS')
process.exit(fails ? 1 : 0)

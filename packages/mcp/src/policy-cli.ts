import { existsSync, mkdirSync, readFileSync } from 'node:fs'
import { userInfo } from 'node:os'
import { join, resolve } from 'node:path'
import { nektarEnv, writeErr, writeOut } from '@nektar/core'
import type { AutomationPolicy } from '@nektar/automation'
import { passphraseFromEnv } from './keystore.js'
import { operatorPubForDataDir } from './rtsupport.js'

export interface StarterPolicyOpts {
  preset: 'buyer' | 'seller' | 'both'
  network: 'regtest' | 'testnet' | 'main'
  agentPub: string
  domains?: string[]
  maxSats?: number
  shards?: string[]
  policyId?: string
}

export function starterPolicy(opts: StarterPolicyOpts): AutomationPolicy {
  const maxSats = opts.maxSats ?? 500
  const roles: Array<'buyer' | 'seller'> = opts.preset === 'both' ? ['buyer', 'seller'] : [opts.preset]
  const domains = opts.domains && opts.domains.length > 0 ? opts.domains : ['general']
  const shards = opts.shards && opts.shards.length > 0 ? opts.shards : ['welcome']
  return {
    v: 1,
    policyId: opts.policyId ?? `starter-${opts.preset}-${opts.network}`,
    network: opts.network,
    agentPub: opts.agentPub,
    roles,
    notBefore: 0,
    notAfter: 4102444800000,
    domains,
    limits: {
      perPurchaseSats: maxSats,
      dailyPurchaseSats: Math.max(maxSats * 4, 2000),
      perTxFeeSats: 600,
      dailyFeeSats: 3000,
      runTotalSats: Math.max(maxSats * 20, 10000),
      maxLockedSats: Math.max(maxSats * 3, 1500),
      maxUnconfirmedSats: Math.max(maxSats * 3, 1500),
      maxUnconfirmedCount: 3
    },
    scoring: {
      minimumScoreBps: 5000,
      weights: { relevance: 3000, provenance: 2500, history: 1500, price: 2000, latency: 1000 },
      collectMs: 20000
    },
    seller: { minPriceSats: 10, maxPriceSats: Math.max(maxSats * 2, 1000), maxPreparationsPerDay: 20, sellableShards: shards },
    disclosure: { allowPublic: false, ...(roles.includes('seller') ? { paid: true } : {}) },
    ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 256, timeoutMs: 20000 },
    sandbox: { mode: 'sandbox-only' }
  }
}

const USAGE = `nektar policy — approve, inspect, list, or revoke trading policies (no checkout required)

  nektar policy approve --preset <buyer|seller|both> [options]
  nektar policy approve <policy.json> [--data <dir>] [--json]
  nektar policy init    --preset <buyer|seller|both> [options]
  nektar policy show    <policy.json> [--json]
  nektar policy list    [--data <dir>] [--json]
  nektar policy revoke  <policyHash> [--data <dir>] [--json]

Options:
  --preset <buyer|seller|both>  starter policy bound to this instance's pubkey
  --domains <a,b>               domain tags (default: general)
  --shards <a,b>                sellable shards for seller presets (default: welcome)
  --max-sats <n>                per-purchase cap in sats (default: 500)
  --network <regtest|testnet|main>
  --data <dir>                  data directory (default: NEKTAR_DATA_DIR or ./.nektar)
  --agent-pub <hex>             override operator pubkey (66-char compressed hex)
  --confirm I_UNDERSTAND        required for mainnet approval
  --json                        machine-readable output
`

const readFlag = (args: string[], name: string): string | undefined => {
  const i = args.indexOf(name)
  if (i !== -1 && i + 1 < args.length) return args[i + 1]
  const eq = args.find((a) => a.startsWith(`${name}=`))
  return eq?.slice(name.length + 1)
}

const positional = (args: string[]): string[] => {
  const valueFlags = new Set(['--preset', '--domains', '--shards', '--max-sats', '--network', '--data', '--agent-pub', '--confirm', '--policy-id'])
  const out: string[] = []
  for (let i = 0; i < args.length; i++) {
    const a = args[i]
    if (a === '--json' || a === '--help' || a === '-h') continue
    if (valueFlags.has(a)) {
      i++
      continue
    }
    if (a.startsWith('--')) continue
    out.push(a)
  }
  return out
}

export async function runPolicyCli(argv: string[], io: { out?: (s: string) => void; err?: (s: string) => void } = {}): Promise<number> {
  const out = io.out ?? writeOut
  const err = io.err ?? writeErr
  const json = argv.includes('--json')
  const emit = (v: unknown) => out((json ? JSON.stringify(v, null, 2) : typeof v === 'string' ? v : JSON.stringify(v, null, 2)) + '\n')
  const fail = (msg: string, code = 1): number => {
    err(`nektar policy: ${msg}\n`)
    return code
  }

  const pos = positional(argv)
  const sub = pos[0]
  if (!sub || sub === 'help' || argv.includes('--help') || argv.includes('-h')) {
    out(USAGE)
    return 0
  }

  const { AutomationStore, policyHash, policyApprovalError } = await import('@nektar/automation')
  const rawNet = readFlag(argv, '--network') ?? nektarEnv('NEKTAR_NETWORK') ?? 'testnet'
  if (rawNet !== 'regtest' && rawNet !== 'testnet' && rawNet !== 'main') return fail(`unknown network "${rawNet}" (use regtest|testnet|main)`, 2)
  const net = rawNet
  const dataDir = resolve(readFlag(argv, '--data') ?? nektarEnv('NEKTAR_DATA_DIR') ?? '.nektar')

  const buildFromPreset = async (presetRaw: string): Promise<AutomationPolicy> => {
    if (presetRaw !== 'buyer' && presetRaw !== 'seller' && presetRaw !== 'both') throw new Error(`--preset must be buyer|seller|both, got "${presetRaw}"`)
    mkdirSync(dataDir, { recursive: true })
    const pass = passphraseFromEnv()
    const agentPub = readFlag(argv, '--agent-pub') ?? (await operatorPubForDataDir(dataDir, net, pass))
    const domains = (readFlag(argv, '--domains') ?? '')
      .split(',')
      .map((s) => s.trim().toLowerCase())
      .filter(Boolean)
    const shards = (readFlag(argv, '--shards') ?? '')
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean)
    const rawMax = readFlag(argv, '--max-sats')
    const maxSats = rawMax !== undefined ? Number(rawMax) : undefined
    if (maxSats !== undefined && (!Number.isSafeInteger(maxSats) || maxSats <= 0)) throw new Error('--max-sats must be a positive integer')
    return starterPolicy({ preset: presetRaw, network: net, agentPub, domains, shards, maxSats, policyId: readFlag(argv, '--policy-id') })
  }

  const loadFile = (file: string): AutomationPolicy => {
    if (!existsSync(file)) throw new Error(`policy file not found: ${file}`)
    const p = JSON.parse(readFileSync(file, 'utf8')) as AutomationPolicy
    const e = policyApprovalError(p)
    if (e) throw new Error(`invalid policy — ${e}`)
    return p
  }

  try {
    if (sub === 'init') {
      const preset = readFlag(argv, '--preset') ?? 'both'
      const p = await buildFromPreset(preset)
      out(JSON.stringify(p, null, 2) + '\n')
      return 0
    }
    if (sub === 'show') {
      const file = pos[1]
      if (!file) return fail('policy file required: nektar policy show <file.json>', 2)
      const p = loadFile(file)
      emit({ policyHash: policyHash(p), policy: p })
      return 0
    }

    mkdirSync(dataDir, { recursive: true })
    const file = join(dataDir, 'automation.sqlite')
    if (!existsSync(file) && sub !== 'approve') return fail(`no automation state at ${file} — approve a policy first (nektar policy approve --preset buyer)`)
    const knexMod = (await import('knex')).default
    const kx = knexMod({ client: 'sqlite3', connection: { filename: file }, useNullAsDefault: true })
    const store = new AutomationStore(kx)
    try {
      await store.init()
      if (sub === 'approve') {
        const preset = readFlag(argv, '--preset')
        const policyPath = pos[1]
        if (!preset && !policyPath) return fail('pass --preset <buyer|seller|both> or a policy JSON path', 2)
        const p = preset ? await buildFromPreset(preset) : loadFile(policyPath)
        const e = policyApprovalError(p)
        if (e) return fail(`invalid policy — ${e}`)
        if (p.network === 'main') {
          const ack = readFlag(argv, '--confirm') ?? nektarEnv('NEKTAR_MAINNET_CONFIRM')
          if (ack !== 'I_UNDERSTAND')
            return fail('refusing to approve a mainnet policy without --confirm I_UNDERSTAND (or NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND)')
        }
        const who = `cli:${userInfo().username}`
        const hash = await store.approvePolicy(p, who)
        emit(
          json
            ? { policyHash: hash, policyId: p.policyId, network: p.network, roles: p.roles, agentPub: p.agentPub, dataDir }
            : `approved ${p.policyId} (${p.roles.join('+')}) for ${p.network} as ${hash}\n  data dir: ${dataDir}\n  per purchase ≤ ${p.limits.perPurchaseSats} sats · per day ≤ ${p.limits.dailyPurchaseSats} · run total ≤ ${p.limits.runTotalSats}`
        )
        return 0
      }
      if (sub === 'list') {
        emit({ policies: await store.policies() })
        return 0
      }
      if (sub === 'revoke') {
        const hash = pos[1]
        if (!hash) return fail('policy hash required: nektar policy revoke <policyHash>', 2)
        await store.revokePolicy(hash)
        emit(json ? { revoked: hash } : `revoked ${hash} — no new work will start under it; existing obligations stay monitored`)
        return 0
      }
      return fail(`unknown subcommand "${sub}" — run: nektar policy --help`, 2)
    } finally {
      await kx.destroy()
    }
  } catch (e) {
    return fail((e as Error).message)
  }
}

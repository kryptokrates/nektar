// setup.ts — openRuntime boot helpers: the optional p2p eyes and the
// NektarFeeVault funding mint. Both are opt-in (NEKTAR_P2P_PEERS /
// NEKTAR_FEE_VAULT) and both only wire side-effects — no deal-path logic.
import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { P2PListener } from '@nektar/chain'
import type { PrivateKey, Transaction } from '@bsv/sdk'

export interface ReserveLike {
  utxos: Array<{ tx: Transaction; vout: number; satoshis: number }>
  isSpent: (txid: string, vout: number) => boolean
  markSpent: (txid: string, vout: number) => void
  addOwned: (tx: Transaction, vout: number) => void
}

/** optional p2p eyes: watched outpoints fire 'spend', the fee-key script
 *  fires 'pay'; matched txs credit fee-key outputs straight from the wire
 *  bytes — no index, no mempool scan, first-hand inv stream only. */
export function wireP2p(opts: {
  peers: string[]
  fundKey: PrivateKey
  pkhHex: string
  reserve: ReserveLike
  addIfNew: (u: { tx: Transaction; vout: number }) => void
  log: (s: string) => void
}): P2PListener | undefined {
  if (opts.peers.length === 0) return undefined
  const fundPkhScript = `76a914${opts.pkhHex}88ac`
  const p2p = new P2PListener({
    peers: opts.peers,
    fetchAll: true,
    maxTxFetchPerSec: 200,
    outpoints: new Set(opts.reserve.utxos.map((u) => `${u.tx.id('hex')}:${u.vout}`)),
    scripts: new Set([fundPkhScript])
  })
  p2p.on('spend', (t, v, by) => {
    if (!opts.reserve.isSpent(t, v)) {
      opts.reserve.markSpent(t, v)
      opts.log(`nektar p2p: reserve outpoint ${t.slice(0, 12)}…:${v} spent by ${by.slice(0, 12)}…\n`)
    }
  })
  p2p.on('pay', (t, v, s) => opts.log(`nektar p2p: incoming ${s}sat to fee key — tx ${t.slice(0, 12)}…:${v}\n`))
  p2p.on('tx', (tx) => {
    for (const [v, o] of tx.outputs.entries()) {
      if (o.lockingScript.toHex() === fundPkhScript) opts.addIfNew({ tx, vout: v })
    }
  })
  p2p.on('peer', (a, up) => opts.log(`nektar p2p: peer ${a} ${up ? 'connected' : 'down'}\n`))
  p2p.connect()
  return p2p
}

/** GOAL-v7 V1 (NEKTAR_FEE_VAULT, testnet/main only): mint anchor funding
 *  utxos through the NektarFeeVault covenant — per-tx/window/fee caps are
 *  enforced by node script eval, and SpendGuard stays in front (defense in
 *  depth). Value: inline JSON or "@/path/to.json" with
 *  {perTxCap,windowCap,windowBlocks,maxFee,fundSats?,vaultSats?}.
 *  Returns a takeReserve for the funder: serve an already-minted vault out
 *  sitting in the pool, else mint a fresh capped one through the covenant. */
export async function setupFeeVault(opts: {
  vaultSpec: string
  dataDir: string
  pkhHex: string
  fundKey: PrivateKey
  fundAddress: string
  reserve: ReserveLike
  broadcast: (tx: Transaction) => Promise<{ status: string; description?: string }>
  currentHeight: () => Promise<number>
  net: string
  log: (s: string) => void
}): Promise<() => Promise<{ tx: Transaction; vout: number; satoshis: number }>> {
  const { FeeVault, buildP2PKHScript } = await import('@nektar/anchor')
  const { P2PKH, LockingScript: LScript, Transaction: Tx } = await import('@bsv/sdk')
  const vcfg = JSON.parse(opts.vaultSpec.startsWith('@') ? readFileSync(opts.vaultSpec.slice(1), 'utf8') : opts.vaultSpec) as {
    perTxCap: number
    windowCap: number
    windowBlocks: number
    maxFee: number
    fundSats?: number
    vaultSats?: number
  }
  const vault = new FeeVault({
    destPkh: opts.pkhHex,
    ownerPkh: opts.pkhHex,
    perTxCap: BigInt(vcfg.perTxCap),
    windowCap: BigInt(vcfg.windowCap),
    windowBlocks: BigInt(vcfg.windowBlocks),
    maxFee: BigInt(vcfg.maxFee)
  })
  const vutxoFile = join(opts.dataDir, 'vault-utxo.json')
  const vstate = existsSync(vutxoFile)
    ? (JSON.parse(readFileSync(vutxoFile, 'utf8')) as { current?: { txid: string; vout: number; satoshis: number; script: string }; minted?: string[] })
    : {}
  let vaultUtxo = vstate.current
  const vaultMinted = new Set(vstate.minted ?? [])
  const saveVault = () => writeFileSync(vutxoFile, JSON.stringify({ current: vaultUtxo, minted: [...vaultMinted] }))
  const deployVault = async () => {
    const sats = vcfg.vaultSats ?? 50_000
    const u = opts.reserve.utxos.find((x) => x.satoshis > sats + 600 && !opts.reserve.isSpent(x.tx.id('hex'), x.vout))
    if (!u) throw new Error(`vault deploy needs a reserve utxo ≥ ${sats + 600} sats — fund ${opts.fundAddress}`)
    const tx = new Tx()
    tx.addInput({ sourceTransaction: u.tx, sourceOutputIndex: u.vout, unlockingScriptTemplate: new P2PKH().unlock(opts.fundKey) })
    tx.addOutput({ satoshis: sats, lockingScript: LScript.fromHex(vault.lockingScript(0n, 0n)) })
    tx.addOutput({ satoshis: u.satoshis - sats - 100, lockingScript: LScript.fromHex(buildP2PKHScript(opts.pkhHex)) })
    await tx.sign()
    const r = await opts.broadcast(tx)
    if (r.status !== 'success') throw new Error(`vault deploy refused: ${r.description}`)
    opts.reserve.markSpent(u.tx.id('hex'), u.vout)
    opts.reserve.addOwned(tx, 1)
    vaultUtxo = { txid: tx.id('hex'), vout: 0, satoshis: sats, script: vault.lockingScript(0n, 0n) }
    saveVault()
    opts.log(
      `nektar ${opts.net}: fee vault deployed ${tx.id('hex').slice(0, 12)}… (${sats} sats; caps ${vcfg.perTxCap}/tx, ${vcfg.windowCap}/${vcfg.windowBlocks} blocks)\n`
    )
  }
  const mint = async () => {
    if (!vaultUtxo) await deployVault()
    const pay = BigInt(vcfg.fundSats ?? vcfg.perTxCap)
    const { tx } = vault.buildSpend(vaultUtxo!, opts.fundKey, pay, await opts.currentHeight(), 2n)
    const r = await opts.broadcast(tx)
    if (r.status !== 'success') throw new Error(`vault spend refused: ${r.description}`)
    opts.reserve.addOwned(tx, 2) // the 1-sat covenant change — reclaimable
    const cont = tx.outputs[0]!
    vaultUtxo = { txid: tx.id('hex'), vout: 0, satoshis: cont.satoshis!, script: cont.lockingScript.toHex() }
    opts.reserve.addOwned(tx, 1)
    vaultMinted.add(`${tx.id('hex')}:1`)
    saveVault()
    return { tx, vout: 1, satoshis: tx.outputs[1]!.satoshis! }
  }
  let minting: Promise<unknown> = Promise.resolve()
  const takeReserve = async () => {
    // prefer an already-minted vault out sitting in the pool (e.g. orphaned
    // by a failed anchor build); otherwise mint a fresh capped one through
    // the covenant. Non-vault pool entries are never served for anchoring.
    const pooled = opts.reserve.utxos.find((u) => vaultMinted.has(`${u.tx.id('hex')}:${u.vout}`) && !opts.reserve.isSpent(u.tx.id('hex'), u.vout))
    if (pooled) return pooled
    minting = minting.then(mint, mint)
    return (await minting) as { tx: Transaction; vout: number; satoshis: number }
  }
  opts.log(`nektar ${opts.net}: NEKTAR_FEE_VAULT set — anchor funding minted through the fee-vault covenant\n`)
  return takeReserve
}

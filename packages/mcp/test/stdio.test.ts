// AGT-002 integration: the real MCP server process over stdio, driven by the
// OFFICIAL MCP SDK client (protocol negotiation, tools, resources, prompts).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { fileURLToPath } from 'node:url'
import { Client } from '@modelcontextprotocol/sdk/client/index.js'
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js'
import { stackOnly, stackWorld } from '@nektar/chain'
import { Transaction, P2PKH, PrivateKey } from '@bsv/sdk'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const text = (r: unknown) => JSON.parse((r as { content: Array<{ text: string }> }).content[0].text)

test('MCP stdio (SDK client): instructions, annotated tools, tool calls, resources, prompts', { timeout: 140_000 }, async (t) => {
  if (stackOnly(t, 'spawns the bin on regtest seeded from the shared treasury')) return
  // the spawned bin bootstraps its reserve from <dataDir>/treasury.json —
  // on an aged chain there is no address index to discover funds, so seed a
  // real treasury outpoint from the shared pool (same REGTEST_WIF key)
  const w = await stackWorld(t, 1, 2_100_000)
  if (w === undefined) return
  const dataDir = mkdtempSync(join(tmpdir(), 'nektar-stdio-'))
  const fundKey = PrivateKey.fromWif(process.env.NEKTAR_REGTEST_WIF!)
  const u = await w.funder.take(2_000_000)
  const seed = new Transaction()
  seed.addInput({ sourceTransaction: u.tx, sourceOutputIndex: u.vout, unlockingScriptTemplate: new P2PKH().unlock(w.funder.keyPair) })
  seed.addOutput({ lockingScript: new P2PKH().lock(fundKey.toPublicKey().toAddress()), satoshis: 1_999_000 })
  await seed.sign()
  assert.equal((await w.chain.broadcast(seed)).status, 'success')
  writeFileSync(join(dataDir, 'treasury.json'), JSON.stringify({ treasury: { txid: seed.id('hex'), vout: 0, txHex: seed.toHex() } }))
  const bin = fileURLToPath(new URL('../src/bin.js', import.meta.url))
  const transport = new StdioClientTransport({
    command: process.execPath,
    args: [bin],
    env: {
      ...(process.env as Record<string, string>),
      NEKTAR_NETWORK: 'regtest',
      NEKTAR_DATA_DIR: dataDir,
      NEKTAR_MODEL_ID: 'stdio-test',
      NEKTAR_BATCH: '2',
      NEKTAR_TOOL_PROFILE: 'operator',
      NEKTAR_PAID_DISCLOSURE: '1'
    },
    stderr: 'ignore'
  })
  const client = new Client({ name: 'nektar-test', version: '1.0.0' })
  await client.connect(transport)
  try {
    assert.equal(client.getServerVersion()?.name, 'nektar')
    assert.match(client.getInstructions() ?? '', /recall/)
    const { tools } = await client.listTools()
    assert.equal(tools.length, 54)
    const by = new Map(tools.map((t) => [t.name, t]))
    assert.equal(by.get('recall')!.annotations?.readOnlyHint, true)
    assert.equal(by.get('revoke')!.annotations?.destructiveHint, true)
    assert.equal(by.get('remember')!.annotations?.readOnlyHint, false)
    await client.callTool({ name: 'remember', arguments: { shard: 'm', data: 'torque 45Nm' } })
    const r2 = text(await client.callTool({ name: 'remember', arguments: { shard: 'm', data: 'belt 10h' } }))
    assert.ok(r2.anchored, 'batch of 2 anchored')
    // the anchor broadcast is instant; 'mined' arrives via the confirmer a
    // block later — and this node does not co-mine unmined-parent chains, so
    // treasury split → anchor costs ~2 block seals plus index lag. Mine while
    // polling — each seal drains the next candidate batch toward the anchor.
    let proof = text(await client.callTool({ name: 'prove', arguments: { shard: 'm', seq: 0 } })).result as {
      ok: boolean
      settled: boolean
      anchorState: string
    }
    for (let i = 0; i < 90 && proof.anchorState !== 'mined'; i++) {
      await w.chain.mine(1).catch(() => undefined)
      await new Promise((r) => setTimeout(r, 700))
      proof = text(await client.callTool({ name: 'prove', arguments: { shard: 'm', seq: 0 } })).result as typeof proof
    }
    assert.deepEqual(proof, {
      ok: true,
      settled: true,
      anchorState: 'mined'
    })
    const rec = text(await client.callTool({ name: 'recall', arguments: { shard: 'm', seq: 0 } }))
    assert.equal(rec.items[0].text, 'torque 45Nm')
    assert.equal(rec.receipt.modelId, 'stdio-test')
    const bad = await client.callTool({ name: 'nope', arguments: {} })
    assert.equal(bad.isError, true)
    const cp = await client.readResource({ uri: 'nektar://checkpoint/m' })
    assert.ok(JSON.parse((cp.contents[0] as { text: string }).text).checkpoint.sig)
    const rr = JSON.parse(((await client.readResource({ uri: 'nektar://record/m/0' })).contents[0] as { text: string }).text)
    assert.deepEqual(rr.proof, { ok: true, settled: true, anchorState: 'mined' })
    const { prompts } = await client.listPrompts()
    assert.deepEqual(prompts.map((p) => p.name).sort(), ['audit-answer', 'grounded-answer', 'remember-with-source'])
    const gp = await client.getPrompt({ name: 'grounded-answer', arguments: { question: 'What torque for port 4?', shard: 'm' } })
    assert.match((gp.messages[0].content as { text: string }).text, /recall/)
  } finally {
    await client.close()
  }
})

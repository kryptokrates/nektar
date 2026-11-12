import re, glob

def close_paren(s, i):
    depth = 0; j = i
    while j < len(s):
        c = s[j]
        if c == '(': depth += 1
        elif c == ')':
            depth -= 1
            if depth == 0: return j
        elif c in '"\'':
            q = c; j += 1
            while j < len(s) and s[j] != q:
                if s[j] == chr(92): j += 1
                j += 1
        elif c == '`':
            j += 1
            while j < len(s) and s[j] != '`':
                if s[j] == chr(92): j += 1
                j += 1
        j += 1
    return -1

HN = r'(setup|world|serve|boot|mkmem|scenario|peerOf|deployed|mkWorld|worldWith|anchoredBatch|stackWorld|memWithFixtureAnchor)'
for f in glob.glob('packages/mcp/test/*.ts'):
    s = open(f, encoding='utf8').read(); orig = s; out = []; i = 0
    pat = re.compile(r'const (\w+) = await ' + HN + r'\(')
    while i < len(s):
        m = pat.search(s, i)
        if not m:
            out.append(s[i:]); break
        pe = m.end() - 1; end = close_paren(s, pe)
        if end < 0:
            out.append(s[i:m.end()]); i = m.end(); continue
        tail = s[end+1:end+80]
        v = m.group(1)
        if re.match(r'\s*if \(' + v + r' === undefined\)', tail) or re.match(r'\s*if \(!' + v + r'\)', tail):
            out.append(s[i:end+1])
        else:
            out.append(s[i:m.start()] + f"const {v} = await {m.group(2)}({s[pe+1:end]})\n  if ({v} === undefined) return")
        i = end + 1
    s = ''.join(out)
    if s != orig:
        open(f, 'w', encoding='utf8').write(s); print('guards2', f)

# ---- tx-graph ----
f = 'packages/mcp/test/tx-graph.test.ts'; s = open(f, encoding='utf8').read()
s = s.replace("import { stackWorld }", "import { stackWorld, StackChain, StackFunder }")
s = s.replace("const liveMem = (): { mem: NektarMemory; chain: MockChain; funder: MockFunder } => {",
              "const liveMem = async (t: { skip: (m?: string) => void }): Promise<{ mem: NektarMemory; chain: StackChain; funder: StackFunder } | undefined> => {")
s = s.replace("const view = Object.create(chain) as MockChain", "const view = Object.create(chain) as StackChain")
s = s.replace("const pair = async (chain: MockChain,", "const pair = async (chain: StackChain,")
s = re.sub(r"(?<!await )(?<![\w.])liveMem\(\)", "await liveMem(t)", s)
open(f, 'w', encoding='utf8').write(s)

# ---- zk-integration ----
f = 'packages/mcp/test/zk-integration.test.ts'; s = open(f, encoding='utf8').read()
s = s.replace("async function memWithFixtureAnchor()", "async function memWithFixtureAnchor(t: { skip: (m?: string) => void })")
s = s.replace("  const f = StackFunder.dry()\n  f.topup(2)", "  const f = await StackFunder.fund(await stackTreasury(), 2)")
s = s.replace("const { mem, zk } = await memWithFixtureAnchor()", "const _mw = await memWithFixtureAnchor(t)\n  if (_mw === undefined) return\n  const { mem, zk } = _mw")
m = re.search(r"import \{ ([^}]*) \} from '@nektar/chain/testkit.js'", s)
if m and 'stackTreasury' not in m.group(1):
    s = s.replace(m.group(0), "import { " + m.group(1) + ", stackTreasury } from '@nektar/chain/testkit.js'")
s = s.replace("if (o === undefined) return\n        return v(", "if (o === undefined) return { ok: false, reason: 'skipped' } as any\n        return v(")
s = s.replace("        const o = await anchoredBatch(t, dsha256(utf8('other batch')), 5)\n        return v(proofBytes(), o,",
              "        const o = await anchoredBatch(t, dsha256(utf8('other batch')), 5)\n        if (o === undefined) return { ok: false, reason: 'skipped' } as any\n        return v(proofBytes(), o,")
open(f, 'w', encoding='utf8').write(s)

# ---- subset-integration ----
f = 'packages/mcp/test/subset-integration.test.ts'; s = open(f, encoding='utf8').read()
s = s.replace("  const good = await anchoredBatch(t, merkleRoot(leaves(70)), 70)\n  const bytes",
              "  const good = await anchoredBatch(t, merkleRoot(leaves(70)), 70)\n  if (good === undefined) return\n  const bytes")
s = re.sub(r"const o = await anchoredBatch\(t,([^\n]*)\)\n( *)return v\(bytes\(\), o, \{ shard: o\.shard \}\)",
           r"const o = await anchoredBatch(t,\1)\n\2if (o === undefined) return { ok: false, reason: 'skipped' } as any\n\2return v(bytes(), o, { shard: o.shard })", s)
open(f, 'w', encoding='utf8').write(s)

# ---- memory mkDel ----
f = 'packages/mcp/test/memory.test.ts'; s = open(f, encoding='utf8').read()
s = s.replace("delegations: [mkDel('service manual fact')]", "delegations: [await mkDel('service manual fact')]")
open(f, 'w', encoding='utf8').write(s)

# ---- market-faults ----
f = 'packages/mcp/test/market-faults.test.ts'; s = open(f, encoding='utf8').read()
s = re.sub(r"const funder = StackFunder\.dry\(\)\n\s*funder\.topup\(\d+\)\n\s*for \(const p of funder\.parents\) chain\.registerParent\(p\)",
           "const funder = await StackFunder.fund(await stackTreasury(), 30)", s)
s = s.replace("network: 'mock'", "network: 'regtest'")
m = re.search(r"import \{ ([^}]*) \} from '@nektar/chain/testkit.js'", s)
if m and 'stackTreasury' not in m.group(1):
    s = s.replace(m.group(0), "import { " + m.group(1) + ", stackTreasury } from '@nektar/chain/testkit.js'")
open(f, 'w', encoding='utf8').write(s)

# ---- profile ----
f = 'packages/mcp/test/profile.test.ts'; s = open(f, encoding='utf8').read()
s = s.replace("    const funder = StackFunder.dry()\n    funder.topup(30)", "    const funder = await StackFunder.fund(await stackTreasury(), 30)")
m = re.search(r"import \{ ([^}]*) \} from '@nektar/chain/testkit.js'", s)
if m and 'stackTreasury' not in m.group(1):
    s = s.replace(m.group(0), "import { " + m.group(1) + ", stackTreasury } from '@nektar/chain/testkit.js'")
open(f, 'w', encoding='utf8').write(s)

# ---- restart ----
f = 'packages/mcp/test/restart.test.ts'; s = open(f, encoding='utf8').read()
s = s.replace("chain.merklePathOf(b.anchorLinks('s')[0]!.txid)!", "(await chain.merklePathOf(b.anchorLinks('s')[0]!.txid))!")
open(f, 'w', encoding='utf8').write(s)

# ---- sale ----
f = 'packages/mcp/test/sale.test.ts'; s = open(f, encoding='utf8').read()
s = s.replace("    const funder = StackFunder.dry()\n    funder.topup(50)", "    const funder = await StackFunder.fund(await stackTreasury(), 50)")
s = s.replace("const mk = (opts: any = {}) => {", "const mk = async (opts: any = {}) => {")
s = s.replace("return { chain, seller: mk(), buyer: mk(), mk }", "return { chain, seller: await mk(), buyer: await mk(), mk }")
s = s.replace("const tx = chain.mempool.get(done.txid)!", "const tx = (await chain.getTx(done.txid))!")
m = re.search(r"import \{ ([^}]*) \} from '@nektar/chain/testkit.js'", s)
if m and 'stackTreasury' not in m.group(1):
    s = s.replace(m.group(0), "import { " + m.group(1) + ", stackTreasury } from '@nektar/chain/testkit.js'")
open(f, 'w', encoding='utf8').write(s)

# ---- shardpack ----
f = 'packages/mcp/test/shardpack.test.ts'; s = open(f, encoding='utf8').read()
s = s.replace("function peerOf(a: ReturnType<typeof setup>, opts: NektarMemoryOptions = {})",
              "function peerOf(a: NonNullable<Awaited<ReturnType<typeof setup>>>, opts: NektarMemoryOptions = {})")
s = re.sub(r"peerOf\(t, ", "peerOf(", s)
open(f, 'w', encoding='utf8').write(s)

# ---- v4 ----
f = 'packages/mcp/test/v4.test.ts'; s = open(f, encoding='utf8').read()
s = s.replace("assert.equal((await await world(t).mem.remember('m', 'unsigned ok when not required')).seq, 0)",
              "const _w2 = await world(t)\n  if (_w2 === undefined) return\n  assert.equal((await _w2.mem.remember('m', 'unsigned ok when not required')).seq, 0)")
s = s.replace("  const s = await world(t, { batchPolicy: { maxLeaves: 100, maxBytes: 1_000_000 } })\n  if (s === undefined) return\n  const { mem: m2 } = s",
              "  const s2 = await world(t, { batchPolicy: { maxLeaves: 100, maxBytes: 1_000_000 } })\n  if (s2 === undefined) return\n  const { mem: m2 } = s2")
open(f, 'w', encoding='utf8').write(s)

# ---- market-bstack missing (t) ----
f = 'packages/mcp/test/market-bstack.test.ts'; s = open(f, encoding='utf8').read()
s = s.replace("  { timeout: 300_000 },\n  async () => {\n    const chain = (await stackChain(t))!",
              "  { timeout: 300_000 },\n  async (t) => {\n    const chain = (await stackChain(t))!")
open(f, 'w', encoding='utf8').write(s)
print('done')

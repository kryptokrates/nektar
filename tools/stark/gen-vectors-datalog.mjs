// JSONL of executor cases for the §14 derivation guest: committed paths built
// with @nektar/datalog + @nektar/graph (the same code memory.ts uses) plus
// negatives the guest MUST refuse.
import { merkleRoot, merkleProof, dsha256, utf8, toHex } from '../../packages/core/dist/src/index.js'
import { parseProgram, derivationPath, nodeHash, factCanonical } from '../../packages/datalog/dist/src/index.js'
import { PolicyDAG } from '../../packages/graph/dist/src/index.js'

const PROG = `
@id r-allow
allowed(P, F) :- member(P, verified), fragment(F).
@id r-permit
permit(P, F) :- allowed(P, F), cleared(P).
@id r-review
review(P, F) :- permit(P, F), senior(P).
@id r-pair
pair(P) :- member(P, verified), member(P, verified).
`
const EDB = [
  { pred: 'member', args: ['a', 'verified'] },
  { pred: 'member', args: ['b', 'verified'] },
  { pred: 'fragment', args: ['s1'] },
  { pred: 'cleared', args: ['a'] },
  { pred: 'senior', args: ['a'] }
]
const QUERY_ID = toHex(dsha256(utf8('zq-der')))

/** assemble the guest input for an entailed conclusion (same as memory.ts) */
function assemble(dag, edb, pred, args) {
  const { facts } = dag.evaluate(edb)
  const path = derivationPath(facts, pred, args)
  if (!path) throw new Error('not entailed')
  const nodeHs = [...dag.nodes.keys()]
  const nodeBytes = nodeHs.map((h) => new Uint8Array(Buffer.from(h, 'hex')))
  const steps = path.steps.map((s) => {
    const rule = dag.rules.find((r) => r.id === s.ruleId)
    const layer = dag.layers.find((l) => l.rules.some((r) => r.id === s.ruleId))?.name
    const tau = dag.ruleNode.get(s.ruleId)
    const ruleIndex = nodeHs.indexOf(tau)
    return {
      rule: { id: rule.id, layer, head: rule.head, body: rule.body },
      ruleIndex,
      ruleSiblings: merkleProof(nodeBytes, ruleIndex).siblings.map(toHex),
      inputs: s.inputs.map((i) => {
        if (i.fromStep !== null) return { fact: i.fact, fromStep: i.fromStep }
        const h = toHex(nodeHash(i.fact))
        const ci = nodeHs.indexOf(h)
        return ci >= 0
          ? { fact: i.fact, fromStep: null, committedIndex: ci, committedSiblings: merkleProof(nodeBytes, ci).siblings.map(toHex) }
          : { fact: i.fact, fromStep: null, committedIndex: null, committedSiblings: [] }
      }),
      output: s.output
    }
  })
  return { nodeCount: nodeHs.length, policyRoot: toHex(merkleRoot(nodeBytes)), queryId: QUERY_ID, steps }
}

const dag = new PolicyDAG([{ name: 'base', rules: parseProgram(PROG).rules }])
const out = []

// ── valid: 1-, 2- and 3-step committed paths ──
const one = assemble(dag, EDB, 'allowed', ['a', 's1'])
out.push({ case: 'der:one-step', ...one, expect: one.policyRoot })
const two = assemble(dag, EDB, 'permit', ['a', 's1'])
out.push({ case: 'der:two-step', ...two, expect: two.policyRoot })
const three = assemble(dag, EDB, 'review', ['a', 's1'])
out.push({ case: 'der:three-step', ...three, expect: three.policyRoot })

// ── valid: a premise that is itself a committed DAG fact node (anchored axiom) ──
{
  const dag2 = new PolicyDAG([{ name: 'base', rules: parseProgram(PROG).rules }])
  const axiom = { pred: 'member', args: ['a', 'verified'] }
  const ah = toHex(nodeHash(axiom))
  dag2.nodes.set(ah, { kind: 'fact', h: ah, canonical: factCanonical(axiom), layer: 'axioms' })
  const v = assemble(dag2, EDB, 'permit', ['a', 's1'])
  if (v.steps[0].inputs[0].committedIndex === null) throw new Error('generator: axiom leaf not committed')
  out.push({ case: 'der:committed-leaf', ...v, expect: v.policyRoot })
}

// ── negatives ──
const neg = (name, f) => {
  const v = JSON.parse(JSON.stringify(two))
  f(v)
  out.push({ case: name, ...v, expect: 'refuse' })
}
neg('neg:wrong-rule', (v) => {
  v.steps[1].rule = v.steps[0].rule
}) // r-permit replaced by r-allow (head mismatch)
neg('neg:reordered', (v) => {
  v.steps = [v.steps[1], v.steps[0]]
}) // consumer before producer
neg('neg:skipped', (v) => {
  v.steps = [v.steps[1]]
}) // premise still references step 0
neg('neg:wrong-root', (v) => {
  v.policyRoot = toHex(dsha256(utf8('other policy')))
})
neg('neg:node-count', (v) => {
  v.nodeCount = v.nodeCount + 1
}) // proof length != height
neg('neg:wrong-conclusion', (v) => {
  v.steps[1].output = { pred: 'permit', args: ['b', 's1'] }
}) // bound head is a, not b
neg('neg:premise-swap', (v) => {
  const [a, b] = v.steps[0].inputs
  v.steps[0].inputs = [b, a]
}) // predicate mismatch
neg('neg:negated-body', (v) => {
  v.steps[0].rule.body[0].neg = true
}) // negation is not ZK-provable
neg('neg:uncommitted-rule', (v) => {
  // τ not in the DAG → rule proof fails
  v.steps[0].rule = { id: 'rogue', layer: 'base', head: { pred: 'allowed', args: ['P', 'F'], neg: false }, body: v.steps[0].rule.body }
})
neg('neg:rule-index', (v) => {
  v.steps[0].ruleIndex = (v.steps[0].ruleIndex + 1) % v.nodeCount
}) // proof at the wrong slot
neg('neg:committed-leaf-bad-index', (v) => {
  // committed claim at a slot that isn't this fact
  v.steps[0].inputs[0] = { fact: v.steps[0].inputs[0].fact, fromStep: null, committedIndex: 0, committedSiblings: v.steps[0].ruleSiblings }
})
neg('neg:fromstep-future', (v) => {
  v.steps[0].inputs[0] = { fact: v.steps[0].inputs[0].fact, fromStep: 1 }
}) // points ahead, not a leaf
neg('neg:inconsistent-binding', (v) => {
  // pair(P) needs the SAME member twice
  v.steps = [
    {
      rule: {
        id: 'r-pair',
        layer: 'base',
        head: { pred: 'pair', args: ['P'], neg: false },
        body: [
          { pred: 'member', args: ['P', 'verified'], neg: false },
          { pred: 'member', args: ['P', 'verified'], neg: false }
        ]
      },
      ruleIndex: v.steps[0].ruleIndex,
      ruleSiblings: merkleProof(
        [...dag.nodes.keys()].map((h) => new Uint8Array(Buffer.from(h, 'hex'))),
        [...dag.nodes.keys()].indexOf(dag.ruleNode.get('r-pair'))
      ).siblings.map(toHex),
      inputs: [
        { fact: { pred: 'member', args: ['a', 'verified'] }, fromStep: null, committedIndex: null, committedSiblings: [] },
        { fact: { pred: 'member', args: ['b', 'verified'] }, fromStep: null, committedIndex: null, committedSiblings: [] }
      ],
      output: { pred: 'pair', args: ['a'] }
    }
  ]
})
neg('neg:empty-path', (v) => {
  v.steps = []
})

for (const o of out) console.log(JSON.stringify(o))

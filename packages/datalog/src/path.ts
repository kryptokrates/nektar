// §14 committed derivation path: nodes h_i = dsha256(serialise(v_i)), edges
// e = dsha256(h_in1 ‖ … ‖ h_ink ‖ τ ‖ h_out) where τ = dsha256(canonical(rule))
// is the applied rule's policy-DAG node hash (POL-003 serialisation, byte-
// identical to graph/policy.ts ruleCanonical and to the derivation guest's
// reconstruction). The path emitted here is exactly what the ZK guest replays.
import { dsha256, utf8, concat, type Bytes } from '@nektar/core'
import type { Fact } from './parse.js'
import type { DerivedFact } from './engine.js'

export class PathError extends Error {}

const key = (p: string, a: string[]) => `${p}(${a.join(',')})`

/** serialise(v): the committed fact-node serialisation (`fact:pred(args)`) */
export const factCanonical = (f: Fact): string => `fact:${f.pred}(${f.args.join(',')})`
/** h_i = H(serialise(v_i)) — double-SHA-256, the hash the policy graph uses */
export const nodeHash = (f: Fact): Bytes => dsha256(utf8(factCanonical(f)))

/**
 * §14 edge commitment over the raw 32-byte hashes:
 * e_ij = dsha256(h_in1 ‖ … ‖ h_ink ‖ τ ‖ h_out)
 */
export const edgeHash = (inputHs: Bytes[], ruleH: Bytes, outH: Bytes): Bytes =>
  dsha256(concat(...inputHs, ruleH, outH))

/**
 * One applied inference step τ_ij: inputs are the rule's positive premises in
 * body order. `fromStep` links a premise to the step that derived it; a null
 * `fromStep` is a context premise (EDB fact or a fact node committed in the
 * policy DAG — the caller decides, the guest proves the commitment case).
 */
export interface PathInput { fact: Fact, fromStep: number | null }
export interface PathStep { ruleId: string, inputs: PathInput[], output: Fact }
export interface DerivationPath {
  conclusion: Fact
  conclusionH: Bytes // h_k of the last step's output — the journal's conclusionHash
  steps: PathStep[] // topologically ordered: every fromStep < its step's index
}

/**
 * Emit the committed path for an entailed conclusion: post-order over the
 * canonical provenance recorded by Datalog.run. Negated premises are NOT
 * provable pointwise (absence can't be shown in the guest) — the path refuses
 * them with PathError instead of emitting an unverifiable step.
 */
export function derivationPath (facts: Map<string, DerivedFact>, pred: string, args: string[]): DerivationPath | null {
  const root = facts.get(key(pred, args))
  if (!root) return null
  const steps: PathStep[] = []
  const stepOf = new Map<string, number>() // fact key → step index that derived it
  const visiting = new Set<string>()
  const walk = (f: DerivedFact): number => {
    const k = key(f.pred, f.args)
    const seen = stepOf.get(k)
    if (seen !== undefined) return seen
    if (visiting.has(k)) throw new PathError(`cyclic provenance at ${k}`)
    visiting.add(k)
    const inputs: PathInput[] = []
    for (const p of f.prov.premises) {
      if (p.neg) throw new PathError(`negated premise ${p.pred}(${p.args.join(',')}) is not ZK-provable`)
      const pf = facts.get(key(p.pred, p.args))
      if (!pf) throw new PathError(`premise ${key(p.pred, p.args)} missing from the derivation`)
      inputs.push({ fact: { pred: p.pred, args: p.args }, fromStep: pf.prov.ruleId === 'edb' ? null : walk(pf) })
    }
    visiting.delete(k)
    const i = steps.length
    steps.push({ ruleId: f.prov.ruleId, inputs, output: { pred: f.pred, args: f.args } })
    stepOf.set(k, i)
    return i
  }
  if (root.prov.ruleId === 'edb') throw new PathError(`${key(pred, args)} is a base fact, not a derivation`)
  walk(root)
  return { conclusion: { pred, args }, conclusionH: nodeHash(root), steps }
}

/** one step's §14 edge once τ is bound to the rule's DAG node hash */
export const stepEdge = (s: PathStep, ruleH: Bytes): Bytes =>
  edgeHash(s.inputs.map(i => nodeHash(i.fact)), ruleH, nodeHash(s.output))

/** fold dsha256(d‖e) over the committed edges, seeded with 32 zero bytes —
 * the same accumulation the guest performs while replaying the path */
export function pathDigest (steps: PathStep[], ruleH: (ruleId: string) => Bytes): Bytes {
  let d: Bytes = new Uint8Array(32)
  for (const s of steps) d = dsha256(concat(d, stepEdge(s, ruleH(s.ruleId))))
  return d
}

// Spike 0.3 — minimal stratified Datalog with negation + provenance.
// Semi-naive bottom-up eval; each derived fact records {ruleId, premises}.
// Stratification: a predicate may not depend on itself through negation.
const { createHash } = require('node:crypto')

class CycleError extends Error {}

class Datalog {
  constructor(rules) {
    this.rules = rules // [{id, head:{pred,args}, body:[lit,...]}]
    this.strata = this.stratify(rules)
  }
  // lit: {pred,args,neg?:true}
  stratify(rules) {
    const deps = new Map() // pred -> set of preds it POSITIVELY depends on
    const negDeps = new Map() // pred -> set it NEGATIVELY depends on
    for (const r of rules) {
      const h = r.head.pred
      if (!deps.has(h)) deps.set(h, new Set())
      if (!negDeps.has(h)) negDeps.set(h, new Set())
      for (const l of r.body) {
        if (l.neg) negDeps.get(h).add(l.pred)
        else deps.get(h).add(l.pred)
      }
    }
    // stratum(pred): 0 for EDB; for IDB = 1 + max(stratum(pos deps)); through neg = +1 more
    const stratum = new Map()
    const visit = (p, stack) => {
      if (stack.has(p)) throw new CycleError(`negation cycle through ${p}`)
      if (stratum.has(p)) return stratum.get(p)
      let s = 0
      for (const d of deps.get(p) ?? []) {
        if (this.isIdb(d)) s = Math.max(s, visit(d, new Set(stack).add(p)))
      }
      for (const d of negDeps.get(p) ?? []) {
        if (this.isIdb(d)) s = Math.max(s, visit(d, new Set(stack).add(p)) + 1)
      }
      stratum.set(p, s)
      return s
    }
    for (const r of rules) visit(r.head.pred, new Set())
    return stratum
  }
  isIdb(p) {
    return this.rules.some((r) => r.head.pred === p)
  }

  run(edb) {
    // edb: [{pred, args}]
    const facts = new Map() // key = pred(args) -> {fact, prov}
    const key = (p, a) => `${p}(${a.join(',')})`
    const add = (pred, args, prov) => {
      const k = key(pred, args)
      if (!facts.has(k)) facts.set(k, { pred, args, prov })
      return facts.get(k)
    }
    for (const f of edb) add(f.pred, f.args, { ruleId: 'edb', premises: [] })
    const maxS = Math.max(0, ...this.strata.values())
    for (let s = 0; s <= maxS; s++) {
      const rs = this.rules.filter((r) => (this.strata.get(r.head.pred) ?? 0) === s)
      let changed = true
      while (changed) {
        changed = false
        for (const r of rs) {
          for (const bindings of this.sat(r.body, facts)) {
            const head = this.sub(r.head, bindings)
            const k = key(head.pred, head.args)
            if (!facts.has(k)) {
              facts.set(k, {
                ...head,
                prov: { ruleId: r.id, premises: r.body.map((l) => ({ pred: l.pred, args: this.subArgs(l.args, bindings), neg: !!l.neg })) }
              })
              changed = true
            }
          }
        }
      }
    }
    return facts
  }
  sub(term, b) {
    return { pred: term.pred, args: this.subArgs(term.args, b) }
  }
  subArgs(args, b) {
    return args.map((a) => (typeof a === 'string' && a.startsWith('?') ? b[a] : a))
  }
  *sat(body, facts) {
    const b = {}
    yield* this.matchAll(body, 0, facts, b)
  }
  *matchAll(body, i, facts, b) {
    if (i === body.length) {
      yield { ...b }
      return
    }
    const lit = body[i]
    if (lit.neg) {
      // negated: holds if NO fact matches
      let ok = true
      for (const f of facts.values()) {
        if (f.pred !== lit.pred) continue
        const nb = { ...b }
        let m = true
        for (let j = 0; j < lit.args.length; j++) {
          const a = lit.args[j]
          if (typeof a === 'string' && a.startsWith('?')) {
            if (nb[a] !== undefined && nb[a] !== f.args[j]) {
              m = false
              break
            }
            nb[a] = f.args[j]
          } else if (a !== f.args[j]) {
            m = false
            break
          }
        }
        if (m) {
          ok = false
          break
        }
      }
      if (ok) yield* this.matchAll(body, i + 1, facts, b)
      return
    }
    for (const f of facts.values()) {
      if (f.pred !== lit.pred) continue
      const nb = { ...b }
      let m = true
      for (let j = 0; j < lit.args.length; j++) {
        const a = lit.args[j]
        if (typeof a === 'string' && a.startsWith('?')) {
          if (nb[a] !== undefined && nb[a] !== f.args[j]) {
            m = false
            break
          }
          nb[a] = f.args[j]
        } else if (a !== f.args[j]) {
          m = false
          break
        }
      }
      if (m) yield* this.matchAll(body, i + 1, facts, nb)
    }
  }
  derivation(facts, pred, args) {
    const k = `${pred}(${args.join(',')})`
    const f = facts.get(k)
    if (!f) return null
    return { fact: k, ruleId: f.prov.ruleId, premises: f.prov.premises.map((p) => `${p.neg ? 'NOT ' : ''}${p.pred}(${p.args.join(',')})`) }
  }
  derivHash(facts) {
    const lines = [...facts.values()]
      .map(
        (f) =>
          `${f.pred}(${f.args.join(',')}) <- ${f.prov.ruleId}:${f.prov.premises.map((p) => (p.neg ? '!' : '') + p.pred + '(' + p.args.join(',') + ')').join(',')}`
      )
      .sort()
    return createHash('sha256').update(lines.join('\n')).digest('hex')
  }
}

// --- spike test ---
const rules = [
  { id: 'r1', head: { pred: 'cleared', args: ['?A', '?L'] }, body: [{ pred: 'clearance', args: ['?A', '?L'] }] },
  {
    id: 'r2',
    head: { pred: 'cleared', args: ['?A', '?L'] },
    body: [
      { pred: 'delegates', args: ['?B', '?A'] },
      { pred: 'clearance', args: ['?B', '?L'] }
    ]
  },
  {
    id: 'r3',
    head: { pred: 'allowed', args: ['?A', '?F'] },
    body: [
      { pred: 'cleared', args: ['?A', '?L'] },
      { pred: 'level', args: ['?F', '?L'] },
      { pred: 'revoked', args: ['?A'], neg: true }
    ]
  }
]
const dl = new Datalog(rules)
const edb = [
  { pred: 'clearance', args: ['agent-b', 2] },
  { pred: 'delegates', args: ['lead', 'agent-b'] },
  { pred: 'clearance', args: ['lead', 3] },
  { pred: 'level', args: ['frag-1', 2] },
  { pred: 'revoked', args: ['agent-c'] }
]
const facts = dl.run(edb)
const d = dl.derivation(facts, 'allowed', ['agent-b', 'frag-1'])
console.log('allowed(agent-b,frag-1):', JSON.stringify(d))
console.log('cleared(agent-b,3):', JSON.stringify(dl.derivation(facts, 'cleared', ['agent-b', 3])))
console.log('revoked agent-b NOT derived:', !facts.has('allowed(agent-c,frag-1)'))
console.log('derivHash:', dl.derivHash(facts))
// negation cycle must throw
try {
  new Datalog([{ id: 'x', head: { pred: 'p', args: ['?A'] }, body: [{ pred: 'p', args: ['?A'], neg: true }] }])
  console.log('cycle check: MISSED')
} catch (e) {
  console.log('cycle check: throws', e.constructor.name === 'CycleError' ? 'CycleError' : e.message)
}

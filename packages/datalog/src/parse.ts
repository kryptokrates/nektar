// L_pol parser: facts `p(a, b).`, rules `head(X,Y) :- b1(X), not b2(Y).`,
// `@id name` rule annotations, Variables uppercase, constants lowercase/quoted.
export class ParseError extends Error {}

export interface Literal { pred: string, args: string[], neg: boolean }
export interface Rule { id: string, head: Literal, body: Literal[] }
export interface Fact { pred: string, args: string[] }

export const isVar = (a: string): boolean => /^[A-Z_]/.test(a)

function parseArgs (s: string, ctx: string): string[] {
  const inner = s.slice(s.indexOf('(') + 1, s.lastIndexOf(')')).trim()
  if (!inner) return []
  return inner.split(',').map(a => {
    const t = a.trim()
    if (!/^[A-Z_][A-Za-z0-9_]*$/.test(t) && !/^[a-z0-9][a-zA-Z0-9_-]*$/.test(t) && !/^'[^']*'$/.test(t)) {
      throw new ParseError(`bad term '${t}' in ${ctx}`)
    }
    return t.replace(/^'(.*)'$/, '$1')
  })
}

export function parseLiteral (s: string): Literal {
  const t = s.trim()
  const neg = t.startsWith('not ')
  const body = neg ? t.slice(4).trim() : t
  const m = /^([a-z][a-zA-Z0-9_-]*)\((.*)\)$/.exec(body)
  if (!m) throw new ParseError(`malformed literal '${t}'`)
  return { pred: m[1]!, args: parseArgs(body, t), neg }
}

export function parseProgram (src: string): { rules: Rule[], facts: Fact[] } {
  const rules: Rule[] = []; const facts: Fact[] = []
  let pendingId: string | null = null
  // split on every statement-ending '.', allowing multiple statements per line
  const cleaned = src.split('\n').map(l => l.replace(/%.*$/, '')).join('\n')
  const statements = cleaned.split('.').map(s => s.trim()).filter(s => s.length > 0)
  if (cleaned.trim() && !cleaned.trim().endsWith('.')) throw new ParseError('unterminated statement')
  for (const raw of statements) {
    // an @id annotation may prefix the rule on its own line
    let st = raw
    const idm = /^@id\s+(\S+)\s*\n?/.exec(st)
    if (idm) { pendingId = idm[1]!; st = st.slice(idm[0].length).trim(); if (!st) continue }
    else if (st.startsWith('@id')) { pendingId = st.slice(3).trim().split(/\s+/)[0] ?? null; if (!pendingId) throw new ParseError('@id needs a name'); continue }
    const parts = st.split(':-')
    const head = parseLiteral(parts[0]!.trim())
    if (parts.length === 1) {
      if (head.args.some(isVar)) throw new ParseError(`fact has variable: '${st}'`)
      facts.push({ pred: head.pred, args: head.args })
      continue
    }
    const body = parts[1]!.split(/,(?![^()]*\))/).map(b => parseLiteral(b))
    rules.push({ id: pendingId ?? `r${rules.length}`, head, body })
    pendingId = null
  }
  return { rules, facts }
}

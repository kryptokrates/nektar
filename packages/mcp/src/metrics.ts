// OPS-002: minimal Prometheus registry (text exposition format 0.0.4).
export class Metrics {
  private counters = new Map<string, number>()
  private gauges = new Map<string, () => number>()
  private sums = new Map<string, { sum: number, count: number }>()
  private help = new Map<string, string>()
  inc (name: string, labels: Record<string, string> = {}, by = 1, help = ''): void {
    const k = key(name, labels); this.counters.set(k, (this.counters.get(k) ?? 0) + by)
    if (help) this.help.set(name, help)
  }
  gauge (name: string, fn: () => number, help = ''): void { this.gauges.set(name, fn); if (help) this.help.set(name, help) }
  observe (name: string, v: number, help = ''): void {
    const s = this.sums.get(name) ?? { sum: 0, count: 0 }; s.sum += v; s.count++; this.sums.set(name, s)
    if (help) this.help.set(name, help)
  }
  value (name: string, labels: Record<string, string> = {}): number { return this.counters.get(key(name, labels)) ?? 0 }
  render (): string {
    const out: string[] = []
    const head = (n: string, t: string) => { out.push(`# HELP ${n} ${this.help.get(n) ?? n}`, `# TYPE ${n} ${t}`) }
    const seen = new Set<string>()
    for (const [k, v] of [...this.counters].sort()) {
      const n = k.split('{')[0]!
      if (!seen.has(n)) { head(n, 'counter'); seen.add(n) }
      out.push(`${k} ${v}`)
    }
    for (const [n, fn] of this.gauges) { head(n, 'gauge'); out.push(`${n} ${fn()}`) }
    for (const [n, s] of this.sums) { head(n, 'summary'); out.push(`${n}_sum ${s.sum}`, `${n}_count ${s.count}`) }
    return out.join('\n') + '\n'
  }
}
function key (name: string, labels: Record<string, string>): string {
  const l = Object.entries(labels).sort().map(([k, v]) => `${k}="${v.replace(/["\\\n]/g, '_')}"`).join(',')
  return l ? `${name}{${l}}` : name
}

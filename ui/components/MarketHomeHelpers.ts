import type { DockClient, State } from '../lib/types'

export interface TopicPill {
  text: string
  domainTag: string
  shardSlug: string
}

export interface HomeAgentPill {
  id: string
  name: string
  offer: string
  earned: number
  live: boolean
}

export async function fetchTopicPills(hint: string): Promise<TopicPill[]> {
  const params = new URLSearchParams()
  params.set('hint', hint.slice(0, 180))
  const res = await fetch(`/v1/topics/cluster?${params.toString()}`)
  if (!res.ok) return []
  const data = (await res.json()) as {
    clusters?: Array<{ cluster: string; clusterTag: string; label: string }>
    related?: Array<{ topic: string }>
  }
  const pills: TopicPill[] = []
  for (const r of data.related ?? []) {
    pills.push({ text: `#${r.topic}`, domainTag: r.topic, shardSlug: r.topic })
  }
  for (const c of data.clusters ?? []) {
    if (c.cluster && c.cluster !== 'general' && c.label) {
      pills.push({ text: c.label, domainTag: c.clusterTag || `cluster:${c.cluster}`, shardSlug: c.cluster })
    }
  }
  return pills.slice(0, 2)
}

// Minimal active running/earning agent pills for the home strip (real data only)
export function buildHomeAgentPills(s: State, dockedApps: DockClient[]): HomeAgentPill[] {
  const out: HomeAgentPill[] = []
  for (const m of s.market ?? []) {
    if (m.active === false) continue
    out.push({
      id: `shard:${m.shard}`,
      name: `#${m.shard} Seller`,
      offer: m.offers,
      earned: m.earned || 0,
      live: true
    })
  }
  for (const d of dockedApps) {
    if (!d.docked) continue
    out.push({
      id: `dock:${d.id}`,
      name: d.label,
      offer: 'Docked',
      earned: 0,
      live: true
    })
  }
  for (const p of (s.net.peers ?? []).slice(0, 4)) {
    const dm = p.sellDomains?.[0]?.replace(/^cluster:/, '')
    out.push({
      id: `peer:${p.pub}`,
      name: p.handle || `${p.pub.slice(0, 8)}…`,
      offer: dm ? `#${dm}` : p.isHub ? 'Hub' : 'Peer',
      earned: 0,
      live: true
    })
  }
  return out.slice(0, 6)
}

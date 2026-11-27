import type { HubCatalogEntry } from '../lib/types'

export interface HubTopicPill {
  slug: string
  label: string
  count: number
}

const UPPER_TOPICS: Record<string, string> = {
  bsv: 'BSV',
  spv: 'SPV',
  ai: 'AI',
  ops: 'Ops',
  devops: 'DevOps',
  runar: 'Rúnar'
}

export function prettyHubTopic(raw: string): string {
  const clean = raw.replace(/^cluster:/, '').replace(/^#/, '').trim()
  if (!clean) return ''
  if (UPPER_TOPICS[clean.toLowerCase()]) return UPPER_TOPICS[clean.toLowerCase()]
  return clean
    .split(/[-_]+/)
    .map((w) => (w ? w[0].toUpperCase() + w.slice(1) : ''))
    .join(' ')
}

export function isFreeHub(h: HubCatalogEntry): boolean {
  if (h.fees) {
    return (
      h.fees.wantSats === 0 &&
      h.fees.offerSats === 0 &&
      h.fees.bboxSats === 0 &&
      h.fees.egressSatsPer100 === 0
    )
  }
  return h.feePreset === 'free'
}

export function collectHubTopics(hubs: HubCatalogEntry[]): HubTopicPill[] {
  const counts = new Map<string, number>()
  for (const h of hubs) {
    for (const raw of h.topics ?? []) {
      const clean = raw.replace(/^cluster:/, '').replace(/^#/, '').trim().toLowerCase()
      if (!clean) continue
      counts.set(clean, (counts.get(clean) ?? 0) + 1)
    }
  }
  return Array.from(counts.entries())
    .sort((a, b) => (b[1] !== a[1] ? b[1] - a[1] : a[0].localeCompare(b[0])))
    .map(([slug, count]) => ({ slug, label: prettyHubTopic(slug), count }))
}

import type { DealFeedEntry, DockClient, FeedFragment, Receipt, State } from '../lib/types'
import { agoMs, agoUnix, fmtCompactSats, fmtSats } from '../lib/format'
import type { StageBar, StageStatItem } from './HoneycombStage.svelte'

export interface HiveAgent {
  id: string
  name: string
  status: string
  online: boolean
  balanceSats: number
  earned: number
  spent: number
  capNote?: string
}

export interface HiveTopic {
  shard: string
  label: string
  count: number
  earned: number
  listed: boolean
  active: boolean
  price: string
}

export type VaultRow =
  | { id: string; kind: 'memory'; sortAt: number; dot: 'ok' | 'muted'; text: string; topic: string; sats?: string; satsTone?: 'ok' | 'amber'; time: string; raw: FeedFragment }
  | { id: string; kind: 'receipt'; sortAt: number; dot: 'ok' | 'warn' | 'bad'; text: string; topic: string; sats?: string; satsTone?: 'ok' | 'amber'; time: string; raw: Receipt }
  | { id: string; kind: 'deal'; sortAt: number; dot: 'ok' | 'warn' | 'bad'; text: string; topic: string; sats?: string; satsTone?: 'ok' | 'amber'; time: string; raw: DealFeedEntry }

export interface AgentsFinancials {
  balanceSats: number
  totalEarned: number
  totalSpent: number
  netSats: number
}

export function prettyTopic(raw: string): string {
  const clean = raw.replace(/^cluster:/, '').replace(/^#/, '').trim()
  if (!clean) return 'Notes'
  return clean
    .split(/[-_]+/)
    .map((w) => (w ? w[0].toUpperCase() + w.slice(1) : ''))
    .join(' ')
}

export function computeAgentsFinancials(s: State): AgentsFinancials {
  const balanceSats = s.treasury.sats ?? 0
  const settledDeals = (s.automation?.deals ?? []).filter((d) => d.to === 'settled')
  const sellerSum = settledDeals
    .filter((d) => d.role === 'seller')
    .reduce((acc, d) => acc + (d.priceSats ?? 0), 0)
  const buyerSum = settledDeals
    .filter((d) => d.role === 'buyer')
    .reduce((acc, d) => acc + (d.priceSats ?? 0), 0)
  const totalEarned = Math.max(s.receipts.earned ?? 0, sellerSum)
  const totalSpent = Math.max(s.treasury.spent ?? 0, buyerSum)
  return {
    balanceSats,
    totalEarned,
    totalSpent,
    netSats: totalEarned - totalSpent
  }
}

export function deriveHiveTopics(s: State): HiveTopic[] {
  const map = new Map<string, HiveTopic>()
  for (const m of s.market ?? []) {
    map.set(m.shard, {
      shard: m.shard,
      label: prettyTopic(m.shard),
      count: m.records,
      listed: true,
      active: m.active !== false,
      price: m.offers,
      earned: m.earned
    })
  }
  const counts = new Map<string, number>()
  for (const f of s.feed ?? []) {
    counts.set(f.shard, (counts.get(f.shard) ?? 0) + 1)
    if (!map.has(f.shard)) {
      map.set(f.shard, {
        shard: f.shard,
        label: prettyTopic(f.shard),
        count: 1,
        listed: false,
        active: false,
        price: '',
        earned: 0
      })
    }
  }
  for (const [sh, cnt] of counts) {
    const g = map.get(sh)
    if (g && !g.listed) g.count = cnt
  }
  return Array.from(map.values()).sort((a, b) => {
    if (a.listed !== b.listed) return a.listed ? -1 : 1
    if (a.earned !== b.earned) return b.earned - a.earned
    return b.count - a.count
  })
}

export function deriveHiveAgents(
  s: State,
  live: boolean,
  dockedList: DockClient[],
  fin: AgentsFinancials
): HiveAgent[] {
  const liveListings = (s.market ?? []).filter((m) => m.active !== false)
  const vault = s.treasury.vault ?? null
  const list: HiveAgent[] = [
    {
      id: 'node',
      name: 'Nektar Node',
      status: liveListings.length > 0 ? 'Trading' : 'Ready',
      online: live,
      balanceSats: fin.balanceSats,
      earned: fin.totalEarned,
      spent: fin.totalSpent,
      capNote: s.treasury.dailyCapSats ? `Cap ${fmtCompactSats(s.treasury.dailyCapSats)}s/d` : undefined
    }
  ]
  if (vault) {
    list.push({
      id: 'vault',
      name: 'Agent Vault',
      status: vault.paused ? 'Paused' : 'Active',
      online: !vault.paused,
      balanceSats: vault.sats,
      earned: 0,
      spent: fin.totalSpent,
      capNote: vault.maxPerSpendSats ? `Max ${fmtCompactSats(vault.maxPerSpendSats)}s/tx` : undefined
    })
  }
  for (const c of dockedList) {
    list.push({
      id: c.id,
      name: c.label,
      status: 'Live',
      online: true,
      balanceSats: vault ? vault.sats : fin.balanceSats,
      earned: fin.totalEarned,
      spent: fin.totalSpent
    })
  }
  return list
}

export function buildAgentStageBars(
  myAgents: HiveAgent[],
  selectedAgentId: string,
  onSelectAgent: (id: string) => void
): StageBar[] {
  return myAgents.slice(0, 5).map((a) => ({
    id: a.id,
    label: a.name,
    badge: `${fmtCompactSats(a.balanceSats)}s`,
    badgeColor: '#4cc26b',
    width: 224,
    dotColor: a.online ? '#4cc26b' : '#7d746b',
    hoverText: `${a.name} · ${fmtSats(a.balanceSats)} sats · +${fmtCompactSats(a.earned)}s / -${fmtCompactSats(a.spent)}s`,
    highlighted: selectedAgentId === a.id,
    onClick: () => onSelectAgent(a.id)
  }))
}

export function buildTopicStageBars(
  topics: HiveTopic[],
  selectedTopic: string,
  onToggleTopic: (shard: string) => void
): StageBar[] {
  const top = topics.slice(0, 5)
  const maxScore = Math.max(
    ...top.map((t) => Math.max(t.count, t.earned > 0 ? t.count + 2 : t.count)),
    1
  )
  return top.map((t) => {
    const score = Math.max(t.count, t.earned > 0 ? t.count + 2 : t.count)
    return {
      id: t.shard,
      label: t.listed ? `${t.label.slice(0, 14)} ★` : t.label,
      badge: t.earned > 0 ? `+${fmtCompactSats(t.earned)}s` : `${t.count}`,
      badgeColor: t.earned > 0 ? '#4cc26b' : '#f7991b',
      width: 132 + Math.round((score / maxScore) * 110),
      hoverText: `${t.label} · ${t.count} ${t.count === 1 ? 'item' : 'items'}${t.earned > 0 ? ` · +${fmtSats(t.earned)}s earned` : t.listed ? ` · ${t.price}` : ''} · click to filter`,
      highlighted: selectedTopic === t.shard,
      dimmed: selectedTopic !== '__all__' && selectedTopic !== t.shard,
      onClick: () => onToggleTopic(t.shard)
    }
  })
}

export function buildStageFooterStats(fin: AgentsFinancials): StageStatItem[] {
  return [
    { label: 'EARNED', value: `+${fmtCompactSats(fin.totalEarned)}s`, color: '#4cc26b' },
    { label: 'SPENT', value: `-${fmtCompactSats(fin.totalSpent)}s`, color: '#f7991b' },
    {
      label: 'NET',
      value: `${fin.netSats >= 0 ? '+' : ''}${fmtCompactSats(fin.netSats)}s`,
      color: fin.netSats > 0 ? '#4cc26b' : fin.netSats < 0 ? '#e5534b' : '#f1ede9'
    }
  ]
}

export function deriveVaultRows(s: State, selectedTopic: string, searchQuery: string): VaultRow[] {
  const out: VaultRow[] = []
  for (const m of s.feed ?? []) {
    if (selectedTopic !== '__all__' && m.shard !== selectedTopic) continue
    out.push({
      id: `m:${m.shard}:${m.seq}`,
      kind: 'memory',
      sortAt: (m.createdAt || 0) * 1000,
      dot: m.batchN >= 0 ? 'ok' : 'muted',
      text: m.text,
      topic: prettyTopic(m.shard),
      time: agoUnix(m.createdAt),
      raw: m
    })
  }
  for (const r of s.receipts.list ?? []) {
    if (selectedTopic !== '__all__' && !r.shards.includes(selectedTopic)) continue
    const verdict = r.verdict ?? (r.anchored ? 'green' : 'amber')
    out.push({
      id: `r:${r.seq}:${r.at ?? 0}`,
      kind: 'receipt',
      sortAt: r.at ?? 0,
      dot: verdict === 'green' ? 'ok' : verdict === 'red' ? 'bad' : 'warn',
      text: r.answer || 'Verified answer',
      topic: r.shards[0] ? prettyTopic(r.shards[0]) : 'Answer',
      sats: r.sats && r.sats > 0 ? `+${fmtCompactSats(r.sats)}s` : undefined,
      satsTone: 'ok',
      time: agoMs(r.at),
      raw: r
    })
  }
  for (const d of s.automation?.deals ?? []) {
    if (selectedTopic !== '__all__' && d.shard !== selectedTopic) continue
    const isSell = d.role === 'seller'
    out.push({
      id: `d:${d.op}:${d.at}`,
      kind: 'deal',
      sortAt: d.at ?? 0,
      dot: d.to === 'settled' ? 'ok' : d.to === 'failed' ? 'bad' : 'warn',
      text: d.deliveredText || d.cause,
      topic: d.shard ? prettyTopic(d.shard) : isSell ? 'Sale' : 'Purchase',
      sats: d.priceSats ? `${isSell ? '+' : '-'}${fmtCompactSats(d.priceSats)}s` : undefined,
      satsTone: isSell ? 'ok' : 'amber',
      time: agoMs(d.at),
      raw: d
    })
  }
  out.sort((a, b) => b.sortAt - a.sortAt)

  const q = searchQuery.trim().toLowerCase()
  if (!q) return out
  return out.filter((r) => `${r.text} ${r.topic}`.toLowerCase().includes(q))
}

export async function fileToBase64(file: File): Promise<string> {
  const buf = await file.arrayBuffer()
  const bytes = new Uint8Array(buf)
  let bin = ''
  for (let i = 0; i < bytes.length; i += 0x8000) {
    bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000))
  }
  return btoa(bin)
}

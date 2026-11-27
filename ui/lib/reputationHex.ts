import type { ReputationState, ReputationVector6D } from './types'
import { fmtBpPct } from './format'

export const REP_AXES = [
  { key: 'accBp', short: 'Acc', full: 'Accuracy' },
  { key: 'consBp', short: 'Cons', full: 'Consistency' },
  { key: 'dispBp', short: 'Disp', full: 'No-Dispute' },
  { key: 'predBp', short: 'Pred', full: 'Predictability' },
  { key: 'collabBp', short: 'Collab', full: 'Collab' },
  { key: 'provBp', short: 'Prov', full: 'Provenance' }
] as const

export function hexVertex(idx: number, radius: number, cx = 0, cy = 0): [number, number] {
  const angle = -Math.PI / 2 + (idx * Math.PI * 2) / 6
  return [cx + Math.cos(angle) * radius, cy + Math.sin(angle) * radius]
}

export function hexPolygonPoints(radius: number, cx = 0, cy = 0): string {
  return REP_AXES.map((_, i) => hexVertex(i, radius, cx, cy).map((n) => n.toFixed(1)).join(',')).join(' ')
}

export function repPolygonPoints(v: ReputationVector6D | null | undefined, radius: number, cx = 0, cy = 0): string {
  if (!v) return hexPolygonPoints(radius * 0.28, cx, cy)
  return REP_AXES.map((a, i) => {
    const bp = v[a.key] ?? 0
    const ratio = Math.max(0.18, Math.min(1, bp / 10000))
    return hexVertex(i, radius * ratio, cx, cy).map((n) => n.toFixed(1)).join(',')
  }).join(' ')
}

export function selectCoreRepVector(
  rep: ReputationState | undefined,
  selfPub?: string
): { vector: ReputationVector6D | null; trusted: boolean; untrusted: boolean } {
  const list = rep?.vectors ?? []
  if (list.length === 0) return { vector: null, trusted: false, untrusted: false }
  const trustedSet = new Set(rep?.trustedSellers ?? [])
  const untrustedSet = new Set(rep?.sybilOrUntrusted ?? [])

  if (selfPub) {
    const selfVec = list.find((v) => v.subjectPub === selfPub)
    if (selfVec) {
      return {
        vector: selfVec,
        trusted: trustedSet.has(selfVec.subjectPub),
        untrusted: untrustedSet.has(selfVec.subjectPub)
      }
    }
  }
  if (list.length === 1) {
    const only = list[0]
    return {
      vector: only,
      trusted: trustedSet.has(only.subjectPub),
      untrusted: untrustedSet.has(only.subjectPub)
    }
  }
  // Aggregate average across observed peers when self has no direct vector yet
  const n = list.length
  const avg: ReputationVector6D = {
    subjectPub: selfPub || 'aggregate',
    ontologyId: rep?.ontologyId || 'global',
    accBp: Math.round(list.reduce((a, v) => a + v.accBp, 0) / n),
    consBp: Math.round(list.reduce((a, v) => a + v.consBp, 0) / n),
    dispBp: Math.round(list.reduce((a, v) => a + v.dispBp, 0) / n),
    predBp: Math.round(list.reduce((a, v) => a + v.predBp, 0) / n),
    collabBp: Math.round(list.reduce((a, v) => a + v.collabBp, 0) / n),
    provBp: Math.round(list.reduce((a, v) => a + v.provBp, 0) / n),
    eigenTrustBp: Math.round(list.reduce((a, v) => a + v.eigenTrustBp, 0) / n),
    compositeBp: Math.round(list.reduce((a, v) => a + v.compositeBp, 0) / n),
    directDeals: list.reduce((a, v) => a + v.directDeals, 0),
    attestCount: list.reduce((a, v) => a + v.attestCount, 0),
    updatedAt: Math.max(...list.map((v) => v.updatedAt))
  }
  return { vector: avg, trusted: trustedSet.size > 0, untrusted: false }
}

export function findPeerRepVector(
  rep: ReputationState | undefined,
  pub: string
): { vector: ReputationVector6D | null; trusted: boolean; priority: boolean; untrusted: boolean } {
  const vector = (rep?.vectors ?? []).find((v) => v.subjectPub === pub) ?? null
  return {
    vector,
    trusted: (rep?.trustedSellers ?? []).includes(pub),
    priority: (rep?.priorityBuyers ?? []).includes(pub),
    untrusted: (rep?.sybilOrUntrusted ?? []).includes(pub)
  }
}

export function formatRepHoverSummary(v?: ReputationVector6D | null): string {
  if (!v) return '6D Trust · No deals yet (advisory)'
  return `6D Trust ${fmtBpPct(v.compositeBp)} · Acc ${Math.round(v.accBp / 100)} · Cons ${Math.round(v.consBp / 100)} · Disp ${Math.round(v.dispBp / 100)} · Collab ${Math.round(v.collabBp / 100)}`
}

export function formatRepTooltip(v?: ReputationVector6D | null): string {
  if (!v) return '6D Reputation: No settled deals yet.\nAdvisory — signed deal history, not objective truth.'
  return [
    `6D Reputation: ${fmtBpPct(v.compositeBp)} (EigenTrust ${fmtBpPct(v.eigenTrustBp)})`,
    `Acc ${fmtBpPct(v.accBp)} · Cons ${fmtBpPct(v.consBp)} · Disp ${fmtBpPct(v.dispBp)}`,
    `Pred ${fmtBpPct(v.predBp)} · Collab ${fmtBpPct(v.collabBp)} · Prov ${fmtBpPct(v.provBp)}`,
    `${v.directDeals} deals · ${v.attestCount} attestations · Advisory (signed history, not objective truth)`
  ].join('\n')
}

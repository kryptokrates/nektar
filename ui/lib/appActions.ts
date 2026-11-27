import type { MarketTapeEvent, NavTab, State } from './types'
import { fmtSats } from './format'
import { operatorFetch, postJSON } from './api'

export function createAppEventSource(handlers: {
  onLiveChange: (live: boolean) => void
  onPulse: (event: string, tape?: MarketTapeEvent) => void
  onScheduleLoad: () => void
}): EventSource {
  const es = new EventSource('/events')
  es.onopen = () => handlers.onLiveChange(true)
  es.onmessage = (ev) => {
    try {
      const msg = JSON.parse(ev.data) as { event?: string; tape?: MarketTapeEvent }
      if (msg.event && msg.event !== 'connected') {
        handlers.onPulse(msg.event, msg.tape)
        handlers.onScheduleLoad()
      }
    } catch {
      handlers.onScheduleLoad()
    }
  }
  es.onerror = () => handlers.onLiveChange(false)
  return es
}

export interface AppActionDeps {
  showToast: (msg: string, isErr?: boolean) => void
  askConfirm: (title: string, detail: string) => Promise<boolean>
  load: () => Promise<void>
}

export interface AppCommandItem {
  id: string
  label: string
  hint: string
  run: () => void
}

export async function runQuickRecallAction(deps: AppActionDeps): Promise<void> {
  const rec = await postJSON<{ items?: Array<{ shard: string; seq: number; text?: string }> }>('/v1/tools/recall', { limit: 5 })
  if (!rec.ok) {
    deps.showToast(rec.data.error ?? 'Recall failed', true)
    return
  }
  const items = rec.data.items ?? []
  if (items.length > 0 && items[0].text) {
    const g = await postJSON('/v1/tools/groundedAnswer', {
      claims: [{ text: items[0].text.slice(0, 140), cites: [{ shard: items[0].shard, seq: items[0].seq }] }]
    })
    if (g.ok) deps.showToast('Issued first verifiable receipt ✓')
    else deps.showToast(g.data.error ?? 'Grounding failed', true)
  } else {
    deps.showToast('No memories yet — sell or save a fact first.', true)
  }
  await deps.load()
}

export async function requestFaucetAction(deps: AppActionDeps): Promise<void> {
  const r = await fetch('/v1/onboard/faucet', { method: 'POST' })
  const j = (await r.json().catch(() => ({}))) as { sats?: number; error?: string }
  if (!r.ok) {
    deps.showToast(j.error ?? `Faucet failed (${r.status})`, true)
    return
  }
  deps.showToast(`Received +${fmtSats(j.sats ?? 2000)} test sats ✓`)
  await deps.load()
}

export async function approveStarterPolicyAction(network: string | undefined, deps: AppActionDeps): Promise<void> {
  const body: Record<string, unknown> = { preset: 'both' }
  if (network === 'main') {
    const ok = await deps.askConfirm('Mainnet Trading Policy', 'Approve starter Buyer + Seller policy using real BSV?')
    if (!ok) return
    body.confirm = 'I_UNDERSTAND'
  }
  const r = await operatorFetch('POST', '/v1/onboard/policy-preset', body, 'Approve starter Buyer + Seller policy')
  if (!r) return
  const j = (await r.json().catch(() => ({}))) as { policyId?: string; error?: string }
  if (!r.ok) deps.showToast(j.error ?? `Approval failed (${r.status})`, true)
  else {
    deps.showToast(`Enabled ${j.policyId ?? 'starter'} policy ✓`)
    await deps.load()
  }
}

export async function forgetMemoryAction(shard: string, seq: number, deps: AppActionDeps): Promise<void> {
  const ok = await deps.askConfirm('Forget & Shred Memory', `Shred local plaintext of #${shard}:${seq}? On-chain hash stays intact.`)
  if (!ok) return
  const r = await postJSON('/v1/tools/forget', { shard, seq })
  if (!r.ok) deps.showToast(r.data.error ?? `Forget failed (${r.status})`, true)
  else {
    deps.showToast(`Shredded #${shard}:${seq}`)
    await deps.load()
  }
}

export async function decideApprovalAction(id: string, approve: boolean, deps: AppActionDeps): Promise<void> {
  const r = await operatorFetch(
    'POST',
    `/v1/automation/approvals/${id}`,
    { approve },
    approve ? 'Approve pending market deal' : 'Deny pending market deal'
  )
  if (!r) return
  if (!r.ok) {
    const j = (await r.json().catch(() => ({}))) as { error?: string }
    deps.showToast(j.error ?? `Decision rejected (${r.status})`, true)
  } else {
    deps.showToast(approve ? 'Deal approved ✓' : 'Deal denied')
    await deps.load()
  }
}

export async function shareUrlAction(url: string, showToast: (msg: string, isErr?: boolean) => void): Promise<void> {
  await navigator.clipboard.writeText(url).catch(() => {})
  showToast('Verification link copied ✓')
}

export function buildAppCommands(ctx: {
  s: State | null
  focusBuy: () => void
  focusSell: () => void
  setTab: (t: NavTab) => void
  openAbout: () => void
  openDock: () => void
  openCapsulePicker: () => void
  openMemoryRepo: (repo: string) => void
  runQuickRecall: () => void
  requestFaucet: () => void
  approveStarterPolicy: () => void
  shareUrl: (url: string) => void
}): AppCommandItem[] {
  const { s } = ctx
  return [
    { id: 'buy', label: 'Buy verified data', hint: 'Market · Left rail', run: ctx.focusBuy },
    { id: 'sell', label: 'Anchor & sell knowledge', hint: 'Market · Right rail', run: ctx.focusSell },
    { id: 'home', label: 'Go to Market', hint: 'Home · Live Pulse', run: () => ctx.setTab('home') },
    { id: 'agents', label: 'Go to Agents', hint: 'Fleet & What They Know', run: () => ctx.setTab('agents') },
    { id: 'hubs', label: 'Go to Hubs', hint: 'LightBSV Overlay Mesh', run: () => ctx.setTab('hubs') },
    { id: 'wallet', label: 'Go to Wallet', hint: 'Balance & Ledger', run: () => ctx.setTab('wallet') },
    { id: 'settings', label: 'Go to Settings', hint: 'Policies · Hubs · System', run: () => ctx.setTab('settings') },
    { id: 'about', label: 'About Nektar', hint: 'Settings · System', run: ctx.openAbout },
    { id: 'dock', label: 'Dock AI agent (Claude / Cursor / Proxy / MCP)', hint: 'Agents', run: ctx.openDock },
    { id: 'inspect-nkt', label: 'Open / verify .nkt capsule…', hint: 'Offline Script VM', run: ctx.openCapsulePicker },
    ...(s?.repos ?? []).map((r) => ({
      id: `repo:${r.repo}`,
      label: `Jump to Memory Repo: ${r.repo}`,
      hint: `${r.commitCount} commits · ${r.branch || 'main'}`,
      run: () => ctx.openMemoryRepo(r.repo)
    })),
    { id: 'recall', label: 'Run verified recall + receipt', hint: 'Grounding check', run: ctx.runQuickRecall },
    ...(s?.network === 'testnet'
      ? [{ id: 'faucet', label: 'Request Testnet Faucet sats', hint: 'Free test BSV', run: ctx.requestFaucet }]
      : []),
    { id: 'policy', label: 'Approve Starter Trading Policy', hint: 'Buyer + Seller', run: ctx.approveStarterPolicy },
    ...(s?.share.enabled
      ? [{ id: 'share', label: "Copy agent's public record link", hint: s.share.url, run: () => ctx.shareUrl(s.share.url) }]
      : [])
  ]
}

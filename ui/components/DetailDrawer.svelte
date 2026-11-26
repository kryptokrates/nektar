<script lang="ts">
  import type { DrawerTarget } from '../lib/types'
  import { postJSON } from '../lib/api'
  import { fmtSats, shortHash } from '../lib/format'

  interface Props {
    target: DrawerTarget
    onClose: () => void
  }

  let { target, onClose }: Props = $props()

  let loading = $state(false)
  let detail = $state<Record<string, unknown> | null>(null)
  let err = $state('')

  $effect(() => {
    detail = null
    err = ''
    if (!target) return
    if (target.kind === 'memory') {
      const m = target.item
      loading = true
      void postJSON('/v1/tools/prove', { shard: m.shard, seq: m.seq })
        .then((res) => {
          if (res.ok) detail = res.data
          else err = res.data.error ?? `Prove returned ${res.status}`
        })
        .catch((e) => (err = String(e)))
        .finally(() => (loading = false))
    } else if (target.kind === 'receipt') {
      const url = target.item.url
      loading = true
      void fetch(url, { headers: { accept: 'application/json' } })
        .then(async (r) => {
          const j = (await r.json().catch(() => ({}))) as Record<string, unknown>
          if (r.ok) detail = j
          else err = String(j.error ?? `Verify returned ${r.status}`)
        })
        .catch((e) => (err = String(e)))
        .finally(() => (loading = false))
    }
  })

  function onKey(e: KeyboardEvent) {
    if (e.key === 'Escape' && target) onClose()
  }
</script>

<svelte:window onkeydown={onKey} />

{#if target}
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="backdrop" onclick={onClose}>
    <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
    <aside class="drawer" onclick={(e) => e.stopPropagation()} aria-label="Details">
      <div class="dhead">
        <b>
          {#if target.kind === 'memory'}Memory #{target.item.shard}:{target.item.seq}
          {:else if target.kind === 'receipt'}Receipt Verification
          {:else if target.kind === 'deal'}Market Deal
          {:else}Peer Details{/if}
        </b>
        <button class="x" onclick={onClose} aria-label="Close">✕</button>
      </div>

      <div class="dbody">
        {#if target.kind === 'memory'}
          {@const m = target.item}
          <p class="quote">{m.text}</p>
          <div class="kv">
            <span>HDP</span><span class="mono">{shortHash(m.hdp, 14, 10)}</span>
            <span>Clearance</span><span>Level {m.level}</span>
            <span>Anchor</span><span>{m.batchN >= 0 ? `Batch #${m.batchN}` : 'Unanchored (local)'}</span>
            {#if m.anchorTxid}<span>TxID</span><span class="mono">{shortHash(m.anchorTxid, 14, 10)}</span>{/if}
          </div>
          {#if loading}
            <p class="mut">Verifying SPV Merkle proof…</p>
          {:else if detail}
            {@const vr = (detail.verify ?? {}) as { ok?: boolean; settled?: boolean; reason?: string; detail?: string }}
            <div class="verdict" class:ok={vr.ok} class:warn={!vr.ok}>
              <b>{vr.ok ? (vr.settled ? 'SPV Mined ✓' : 'Anchored (Pending Block)') : (vr.reason ?? 'Local Only')}</b>
              {#if vr.detail}<p>{vr.detail}</p>{/if}
            </div>
            <details class="rawbox"><summary>ProofBundle JSON</summary><pre>{JSON.stringify(detail, null, 2)}</pre></details>
          {:else if err}
            <p class="mut">{err}</p>
          {/if}
        {:else if target.kind === 'receipt'}
          {@const r = target.item}
          {#if r.answer}<p class="quote">{r.answer}</p>{/if}
          {#if loading}
            <p class="mut">Checking receipt signature, delegations &amp; SPV citations…</p>
          {:else if detail}
            {@const verdict = String(detail.verdict ?? 'amber')}
            <div class="verdict" class:ok={verdict === 'green'} class:warn={verdict === 'amber'} class:bad={verdict === 'red'}>
              <b>Verdict: {verdict.toUpperCase()}</b>
              <p>{String(detail.reason ?? '')}</p>
            </div>
            <div class="kv">
              <span>Citations</span><span>{String(detail.citationsChecked ?? 0)} checked · {String(detail.citationsMined ?? 0)} mined</span>
              <span>Token</span><span class="mono">{shortHash(String(detail.receiptToken ?? ''), 16, 8)}</span>
              <span>Link</span><a href={r.url} target="_blank" rel="noopener">Open verifier page ↗</a>
            </div>
            {#if detail.truthNote}<p class="note">{String(detail.truthNote)}</p>{/if}
            <details class="rawbox"><summary>Receipt JSON</summary><pre>{JSON.stringify(detail, null, 2)}</pre></details>
          {:else if err}
            <p class="badtxt">{err}</p>
          {/if}
        {:else if target.kind === 'deal'}
          {@const d = target.item}
          {#if d.deliveredText}<p class="quote">{d.deliveredText}</p>{/if}
          <div class="kv">
            <span>Deal ID</span><span class="mono">{d.op}</span>
            {#if d.role}<span>Side</span><span>{d.role === 'seller' ? 'Sell' : 'Buy'}</span>{/if}
            {#if d.shard}<span>Shard</span><span>#{d.shard}</span>{/if}
            {#if d.priceSats}<span>Amount</span><span>{fmtSats(d.priceSats)} sats</span>{/if}
            <span>Status</span><span>{d.to}</span>
            {#if d.paymentTxid}<span>TxID</span><span class="mono">{shortHash(d.paymentTxid, 14, 10)}</span>{/if}
            <span>Detail</span><span>{d.cause}</span>
          </div>
          {#if d.matchAudit}
            <div class="verdict ok">
              <b>Jev Semantic Match Audit</b>
              <p>Mode: {d.matchAudit.mode ?? 'gated'} · Action: {d.matchAudit.action ?? '—'}</p>
            </div>
            <pre class="pre">{JSON.stringify(d.matchAudit, null, 2)}</pre>
          {/if}
        {:else}
          {@const p = target.item}
          <div class="kv">
            <span>Handle</span><span>{p.handle || '—'}</span>
            <span>Pubkey</span><span class="mono">{shortHash(p.pub, 14, 10)}</span>
            <span>Transport</span><span>{p.publicUrl || p.bboxOffice || 'NAT bbox'}</span>
            <span>Domains</span><span>{(p.sellDomains ?? []).join(', ') || '—'}</span>
          </div>
        {/if}
      </div>
    </aside>
  </div>
{/if}

<style>
  .backdrop {
    position: fixed; inset: 0; z-index: 60;
    background: rgb(0 0 0 / 55%); backdrop-filter: blur(4px);
    display: flex; justify-content: flex-end;
  }
  .drawer {
    width: min(440px, 100%); height: 100vh;
    background: #100e0d; border-left: 1px solid var(--line2);
    display: flex; flex-direction: column;
    box-shadow: -12px 0 40px rgb(0 0 0 / 60%);
  }
  .dhead {
    display: flex; align-items: center; justify-content: space-between;
    padding: 1rem 1.2rem; border-bottom: 1px solid var(--line); font-size: 0.95rem;
  }
  .x { background: none; border: 0; color: var(--mut); cursor: pointer; font-size: 1rem; }
  .x:hover { color: var(--ink); }
  .dbody { padding: 1.1rem 1.2rem; overflow-y: auto; display: grid; gap: 1rem; align-content: start; }
  .quote {
    margin: 0; padding: 0.85rem 1rem; background: var(--surf); border: 1px solid var(--line);
    border-radius: 12px; font-size: 0.9rem; line-height: 1.5; white-space: pre-wrap;
  }
  .kv { display: grid; grid-template-columns: 95px minmax(0, 1fr); gap: 0.45rem 0.75rem; font-size: 0.82rem; }
  .kv > span:nth-child(odd) { color: var(--mut); }
  .kv a { color: var(--amber); text-decoration: none; }
  .verdict {
    padding: 0.8rem 0.95rem; border-radius: 10px; border: 1px solid var(--line); background: var(--surf); font-size: 0.82rem;
  }
  .verdict p { margin: 0.25rem 0 0; color: var(--mut); }
  .verdict.ok { border-color: rgb(76 194 107 / 35%); }
  .verdict.ok b { color: var(--ok); }
  .verdict.warn { border-color: rgb(247 153 27 / 35%); }
  .verdict.warn b { color: var(--amber); }
  .verdict.bad { border-color: rgb(229 83 75 / 45%); }
  .verdict.bad b { color: var(--bad); }
  .note { margin: 0; font-size: 0.76rem; color: var(--mut); line-height: 1.45; }
  .rawbox summary { cursor: pointer; font-size: 0.78rem; color: var(--mut); }
  .rawbox pre, .pre {
    margin: 0.5rem 0 0; background: var(--bg); border: 1px solid var(--line);
    border-radius: 8px; padding: 0.7rem; font-size: 0.72rem; overflow: auto; max-height: 280px;
  }
  .mut { color: var(--mut); font-size: 0.82rem; margin: 0; }
  .badtxt { color: var(--bad); font-size: 0.82rem; margin: 0; }
</style>

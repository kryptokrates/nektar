<script lang="ts">
  import type { PendingApproval } from '../lib/types'
  import { fmtCompactSats } from '../lib/format'

  interface Props {
    approvals: PendingApproval[]
    onDecideApproval: (id: string, approve: boolean) => void
  }

  let { approvals, onDecideApproval }: Props = $props()
</script>

{#if approvals.length > 0}
  <section class="approvals" aria-label="Needs approval">
    {#each approvals as a (a.id)}
      <div class="appr-row">
        <span class="appr-dot">!</span>
        <span class="appr-txt">{a.reason} · ≤ {fmtCompactSats(a.maxExtraSats)}s</span>
        <div class="appr-acts">
          <button class="btn-ok" onclick={() => onDecideApproval(a.id, true)}>Approve</button>
          <button class="btn-ghost" onclick={() => onDecideApproval(a.id, false)}>Deny</button>
        </div>
      </div>
    {/each}
  </section>
{/if}

<style>
  .approvals {
    background: rgb(247 153 27 / 7%);
    border-bottom: 1px solid rgb(247 153 27 / 26%);
    padding: 0.55rem 1.25rem;
    display: flex;
    flex-direction: column;
    gap: 0.45rem;
  }
  .appr-row {
    display: flex;
    align-items: center;
    gap: 0.65rem;
    font-size: 0.82rem;
  }
  .appr-dot {
    width: 18px;
    height: 18px;
    border-radius: 50%;
    background: var(--amber);
    color: #120e0c;
    display: grid;
    place-items: center;
    font-size: 0.72rem;
    font-weight: 900;
    flex-shrink: 0;
  }
  .appr-txt {
    flex: 1;
    color: var(--ink);
    min-width: 0;
  }
  .appr-acts {
    display: flex;
    gap: 0.4rem;
  }
  .btn-ok {
    background: var(--amber);
    color: #120e0c;
    border: 0;
    border-radius: 999px;
    padding: 0.28rem 0.78rem;
    font: inherit;
    font-size: 0.75rem;
    font-weight: 800;
    cursor: pointer;
  }
  .btn-ghost {
    background: transparent;
    color: var(--mut);
    border: 1px solid var(--line2);
    border-radius: 999px;
    padding: 0.28rem 0.72rem;
    font: inherit;
    font-size: 0.75rem;
    cursor: pointer;
  }
</style>

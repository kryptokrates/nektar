<script lang="ts">
  interface Props {
    hasResult: boolean
    loading: boolean
    verified: boolean
    busy: boolean
    repo: string
    localExists: boolean
    forkMode?: boolean
    forkName?: string
    onClose: () => void
    onImport: (targetRepo: string, mergeIntoMain: boolean) => void
  }

  let {
    hasResult,
    loading,
    verified,
    busy,
    repo,
    localExists,
    forkMode = $bindable(false),
    forkName = $bindable(''),
    onClose,
    onImport
  }: Props = $props()
</script>

{#if hasResult}
  <!-- 4. Clean Action Bar (Fail-Closed when unverified) -->
  <footer class="foot">
    {#if forkMode}
      <form
        class="fork-form"
        onsubmit={(e) => {
          e.preventDefault()
          onImport(forkName, true)
        }}
      >
        <input bind:value={forkName} placeholder="new-repo-name" maxlength="64" aria-label="Fork repo name" />
        <button type="submit" class="btn primary" disabled={!verified || !forkName.trim() || busy}>
          {busy ? 'Importing…' : 'Create Fork'}
        </button>
        <button type="button" class="btn" onclick={() => (forkMode = false)}>Cancel</button>
      </form>
    {:else}
      <div class="foot-note">
        {#if !verified}
          Import disabled — offline verification failed.
        {:else}
          Read-only preview · nothing saved locally yet.
        {/if}
      </div>
      <div class="foot-acts">
        <button class="btn" onclick={onClose}>Close</button>
        <button class="btn" onclick={() => (forkMode = true)} disabled={!verified || busy}>
          Fork as New Repo
        </button>
        <button
          class="btn primary"
          onclick={() => onImport(repo || 'imported', true)}
          disabled={!verified || busy}
        >
          {busy ? 'Importing…' : localExists ? `Merge into ${repo}` : `Import ${repo}`}
        </button>
      </div>
    {/if}
  </footer>
{:else if !loading}
  <footer class="foot">
    <div class="foot-note">File could not be parsed as a valid NKTCAP1 capsule.</div>
    <div class="foot-acts">
      <button class="btn" onclick={onClose}>Close</button>
    </div>
  </footer>
{/if}

<style>
  .foot {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    padding: 12px 18px;
    border-top: 1px solid #1a1714;
    background: #090807;
  }
  .foot-note {
    font-size: 11.5px;
    color: #706860;
  }
  .foot-acts {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-shrink: 0;
  }
  .fork-form {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
  }
  .fork-form input {
    flex: 1;
    background: #12100e;
    border: 1px solid #2b2621;
    border-radius: 6px;
    padding: 6px 10px;
    color: #f4f0eb;
    font: 500 12px ui-monospace, monospace;
    outline: none;
  }
  .fork-form input:focus {
    border-color: #5a4630;
  }

  .btn {
    height: 30px;
    padding: 0 12px;
    border-radius: 6px;
    border: 1px solid #27231f;
    background: #141210;
    color: #d6cfc7;
    font: 500 12px system-ui, sans-serif;
    cursor: pointer;
  }
  .btn:hover:not(:disabled) {
    border-color: #3a342e;
    color: #f4f0eb;
  }
  .btn.primary {
    border-color: transparent;
    background: #f7991b;
    color: #120e0c;
    font-weight: 700;
  }
  .btn.primary:hover:not(:disabled) {
    filter: brightness(1.08);
  }
  .btn:disabled {
    opacity: 0.35;
    cursor: default;
  }
</style>

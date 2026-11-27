<script lang="ts">
  interface Props {
    onUnlocked: () => void
  }
  let { onUnlocked }: Props = $props()

  let token = $state('')
  let busy = $state(false)
  let err = $state('')

  async function submit(e?: Event) {
    e?.preventDefault()
    if (!token.trim()) return
    busy = true
    err = ''
    try {
      const r = await fetch('/v1/auth/session', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ token: token.trim() })
      })
      const j = (await r.json().catch(() => ({}))) as { error?: string }
      if (!r.ok) {
        err = j.error ?? `invalid token (${r.status})`
        return
      }
      token = ''
      onUnlocked()
    } catch (e2) {
      err = String(e2)
    } finally {
      busy = false
    }
  }
</script>

<div class="gate-wrap">
  <form class="gate" onsubmit={submit}>
    <div class="gbrand">
      <svg viewBox="0 0 400 400" width="34" height="34" style="border-radius:7px">
        <rect width="400" height="400" fill="#221e1d" rx="60" />
        <path
          fill="#f7991b"
          d="M 195 55 C 178 55 164 63 157 80 C 153 100 149 112 143 124 C 136 139 115 158 93 186 C 80 206 73 226 72 250 C 71 277 79 306 98 329 C 117 352 150 372 190 377 C 232 380 255 356 259 322 C 261 296 274 273 296 251 C 310 234 318 210 318 186 C 318 158 312 137 302 120 C 292 102 276 84 260 74 C 245 64 218 55 195 55 Z"
        />
        <circle cx="307" cy="78" r="38" fill="#f7991b" />
        <ellipse cx="163" cy="190" rx="20" ry="21" fill="#221e1d" />
        <ellipse cx="231" cy="190" rx="20" ry="21" fill="#221e1d" />
        <path d="M 150 265.5 C 161 279 176 287 196 288 C 216 287 230 278 243 265" fill="none" stroke="#221e1d" stroke-width="24" stroke-linecap="round" />
      </svg>
      <div>
        <h1>Unlock nektar</h1>
        <p class="gtag">One shared, verifiable brain for all your AI agents — encrypted, Git-versioned memory &amp; BSV knowledge market.</p>
        <p class="gmut">Enter your HTTP or operator token, or run <code>nektar open</code> for one-click pairing.</p>
      </div>
    </div>
    <input
      class="gin"
      type="password"
      bind:value={token}
      placeholder="NEKTAR_HTTP_TOKEN or NEKTAR_OPERATOR_TOKEN"
      autocomplete="current-password"
      disabled={busy}
    />
    {#if err}<p class="gerr">{err}</p>{/if}
    <button class="gbtn" type="submit" disabled={busy || !token.trim()}>{busy ? 'verifying…' : 'Open Dashboard'}</button>
    <p class="ghint">Tip: run <code>nektar open</code> for one-click pairing, <code>nektar status</code> for local info, or <code>docker logs nektar</code> inside Docker.</p>
  </form>
</div>

<style>
  .gate-wrap {
    display: flex;
    min-height: 100vh;
    align-items: center;
    justify-content: center;
    padding: 1.5rem;
  }
  .gate {
    background: #11100e;
    border: 1px solid #24201d;
    border-radius: 16px;
    padding: 1.8rem;
    max-width: 420px;
    width: 100%;
    box-shadow: 0 24px 60px rgb(0 0 0 / 65%);
    display: grid;
    gap: 1rem;
  }
  .gbrand {
    display: flex;
    gap: 0.9rem;
    align-items: center;
  }
  h1 {
    margin: 0;
    font-size: 1.25rem;
    color: #ece7e4;
  }
  .gtag {
    margin: 0.25rem 0 0;
    font-size: 0.83rem;
    color: #d6ccc3;
    line-height: 1.4;
  }
  .gmut,
  .ghint {
    margin: 0.32rem 0 0;
    font-size: 0.8rem;
    color: #a89a8e;
    line-height: 1.45;
  }
  .gin {
    width: 100%;
    padding: 0.7rem 0.9rem;
    border-radius: 10px;
    border: 1px solid #3a2f28;
    background: #120e0c;
    color: #ece7e4;
    font-family: ui-monospace, monospace;
    font-size: 0.85rem;
  }
  .gin:focus {
    outline: 1px solid #f7991b;
    border-color: #f7991b;
  }
  .gbtn {
    padding: 0.75rem 1rem;
    border: 0;
    border-radius: 10px;
    background: #f7991b;
    color: #221e1d;
    font-weight: 700;
    font-size: 0.95rem;
    cursor: pointer;
  }
  .gbtn:disabled {
    opacity: 0.55;
    cursor: default;
  }
  .gerr {
    margin: 0;
    color: #e5534b;
    font-size: 0.82rem;
  }
  code {
    background: #2e2620;
    padding: 1px 5px;
    border-radius: 4px;
    font-size: 0.78rem;
  }
</style>

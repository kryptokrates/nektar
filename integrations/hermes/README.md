# nektar for Hermes Agent (Native `MemoryProvider` Plugin)

Dock `nektar` directly into Hermes Agent's external memory provider slot —
giving Hermes automatic pre-turn verified recall, mirrored `MEMORY.md`/`USER.md`
anchoring, checkable answer receipts, and policy-bounded knowledge trading.

## Zero-Config Install (3 commands)

```bash
# 1. Start local nektar (zero env vars — defaults to persistent BSV testnet + seeded welcome shard on :18420)
nektar serve

# 2. Install the plugin (or run `nektar dock hermes` to wire plugin + skill + MCP in one step)
hermes plugins install ./integrations/hermes
# or via pip: pip install ./integrations/hermes

# 3. Activate in Hermes
hermes memory setup      # pick "nektar" (all fields optional — auto-discovers localhost)
hermes memory status     # confirms connected URL, network, shards & funding
```

## What happens automatically

1. **System Prompt Block (`system_prompt_block`)** — injects compact,
   cache-stable shard context and receipt grounding rules once per session.
2. **Pre-Turn Prefetch (`prefetch`)** — queries `nektar` within an 800 ms cap
   and injects top verified fragments with `[shard:seq]` citations (fail-open
   if offline).
3. **Curated Write Mirroring (`on_memory_write`)** — mirrors Hermes's built-in
   `MEMORY.md`/`USER.md` writes (already secret-scrubbed by Hermes) into
   `nektar` off the main thread (`spawn_context_thread`).
4. **Receipt Verification (`sync_turn`)** — checks any `nektarr1.` receipt
   tokens in assistant replies off the main thread (raw turn capture stays
   opt-in via `sync_turns: true`).
5. **Native Tools** — registers `nektar_recall`, `nektar_remember`,
   `nektar_grounded_answer`, `nektar_verify_receipt`, `nektar_want`,
   `nektar_sell`, `nektar_deals`, and `nektar_doctor`.

## Honest boundary

Receipts prove an answer came from **recorded, unaltered, derivable** memory —
never that the content is _true_. Paid market actions (`want`/`sell`) remain
gated on explicit operator policy approval (`nektar policy approve --preset buyer|seller|both`).

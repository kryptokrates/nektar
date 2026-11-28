# Nektar Deterministic Video Engine Contract (`videos/shared/`)

Zero-dependency Node 24 + Headless Chrome CDP + FFmpeg 8.1 pipeline matching `site/demo.mp4`'s dark-obsidian & amber-emerald brand aesthetic. Safe for 5+ parallel workers (each render uses `--remote-debugging-port=0` and an isolated temp profile).

> **Windows Host Rules:**
> 1. Always run commands with `BypassSandbox: true` (standard sandbox fails with `CreateProcessSecurityEnvironment: Zugriff verweigert`).
> 2. Windows PowerShell 5.1 does **not** support `&&` — put commands on separate lines and check `$LASTEXITCODE`.

---

## 1. Folder Structure (Zero Collisions)
```
videos/
  shared/                  # READ-ONLY for Phase 2 workers
    logo.svg               # Official smiling Nektar emblem (copied from site/logo.svg)
    theme.css              # Shared brand tokens, 1920x1080 stage, cards, pills, outro
    tts.mjs                # Voiceover + ambient bed + micro-SFX -> timings.json + audio-mix.wav
    render.mjs             # Headless Chrome -> FFmpeg H.264/AAC + ffprobe verify + 4x4 contact sheet
  01-<slug>/               # Owned by 1 worker
    script.json            # Narration + scene timing/SFX config
    scene.html             # Self-contained deterministic 1920x1080 visual renderer
  out/
    01-<slug>.mp4          # Final 1920x1080 30fps yuv420p AAC 48kHz -16 LUFS video
    01-<slug>-contact.jpg  # Auto-generated 4x4 review contact sheet
```

---

## 2. `script.json` Schema
```json
{
  "title": "Nektar — Example Video",
  "voice": "en-US-AndrewNeural",
  "rate": "+5%",
  "fps": 30,
  "bedVolume": 0.055,
  "autoTransitions": true,
  "scenes": [
    {
      "id": "hook",
      "padBefore": 0.25,
      "padAfter": 0.30,
      "minDuration": 4.0,
      "vo": "Spoken narration for this scene.",
      "subtitle": "Optional tighter X muted-autoplay caption (defaults to vo).",
      "sfx": [
        { "at": 0.15, "type": "slam" },
        { "at": 2.4, "type": "chime", "gain": 0.9 }
      ]
    }
  ]
}
```
- **Target length:** ~55–65 seconds total (~120–150 words across 6–7 scenes at `rate: "+5%"`).
- **Silent scene:** omit or set `"vo": ""` and provide `"duration": 3.5` (or `"minDuration"`).
- **SFX `at`:** seconds relative to scene `start` (negative values count backward from scene `end`).
- **Available SFX types:** `whoosh`, `pop`, `tick`, `chime`, `unlock`, `error`, `slam`.

---

## 3. Generated `timings.json` Fields
Running `tts.mjs` writes `<sceneDir>/timings.json`:
```json
{
  "title": "...",
  "fps": 30,
  "totalDuration": 60.0,
  "totalFrames": 1800,
  "ttsEngine": "edge-tts",
  "voice": "en-US-AndrewNeural",
  "scenes": [
    { "index": 0, "id": "hook", "start": 0, "end": 8.4, "duration": 8.4, "voStart": 0.25, "voEnd": 8.1, "vo": "...", "subtitle": "...", "sfx": [] }
  ]
}
```

---

## 4. `scene.html` Contract
Link `../shared/theme.css` and `../shared/logo.svg`, and define global `window.renderFrame(t, frame, totalFrames, timings)`:
- `t`: exact timestamp in seconds (`frame / fps`, starting at `0`).
- **MUST be 100% deterministic:** drive every `opacity`, `transform`, `width`, SVG `stroke-dashoffset`, and counter purely from `t` (never `Date.now()`, `Math.random()`, `setTimeout`, or CSS `transition`/`@keyframes`).
- Optional `window.initScene(timings)` runs once before frame 0 (e.g. to build DOM nodes).
- Update `#subtitle-bar` text from the active scene's `subtitle` and `#progress-fill` width to `(t / timings.totalDuration) * 100 + '%'`.
- **Honesty Rule (`AGENTS.md`):** Never claim proofs establish real-world truth or zkML model execution; include a quiet `.truth-note` on the Cinema Outro (`Proofs verify recorded + unaltered + derivable — never truth. Model identity is declared.`).

---

## 5. Key `theme.css` Classes
- **Stage:** `.stage`, `.stage-grid`, `.corner-brand`, `.corner-scene-tag`, `.scene`, `.scene-head`
- **Typography:** `.kicker` (`.ok`, `.bad`), `.headline`, `.headline-grad`, `.headline-amber-green`, `.headline-green-amber`, `.subhead`, `.mono`
- **Surfaces:** `.card` (`.card-glass`, `.card-amber`, `.card-ok`, `.card-bad`, `.card-dim`), `.card-label` (`.amber`, `.ok`, `.bad`), `.mac-window` / `.mac-bar` / `.mac-dots.color`, `.terminal` (`.prompt`, `.cmd`, `.arg`, `.dim`, `.ok`, `.bad`, `.cursor`)
- **Indicators:** `.pill` (`.pill-amber`, `.pill-ok`, `.pill-bad`, `.pill-solid-ok`, `.pill-solid-amber`, `.pill-solid-ink`), `.metric-box` / `.metric-val` / `.metric-label`, `.toggle` / `.toggle.on`
- **Overlay & Outro:** `.svg-layer`, `.glow-amber`, `.glow-ok`, `.glow-bad`, `.subtitle-wrap` / `.subtitle-bar`, `.progress-track` / `.progress-fill`, `.outro`, `.outro-brand`, `.outro-emblem`, `.outro-wordmark`, `.outro-tagline`, `.outro-pills`, `.truth-note`

---

## 6. Exact CLI Commands
```powershell
# 1. Generate VO, ambient bed, SFX, timings.json, and audio-mix.wav
node videos/shared/tts.mjs videos/01-<slug>/script.json

# 2. Fast visual QC (1 mid-frame PNG per scene in videos/01-<slug>/preview/)
node videos/shared/render.mjs videos/01-<slug> --preview

# 3. Full 1080p30 render + ffprobe verification + 4x4 contact sheet
node videos/shared/render.mjs videos/01-<slug> videos/out/01-<slug>.mp4
```

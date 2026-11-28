#!/usr/bin/env node
/**
 * Nektar Deterministic TTS + Procedural Audio Mixer
 * Usage: node videos/shared/tts.mjs <sceneDir>/script.json
 *
 * Outputs in <sceneDir>:
 *   - timings.json   (exact scene start/end/voStart/voEnd + fps + totalFrames)
 *   - audio-mix.wav  (48kHz stereo 16-bit PCM: VO + ducked dark ambient bed + UI SFX)
 */

import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { execFileSync } from 'node:child_process';

const SAMPLE_RATE = 48000;
const UVX_BIN = fs.existsSync('C:\\Users\\nedzo\\.local\\bin\\uvx.exe')
  ? 'C:\\Users\\nedzo\\.local\\bin\\uvx.exe'
  : 'uvx';

function die(msg) {
  console.error(`[tts.mjs] ERROR: ${msg}`);
  process.exit(1);
}

const argPath = process.argv[2];
if (!argPath) die('Usage: node videos/shared/tts.mjs <sceneDir>/script.json');

let scriptPath = path.resolve(argPath);
if (fs.existsSync(scriptPath) && fs.statSync(scriptPath).isDirectory()) {
  scriptPath = path.join(scriptPath, 'script.json');
}
if (!fs.existsSync(scriptPath)) die(`script.json not found: ${scriptPath}`);

const sceneDir = path.dirname(scriptPath);
const cacheDir = path.join(sceneDir, '.tts-cache');
fs.mkdirSync(cacheDir, { recursive: true });

const script = JSON.parse(fs.readFileSync(scriptPath, 'utf8'));
const fps = Number(script.fps || 30);
const voice = String(script.voice || 'en-US-AndrewNeural');
const rate = String(script.rate || '+5%');
const bedVolume = script.bedVolume !== undefined ? Number(script.bedVolume) : 0.055;
const autoTransitions = script.autoTransitions !== false;

if (!Array.isArray(script.scenes) || script.scenes.length === 0) {
  die('script.json must contain a non-empty "scenes" array.');
}

// ---------- TTS Synthesis (uvx edge-tts with Windows SAPI fallback) ----------

function hashKey(parts) {
  return crypto.createHash('sha1').update(parts.join('|')).digest('hex').slice(0, 12);
}

function synthWithEdgeTTS(text, outMp3) {
  execFileSync(
    UVX_BIN,
    ['edge-tts', '--voice', voice, `--rate=${rate}`, '--text', text, '--write-media', outMp3],
    { stdio: ['ignore', 'pipe', 'pipe'], timeout: 60000 }
  );
}

function synthWithSapiFallback(text, outWav) {
  const safeText = text.replace(/'/g, "''");
  const safeOut = outWav.replace(/'/g, "''");
  const ps = [
    'Add-Type -AssemblyName System.Speech',
    '$s = New-Object System.Speech.Synthesis.SpeechSynthesizer',
    '$s.Rate = 1',
    `$s.SetOutputToWaveFile('${safeOut}')`,
    `$s.Speak('${safeText}')`,
    '$s.Dispose()'
  ].join('; ');
  execFileSync('powershell.exe', ['-NoProfile', '-Command', ps], {
    stdio: ['ignore', 'pipe', 'pipe'],
    timeout: 60000
  });
}

function decodeToTrimmedMonoF32(inAudioPath, outWavPath) {
  // Trim excess leading/trailing silence cleanly and normalize to 48kHz mono s16le WAV
  execFileSync(
    'ffmpeg',
    [
      '-y', '-v', 'error',
      '-i', inAudioPath,
      '-ac', '1', '-ar', String(SAMPLE_RATE),
      '-af',
      'silenceremove=start_periods=1:start_duration=0.02:start_threshold=-46dB,areverse,silenceremove=start_periods=1:start_duration=0.03:start_threshold=-46dB,areverse',
      '-c:a', 'pcm_s16le',
      outWavPath
    ],
    { stdio: ['ignore', 'pipe', 'pipe'] }
  );
}

function probeDuration(filePath) {
  const out = execFileSync(
    'ffprobe',
    ['-v', 'error', '-show_entries', 'format=duration', '-of', 'default=noprint_wrappers=1:nokey=1', filePath],
    { encoding: 'utf8' }
  ).trim();
  const d = parseFloat(out);
  if (!Number.isFinite(d) || d <= 0) die(`Invalid audio duration from ffprobe for ${filePath}: "${out}"`);
  return d;
}

function readMonoWavS16(wavPath) {
  const buf = fs.readFileSync(wavPath);
  // Find 'data' chunk robustly (handles arbitrary metadata chunks before data)
  let offset = 12;
  while (offset + 8 <= buf.length) {
    const id = buf.toString('ascii', offset, offset + 4);
    const size = buf.readUInt32LE(offset + 4);
    if (id === 'data') {
      const count = Math.floor(size / 2);
      const out = new Float32Array(count);
      for (let i = 0; i < count; i++) {
        out[i] = buf.readInt16LE(offset + 8 + i * 2) / 32768;
      }
      return out;
    }
    offset += 8 + size + (size & 1);
  }
  die(`No data chunk found in WAV: ${wavPath}`);
}

function writeStereoWavS16(outPath, left, right) {
  const n = left.length;
  const dataBytes = n * 4;
  const buf = Buffer.alloc(44 + dataBytes);
  buf.write('RIFF', 0, 'ascii');
  buf.writeUInt32LE(36 + dataBytes, 4);
  buf.write('WAVE', 8, 'ascii');
  buf.write('fmt ', 12, 'ascii');
  buf.writeUInt32LE(16, 16);        // PCM chunk size
  buf.writeUInt16LE(1, 20);         // PCM format
  buf.writeUInt16LE(2, 22);         // Stereo
  buf.writeUInt32LE(SAMPLE_RATE, 24);
  buf.writeUInt32LE(SAMPLE_RATE * 4, 28);
  buf.writeUInt16LE(4, 32);         // Block align
  buf.writeUInt16LE(16, 34);        // Bits per sample
  buf.write('data', 36, 'ascii');
  buf.writeUInt32LE(dataBytes, 40);

  let pos = 44;
  for (let i = 0; i < n; i++) {
    const l = Math.max(-0.98, Math.min(0.98, left[i]));
    const r = Math.max(-0.98, Math.min(0.98, right[i]));
    buf.writeInt16LE(Math.round(l * 32767), pos);
    buf.writeInt16LE(Math.round(r * 32767), pos + 2);
    pos += 4;
  }
  fs.writeFileSync(outPath, buf);
}

// ---------- Synthesize all scenes ----------

let ttsEngine = 'edge-tts';
const sceneData = [];
let cursor = 0;

for (let i = 0; i < script.scenes.length; i++) {
  const s = script.scenes[i];
  const id = String(s.id || `scene-${i + 1}`);
  const voText = String(s.vo || '').trim();
  const subtitle = s.subtitle !== undefined ? String(s.subtitle) : voText;
  const padBefore = s.padBefore !== undefined ? Number(s.padBefore) : (i === 0 ? 0.25 : 0.22);
  const padAfter = s.padAfter !== undefined ? Number(s.padAfter) : (i === script.scenes.length - 1 ? 0.85 : 0.30);

  let samples = null;
  let voDur = 0;

  if (voText.length > 0) {
    const key = hashKey([voice, rate, voText]);
    const rawMp3 = path.join(cacheDir, `${id}-${key}.mp3`);
    const trimWav = path.join(cacheDir, `${id}-${key}.wav`);

    if (!fs.existsSync(trimWav)) {
      if (!fs.existsSync(rawMp3)) {
        console.log(`[tts.mjs] Synthesizing scene ${i + 1}/${script.scenes.length} (${id})...`);
        let ok = false;
        for (let attempt = 1; attempt <= 2 && !ok; attempt++) {
          try {
            synthWithEdgeTTS(voText, rawMp3);
            ok = fs.existsSync(rawMp3) && fs.statSync(rawMp3).size > 200;
          } catch (err) {
            if (attempt === 2) {
              console.warn(`[tts.mjs] WARN: edge-tts failed (${err.message.split('\n')[0]}). Falling back to Windows SAPI.`);
              ttsEngine = 'sapi-fallback';
              const sapiWav = path.join(cacheDir, `${id}-${key}-sapi.wav`);
              synthWithSapiFallback(voText, sapiWav);
              decodeToTrimmedMonoF32(sapiWav, trimWav);
              ok = true;
            }
          }
        }
      }
      if (!fs.existsSync(trimWav) && fs.existsSync(rawMp3)) {
        decodeToTrimmedMonoF32(rawMp3, trimWav);
      }
    } else {
      console.log(`[tts.mjs] Cached VO for scene ${i + 1}/${script.scenes.length} (${id})`);
    }

    voDur = probeDuration(trimWav);
    samples = readMonoWavS16(trimWav);

    // Normalize per-clip speech RMS to a consistent foreground level (-16 dBFS nominal before mix)
    let sumSq = 0;
    let peak = 1e-6;
    for (let k = 0; k < samples.length; k++) {
      const v = samples[k];
      sumSq += v * v;
      const a = Math.abs(v);
      if (a > peak) peak = a;
    }
    const rms = Math.sqrt(sumSq / Math.max(1, samples.length));
    const gain = Math.min(0.16 / Math.max(rms, 1e-5), 0.82 / peak);
    for (let k = 0; k < samples.length; k++) samples[k] *= gain;
  }

  const rawDur = voText.length > 0 ? padBefore + voDur + padAfter : Number(s.duration || s.minDuration || 3.0);
  const duration = Math.max(rawDur, Number(s.minDuration || 0));
  const start = cursor;
  const end = start + duration;
  const voStart = start + padBefore;
  const voEnd = voStart + voDur;

  sceneData.push({
    index: i,
    id,
    start: Number(start.toFixed(4)),
    end: Number(end.toFixed(4)),
    duration: Number(duration.toFixed(4)),
    voStart: Number(voStart.toFixed(4)),
    voEnd: Number(voEnd.toFixed(4)),
    vo: voText,
    subtitle,
    sfx: Array.isArray(s.sfx) ? s.sfx : [],
    _samples: samples
  });

  cursor = end;
}

const totalFrames = Math.ceil(cursor * fps);
const totalDuration = Number((totalFrames / fps).toFixed(4));
// Snap last scene end to exact frame-aligned totalDuration
sceneData[sceneData.length - 1].end = totalDuration;
sceneData[sceneData.length - 1].duration = Number((totalDuration - sceneData[sceneData.length - 1].start).toFixed(4));

// ---------- Procedural Dark-Tech Ambient Bed + Micro-SFX ----------

function mulberry32(seed) {
  let a = seed >>> 0;
  return function () {
    a |= 0;
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const rand = mulberry32(0x4e454b54); // 'NEKT'
const totalSamples = Math.ceil(totalDuration * SAMPLE_RATE);
const voTrack = new Float32Array(totalSamples);
const bedL = new Float32Array(totalSamples);
const bedR = new Float32Array(totalSamples);

// Place VO clips
for (const sc of sceneData) {
  if (!sc._samples) continue;
  const startIdx = Math.round(sc.voStart * SAMPLE_RATE);
  const len = Math.min(sc._samples.length, totalSamples - startIdx);
  for (let i = 0; i < len; i++) {
    voTrack[startIdx + i] += sc._samples[i];
  }
}

// Synthesize subtle warm dark-tech ambient bed (D minor / F / C / G progression, low harmonic warmth)
if (bedVolume > 0) {
  const chords = [
    [73.42, 146.83, 174.61, 220.00], // Dm
    [65.41, 130.81, 164.81, 196.00], // C
    [58.27, 116.54, 146.83, 174.61], // Bb
    [65.41, 130.81, 164.81, 220.00]  // Am/C
  ];
  const barDur = 8.0;
  for (let startSec = 0, bar = 0; startSec < totalDuration; startSec += barDur, bar++) {
    const dur = Math.min(barDur + 1.4, totalDuration - startSec);
    const n = Math.floor(dur * SAMPLE_RATE);
    const baseIdx = Math.floor(startSec * SAMPLE_RATE);
    const notes = chords[bar % chords.length];
    for (let i = 0; i < n && baseIdx + i < totalSamples; i++) {
      const q = i / SAMPLE_RATE;
      const env = Math.min(q / 1.4, 1) * Math.min((dur - q) / 1.4, 1);
      let sL = 0;
      let sR = 0;
      for (let j = 0; j < notes.length; j++) {
        const f = notes[j];
        const w = j === 0 ? 0.34 : 0.22;
        const detune = j % 2 === 0 ? 1.0018 : 0.9982;
        const mod = 0.08 * Math.sin(2 * Math.PI * 0.22 * q + j);
        sL += w * Math.sin(2 * Math.PI * f * q + mod);
        sR += w * Math.sin(2 * Math.PI * f * detune * q - mod);
      }
      bedL[baseIdx + i] += sL * env * bedVolume;
      bedR[baseIdx + i] += sR * env * bedVolume;
    }
  }

  // Gentle muffled sub-pulse every 0.625s (96 BPM)
  const beatDur = 60 / 96;
  for (let t = 0; t < totalDuration; t += beatDur) {
    const idx = Math.floor(t * SAMPLE_RATE);
    const n = Math.floor(0.18 * SAMPLE_RATE);
    for (let i = 0; i < n && idx + i < totalSamples; i++) {
      const q = i / SAMPLE_RATE;
      const kick = Math.sin(2 * Math.PI * (52 * q + 1.4 * (1 - Math.exp(-q * 28)))) * Math.exp(-q * 22);
      const v = kick * bedVolume * 0.32;
      bedL[idx + i] += v;
      bedR[idx + i] += v;
    }
  }

  // Global bed fade-in / fade-out
  const fadeIn = Math.floor(0.7 * SAMPLE_RATE);
  const fadeOut = Math.floor(1.2 * SAMPLE_RATE);
  for (let i = 0; i < totalSamples; i++) {
    let g = 1.0;
    if (i < fadeIn) g *= i / Math.max(1, fadeIn);
    if (i > totalSamples - fadeOut) g *= (totalSamples - i) / Math.max(1, fadeOut);
    bedL[i] *= g;
    bedR[i] *= g;
  }
}

// Smooth sidechain ducking of ambient bed under VO
const hop = 960; // 20ms windows
const blocks = Math.ceil(totalSamples / hop);
const duckEnv = new Float32Array(blocks);
for (let b = 0; b < blocks; b++) {
  let e = 0;
  const s0 = b * hop;
  const s1 = Math.min(totalSamples, s0 + hop);
  for (let i = s0; i < s1; i++) e += voTrack[i] * voTrack[i];
  const rms = Math.sqrt(e / Math.max(1, s1 - s0));
  duckEnv[b] = rms > 0.018 ? 0.30 : 1.0;
}
// Smooth ducking envelope (attack/release)
let curDuck = 1.0;
for (let b = 0; b < blocks; b++) {
  const target = duckEnv[b];
  curDuck += (target - curDuck) * (target < curDuck ? 0.35 : 0.08);
  duckEnv[b] = curDuck;
}
for (let i = 0; i < totalSamples; i++) {
  const d = duckEnv[Math.min(blocks - 1, Math.floor(i / hop))];
  bedL[i] *= d;
  bedR[i] *= d;
}

// Add crisp micro-SFX
function addSfx(atSec, type, gainScale = 1.0) {
  const idx = Math.round(atSec * SAMPLE_RATE);
  if (idx < 0 || idx >= totalSamples) return;
  let dur = 0.25;
  if (type === 'whoosh') dur = 0.32;
  else if (type === 'pop') dur = 0.095;
  else if (type === 'tick') dur = 0.032;
  else if (type === 'chime') dur = 0.34;
  else if (type === 'unlock') dur = 0.40;
  else if (type === 'error') dur = 0.24;
  else if (type === 'slam') dur = 0.42;

  const n = Math.floor(dur * SAMPLE_RATE);
  let lp = 0;
  for (let i = 0; i < n && idx + i < totalSamples; i++) {
    const q = i / SAMPLE_RATE;
    const p = q / dur;
    let s = 0;
    if (type === 'whoosh') {
      const white = rand() * 2 - 1;
      const alpha = 0.04 + 0.16 * Math.sin(Math.PI * p);
      lp += alpha * (white - lp);
      s = lp * Math.pow(Math.sin(Math.PI * p), 1.8) * 0.11;
    } else if (type === 'pop') {
      const freq = 520 * Math.exp(-q * 24) + 180;
      s = Math.sin(2 * Math.PI * freq * q) * Math.exp(-q * 36) * 0.14;
    } else if (type === 'tick') {
      const white = rand() * 2 - 1;
      s = (Math.sin(2 * Math.PI * 1900 * q) * 0.65 + white * 0.35) * Math.exp(-q * 140) * 0.09;
    } else if (type === 'chime') {
      const env = Math.min(q / 0.008, 1) * Math.exp(-q * 11);
      s = (Math.sin(2 * Math.PI * 880 * q) + 0.45 * Math.sin(2 * Math.PI * 1318.5 * q) + 0.2 * Math.sin(2 * Math.PI * 1760 * q)) * env * 0.075;
    } else if (type === 'unlock') {
      const env1 = Math.min(q / 0.008, 1) * Math.exp(-q * 12);
      const q2 = Math.max(0, q - 0.09);
      const env2 = q > 0.09 ? Math.min(q2 / 0.008, 1) * Math.exp(-q2 * 10) : 0;
      s = (Math.sin(2 * Math.PI * 587.33 * q) * env1 + Math.sin(2 * Math.PI * 880 * q2) * env2 + 0.35 * Math.sin(2 * Math.PI * 1174.66 * q2) * env2) * 0.08;
    } else if (type === 'error') {
      const env1 = Math.exp(-q * 28);
      const q2 = Math.max(0, q - 0.10);
      const env2 = q > 0.10 ? Math.exp(-q2 * 26) : 0;
      s = (Math.sin(2 * Math.PI * 210 * q) * env1 + Math.sin(2 * Math.PI * 155 * q2) * env2) * 0.14;
    } else if (type === 'slam') {
      const white = rand() * 2 - 1;
      lp += 0.06 * (white - lp);
      const sub = Math.sin(2 * Math.PI * (78 * Math.exp(-q * 9) + 34) * q) * Math.exp(-q * 8.5);
      s = (sub * 0.22 + lp * Math.exp(-q * 11) * 0.09);
    } else {
      s = Math.sin(2 * Math.PI * 740 * q) * Math.exp(-q * 25) * 0.08;
    }
    const v = s * gainScale;
    bedL[idx + i] += v;
    bedR[idx + i] += v;
  }
}

for (let i = 0; i < sceneData.length; i++) {
  const sc = sceneData[i];
  if (autoTransitions && i > 0) {
    addSfx(Math.max(0, sc.start - 0.10), 'whoosh', 0.65);
  }
  for (const cue of sc.sfx) {
    const offset = Number(cue.at || 0);
    const absTime = offset >= 0 ? sc.start + offset : sc.end + offset;
    addSfx(absTime, String(cue.type || 'pop'), cue.gain !== undefined ? Number(cue.gain) : 1.0);
  }
}

// Final stereo mix with soft peak safety
const outL = new Float32Array(totalSamples);
const outR = new Float32Array(totalSamples);
for (let i = 0; i < totalSamples; i++) {
  outL[i] = Math.tanh(voTrack[i] + bedL[i]);
  outR[i] = Math.tanh(voTrack[i] + bedR[i]);
}

const audioMixPath = path.join(sceneDir, 'audio-mix.wav');
writeStereoWavS16(audioMixPath, outL, outR);

const timings = {
  title: script.title || path.basename(sceneDir),
  fps,
  totalDuration,
  totalFrames,
  ttsEngine,
  voice,
  rate,
  scenes: sceneData.map(({ _samples, ...rest }) => rest)
};

const timingsPath = path.join(sceneDir, 'timings.json');
fs.writeFileSync(timingsPath, JSON.stringify(timings, null, 2), 'utf8');

console.log(`[tts.mjs] OK (${ttsEngine}): ${totalDuration.toFixed(2)}s (${totalFrames} frames @ ${fps}fps), ${sceneData.length} scenes`);
console.log(`[tts.mjs] Wrote: ${timingsPath}`);
console.log(`[tts.mjs] Wrote: ${audioMixPath}`);

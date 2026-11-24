// log.ts — the stamped line logger every diagnostic `log:` seam terminates
// at. One call site owns the format: ISO-8601 UTC stamp + verbatim prefix +
// line on stderr. check-arch grep-gates raw console.*/process.std*.write in
// packages/*/src — new code routes through mkLog()/dbg()/writeErr() here.
import { nektarEnv } from './env.js'

export type LogLine = (line: string) => void

export interface LogOpts {
  /** clock seam — tests pass a fixed now() so stamped output is replayable */
  now?: () => number
  /** sink seam — tests capture chunks; production leaves it on stderr */
  write?: (chunk: string) => void
}

/** Raw stderr write — the explicit escape hatch for non-log terminal output
 *  (QR codes, progress UI, CLIs whose output IS the interface). Diagnostic
 *  lines never use this; they go through mkLog/dbg so they're stamped. */
export const writeErr = (chunk: string): void => {
  process.stderr.write(chunk)
}

export const writeOut = (chunk: string): void => {
  process.stdout.write(chunk)
}

/** mkLog(prefix) → (line) => void — ISO-8601 stamp + `prefix:` + line on
 *  stderr, exactly one stamped line per call (a trailing '\n' in the line
 *  is normalized away; pass-through seams emit complete lines). Pass ''
 *  for the bare `stamp line` form when the caller's text already carries
 *  its own tag. */
export const mkLog = (prefix: string, opts: LogOpts = {}): LogLine => {
  const now = opts.now ?? (() => Date.now())
  const write = opts.write ?? writeErr
  const tag = prefix === '' ? '' : `${prefix}: `
  return (line) => write(`${new Date(now()).toISOString()} ${tag}${line.replace(/\n+$/, '')}\n`)
}

/** dbg(flag, line) — NEKTAR_*-gated stamped debug line; flag is the env
 *  name (any non-empty value enables, same convention as the call sites
 *  that used to gate their own console.error). */
export const dbg = (flag: string, line: string): void => {
  if (nektarEnv(flag) === undefined) return
  mkLog('debug')(line)
}

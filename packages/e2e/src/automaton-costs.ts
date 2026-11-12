// AUT-009: measured per-transition cost of a full-mode automaton step.
import type { Transaction } from '@bsv/sdk'

export function automatonCosts(artifact: { script: string }, stepTx: Transaction): { scriptBytes: number; txBytes: number; feeSats: number } {
  const inSats = stepTx.inputs.reduce((s, i) => s + (i.sourceTransaction?.outputs[i.sourceOutputIndex]?.satoshis ?? NaN), 0)
  const outSats = stepTx.outputs.reduce((s, o) => s + (o.satoshis ?? 0), 0)
  return { scriptBytes: artifact.script.length / 2, txBytes: stepTx.toBinary().length, feeSats: inSats - outSats }
}

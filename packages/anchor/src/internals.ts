// RunarContract members the SDK declares `private` that this package calls
// at runtime — the methods exist; upstream just hides them. One named
// structural view instead of an untyped cast at every call site.
import type { Transaction } from '@bsv/sdk'
import type { RunarContract, UTXO } from 'runar-sdk'

export interface ContractInternals {
  /** code-part hex — the caller appends OP_RETURN + serialized state */
  getCodePartHex(): string
  computeOpPushTxWithCodeSep(
    tx: Transaction,
    inputIndex: number,
    scriptHex: string,
    satoshis: number | bigint,
    codeSepIndex: number
  ): { sigHex: string; preimageHex: string }
  buildStatefulPrefix(sigHex: string, includeCodeSep?: boolean): string
  getSubscriptForSigning(scriptHex: string, codeSepIndex: number): string
  /** the UTXO the contract currently sits in — set by deploy/call */
  currentUtxo?: UTXO
}

export const internals = (c: RunarContract): ContractInternals => c as unknown as ContractInternals

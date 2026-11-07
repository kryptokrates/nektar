/**
 * The engine interfaces an application's overlay modules satisfy, declared
 * structurally.
 *
 * A module is loaded into an overlay host by path and never imports the
 * engine package: at runtime the host's engine calls it through these
 * shapes, and at build time it compiles with nothing but the SDK installed.
 * The shapes are copied from @bsv/overlay 2.3.1's declarations
 * (TopicManager, LookupService, LookupFormula, Output, and the SDK's
 * AdmittanceInstructions) with only the members a module uses, so a
 * widening upstream cannot break this build and a narrowing one fails at the
 * host's own type check rather than silently here.
 *
 * ModuleHost and Module are the contract between such a host and a module
 * it loads: the module's default export takes a ModuleHost and returns a
 * Module naming the topic managers and lookup services it mounts.
 */
import type { Script } from '@bsv/sdk'

export interface AdmittanceInstructions {
  outputsToAdmit: number[]
  coinsToRetain: number[]
  coinsRemoved?: number[]
}

export interface TopicManager {
  identifyAdmissibleOutputs: (
    beef: number[],
    previousCoins: number[],
    offChainValues?: number[],
    mode?: 'historical-tx' | 'current-tx' | 'historical-tx-no-spv',
  ) => Promise<AdmittanceInstructions>
  identifyNeededInputs?: (beef: number[], offChainValues?: number[]) => Promise<Array<{ txid: string; outputIndex: number }>>
  getDocumentation: () => Promise<string>
  getMetaData: () => Promise<{
    name: string
    shortDescription: string
    iconURL?: string
    version?: string
    informationURL?: string
  }>
}

export type OutputAdmittedByTopic =
  | {
      mode: 'locking-script'
      txid: string
      outputIndex: number
      topic: string
      satoshis: number
      lockingScript: Script
      offChainValues?: number[]
    }
  | {
      mode: 'whole-tx'
      atomicBEEF: number[]
      outputIndex: number
      topic: string
      offChainValues?: number[]
    }

export type OutputSpent =
  | { mode: 'none'; txid: string; outputIndex: number; topic: string }
  | { mode: 'txid'; txid: string; outputIndex: number; topic: string; spendingTxid: string }
  | {
      mode: 'script'
      txid: string
      outputIndex: number
      topic: string
      spendingTxid: string
      inputIndex: number
      unlockingScript: Script
      sequenceNumber: number
      offChainValues?: number[]
    }
  | {
      mode: 'whole-tx'
      txid: string
      outputIndex: number
      topic: string
      spendingAtomicBEEF: number[]
      offChainValues?: number[]
    }

export type LookupFormula = Array<{
  txid: string
  outputIndex: number
  history?: ((beef: number[], outputIndex: number, currentDepth: number) => Promise<boolean>) | number
  context?: number[]
}>

export interface LookupQuestion {
  service: string
  query: unknown
}

export interface LookupServiceMetaData {
  name: string
  shortDescription: string
  iconURL?: string
  version?: string
  informationURL?: string
}

export interface LookupService {
  readonly admissionMode: 'locking-script' | 'whole-tx'
  readonly spendNotificationMode: 'none' | 'txid' | 'script' | 'whole-tx'
  outputAdmittedByTopic: (payload: OutputAdmittedByTopic) => Promise<void> | void
  outputSpent?: (payload: OutputSpent) => Promise<void> | void
  outputNoLongerRetainedInHistory?: (txid: string, outputIndex: number, topic: string) => Promise<void> | void
  outputEvicted: (txid: string, outputIndex: number) => Promise<void> | void
  lookup: (question: LookupQuestion) => Promise<LookupFormula>
  getDocumentation: () => Promise<string>
  getMetaData: () => Promise<LookupServiceMetaData>
}

/** One row of the engine's outputs table, as findUTXOsForTopic returns it. */
export interface Output {
  txid: string
  outputIndex: number
  outputScript: number[]
  satoshis: number
  topic: string
  spent: boolean
  outputsConsumed: Array<{ txid: string; outputIndex: number }>
  consumedBy: Array<{ txid: string; outputIndex: number }>
  beef?: number[]
  blockHeight?: number
  score?: number
}

export interface ModuleHost {
  log: (msg: string, extra?: Record<string, unknown>) => void
  metrics: {
    inc: (name: string, labels?: Record<string, string>, by?: number) => void
    preset: (name: string, labels?: Record<string, string>) => void
    gauge: (name: string, fn: () => number) => void
  }
}

/**
 * The one storage call a restore may make: the engine's Storage.findOutput
 * (Storage.d.ts: `(txid, outputIndex, topic?, spent?, includeBEEF?) =>
 * Promise<Output | null>`), narrowed to the arguments used. The host passes
 * its real storage object, whose extra optional parameters make it
 * assignable here. It is what lets a lookup service read an output that is
 * NOT among the unspent rows it was handed: a spent funding output, with
 * `spent` and `consumedBy` saying who spent it.
 */
export interface RestoreStorage {
  findOutput: (txid: string, outputIndex: number, topic?: string) => Promise<Output | null>
}

export interface Module {
  topics?: Record<string, TopicManager>
  lookups?: Record<
    string,
    LookupService & { restore?: (outputs: Output[], storage: RestoreStorage) => number | Promise<number> }
  >
}

// types.ts — the memory domain vocabulary lives in @nektar/memory; this
// re-export keeps every mcp-internal './types.js' specifier resolving while
// the ops modules and NektarMemory share the types with the memory package.
export { CATALOG_SHARD } from '@nektar/memory'
export type {
  OperatorWallet,
  NektarMemoryOptions,
  PersistedMemoryState,
  MemoryStateStore,
  ShardPack,
  ManifestOffer,
  ShardManifest,
  SecurityStateStore,
  CallerIdentity,
  ZkClearance,
  RememberResult,
  AnswerReceipt
} from '@nektar/memory'

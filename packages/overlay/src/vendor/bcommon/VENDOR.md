# Vendored: @lightwebinc/bcommon

Source: https://github.com/lightwebinc/bcommon `ts/` tree
Version: 0.33.10 (Apache-2.0 — LICENSE and NOTICE preserved in this directory)
Pinned: do not float; upstream is experimental and interfaces move.

Files vendored: `cbor.ts`, `pubkey.ts`, `record.ts`, `pushdrop.ts`,
`fieldsig.ts`, `commit.ts`, `engine-types.ts` — deterministic CBOR, the
bstack committed-record reader, PushDrop reading + field signatures, the
commitment codec, and the overlay engine type surface (TopicManager,
LookupService, Output, ModuleHost contract) used by `src/host-module.ts`.

Local changes: none beyond compiling under `@bsv/sdk@4.0.1` (upstream peers
2.7.1). Imported symbol names are unchanged.

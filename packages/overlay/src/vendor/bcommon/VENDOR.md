# Vendored: @lightwebinc/bcommon

Source: https://github.com/lightwebinc/bcommon `ts/` tree
Version: 0.33.10 (Apache-2.0 — LICENSE and NOTICE preserved in this directory)
Pinned: do not float; upstream is experimental and interfaces move.

Only `engine-types.ts` is vendored — the overlay engine type surface
(TopicManager, LookupService, Output, ModuleHost contract) used by
`src/host-module.ts`. The codec files (cbor/record/fieldsig/pushdrop/commit)
were dropped as redundant: our artifacts keep their own canonical signing —
importing theirs would duplicate, not interoperate.

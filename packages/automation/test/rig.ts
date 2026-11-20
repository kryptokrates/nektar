// rig.ts — test fixture: an in-process Executor that dispatches to the
// same handler bodies the restate endpoint serves. There is no local mode
// in production — this fixture exists so memory-tier tests exercise the
// real bodies without a live restate server. The implementation lives in
// src/rt/dispatch.ts so other packages' in-process tests can use it too.
export {
  Dispatcher,
  isLocalDispatch,
  wireTx,
  wireSweep,
  wireDeal,
  wireDealfan,
  wireCustody,
  wireTransport,
  wireWake,
  wireCron,
  wireLeg,
  wireNotify,
  TRANSPORT_DRAIN_KEY
} from '../src/rt/dispatch.js'
export type { LocalDispatch, DispatchHandler } from '../src/rt/dispatch.js'

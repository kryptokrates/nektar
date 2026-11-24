// hostplane.ts — the automation-less counterpart of the endpoint
// openAutomation opens. With NEKTAR_AUTOMATION off the host still needs its
// journaled objects served: the tx object answers the ledger's observe/
// resubmit invocations, the confirmer cadence drives flushDue + regtest
// mining, and the records watcher consumes ledger verdicts. Without this
// every wrapBroadcast emit lands on an ingress nobody answers and ledger
// rows sit 'prepared' forever.
//   NEVER both planes in one process — they register the same service
//   names (nektar-tx / nektar-confirmer / nektar-watcher) and the second
//   registration rebinds the deployment off the first.
import type { AutomationStore } from '../store.js'
import type { TxEffects } from '../machines/effects.js'
import { txObject } from './services.js'
import { openEndpoint } from './endpoint.js'
import { executor, setExecutor, type Executor, type InvokeTarget } from './exec.js'
import { Dispatcher, wireTx, type LocalDispatch } from './dispatch.js'

/** the durable host plane — tx object + every host-composed def (confirmer,
 *  watcher) registered on one endpoint, then each boot kick fired once. A
 *  registration failure propagates: the executor plane is mandatory, so a
 *  host that cannot register refuses to boot rather than serve a silent
 *  ledger. */
export async function openHostPlane(d: {
  /** the host's wallet ledger — the tx object serves its rows */
  store: AutomationStore
  txEffects: TxEffects
  /** host-composed defs (confirmer cadence, records watcher) + their boot kicks */
  rtDefs: unknown[]
  rtKicks: InvokeTarget[]
  log: (line: string) => void
  /** test seams — production uses openEndpoint + the ambient executor */
  open?: typeof openEndpoint
  exec?: Executor
}): Promise<{ uri: string; stop: () => Promise<void> }> {
  // no `submit` — the tx object's resubmit handler already answers
  // { resubmitted: false } without one; resubmission is a market-plane leg.
  const txDef = await txObject({ store: d.store, fx: d.txEffects, log: d.log })
  const ep = await (d.open ?? openEndpoint)([txDef, ...d.rtDefs], process.env, d.log)
  d.log(`host plane: restate endpoint ${ep.uri}`)
  for (const k of d.rtKicks) void (d.exec ?? executor()).send(k, {}).catch((e) => d.log(`host plane: ${k.service} kick: ${(e as Error).message}`))
  return { uri: ep.uri, stop: () => ep.close() }
}

/** sidecar-free solo host plane — serves tx object + confirmer + records
 *  watcher in-process on a local Dispatcher when NEKTAR_AUTOMATION is off
 *  (zero Restate binary, zero WSL2, zero Docker). Durable multi-step market
 *  automation (NEKTAR_AUTOMATION=1) continues to require Restate. */
export async function openLocalHostPlane(d: {
  store: AutomationStore
  txEffects: TxEffects
  localWire?: (ex: LocalDispatch) => void
  rtKicks: InvokeTarget[]
  log: (line: string) => void
}): Promise<{ uri: string; stop: () => Promise<void> }> {
  const disp = new Dispatcher(d.log)
  setExecutor(disp)
  wireTx(disp, { store: d.store, fx: d.txEffects, log: d.log })
  d.localWire?.(disp)
  d.log('host plane: in-process solo executor (Restate not required until NEKTAR_AUTOMATION=1)')
  for (const k of d.rtKicks) void disp.send(k, {}).catch((e) => d.log(`host plane: ${k.service} kick: ${(e as Error).message}`))
  return {
    uri: 'local://in-process',
    stop: async () => {
      await disp.close()
      setExecutor(undefined)
    }
  }
}

// One bstack overlay host as an OS process: the real nektar module plus the
// real vendored bbox admission codec, backed by the shared mock-chain hub.
import { minihost } from '../../../bstack/dist/src/testing.js'
import { DEFAULT_OFFICE } from '../../../bstack/dist/src/index.js'
import { RemoteMockChain } from './remote-mock-chain.mjs'

const env = process.env
const host = await minihost(['mock'], { tracker: new RemoteMockChain(env.NEKTAR_CHAIN_HUB), office: DEFAULT_OFFICE, port: Number(env.NEKTAR_HOST_PORT) })
console.log(`market host on ${host.url} (office ${DEFAULT_OFFICE})`)
const stop = async () => {
  await host.close().catch(() => {})
  process.exit(0)
}
process.on('SIGTERM', () => {
  void stop()
})
process.on('SIGINT', () => {
  void stop()
})

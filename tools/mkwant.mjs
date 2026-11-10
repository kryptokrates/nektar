import { PrivateKey } from '@bsv/sdk'
import { signWant } from '/c/Users/nedzo/Documents/Devin/nektar/packages/overlay/dist/src/index.js'
const key = PrivateKey.fromRandom()
process.stdout.write(JSON.stringify(signWant(key, { domains: ['hetzner-fleet-test'], question: 'v2 probe', maxSats: 1, ts: Date.now() })))

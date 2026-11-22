// substrate-probe — probeWalletSubstrate classifies the local BRC-100 API
// into live/locked/absent so NEKTAR_WALLET=client boots with a real reason
// instead of the SDK's flattened 'Install a BSV wallet today!'. Stubs fetch —
// classification logic only; the live path is operator-verified.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { probeWalletSubstrate } from '../src/rtsupport.js'

const stubFetch = (impl: () => Promise<{ json: () => Promise<unknown> } | never>): typeof fetch => impl as unknown as typeof fetch

void test('probe: a wallet that answers is live', async () => {
  const res = stubFetch(async () => ({ json: async () => ({ version: 'wallet-brc100-1.0.0' }) }))
  assert.equal(await probeWalletSubstrate(res), 'live')
})

void test('probe: WALLET_LOCKED 503 body is a locked vault — a live wallet', async () => {
  const res = stubFetch(async () => ({
    json: async () => ({ status: 'error', code: 'WALLET_LOCKED', description: 'Wallet vault is locked.' })
  }))
  assert.equal(await probeWalletSubstrate(res), 'locked')
})

void test('probe: a description-only locked answer still classifies', async () => {
  const res = stubFetch(async () => ({ json: async () => ({ description: 'service locked' }) }))
  assert.equal(await probeWalletSubstrate(res), 'locked')
})

void test('probe: a refused connection is absent', async () => {
  const res = stubFetch(async () => {
    throw new Error('connect ECONNREFUSED 127.0.0.1:3321')
  })
  assert.equal(await probeWalletSubstrate(res), 'absent')
})

void test('probe: an unparseable body still counts as live (the API answered)', async () => {
  const res = stubFetch(async () => ({
    json: async () => {
      throw new Error('not json')
    }
  }))
  assert.equal(await probeWalletSubstrate(res), 'live')
})

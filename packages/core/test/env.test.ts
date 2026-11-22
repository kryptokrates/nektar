import { test } from 'node:test'
import assert from 'node:assert/strict'
import { parseDotEnv } from '../src/env.js'

// parseDotEnv — the one .env contract every reader shares (bin, init,
// watcher, tools/*). These cases pin the behaviors the five divergent
// parsers used to disagree on: quote stripping (a literal-quote RPC pass
// booted one reader and failed another), export prefixes, inline comments
// only after whitespace outside quotes.

test('parseDotEnv: comments, blanks, export prefixes, whitespace around =', () => {
  const out = parseDotEnv(
    ['# a comment', '', '   ', 'NEKTAR_NETWORK=regtest', 'export NEKTAR_HTTP_PORT=8080', '  NEKTAR_DATA_DIR = .nektar ', 'not a line', '=no key'].join('\n')
  )
  assert.deepEqual(out, { NEKTAR_NETWORK: 'regtest', NEKTAR_HTTP_PORT: '8080', NEKTAR_DATA_DIR: '.nektar' })
})

test('parseDotEnv: surrounding quotes strip — the RPC-pass incident case', () => {
  const out = parseDotEnv(['NEKTAR_RPC_PASS="s3cret pass"', "NEKTAR_RPC_USER='operator'"].join('\n'))
  assert.equal(out.NEKTAR_RPC_PASS, 's3cret pass')
  assert.equal(out.NEKTAR_RPC_USER, 'operator')
})

test("parseDotEnv: '#' inside quotes is value; after whitespace outside it's a comment", () => {
  const out = parseDotEnv(['A="hash # inside"', "B='also # inside'", 'C=value # a comment', 'D=value#stays', 'E=mid #dle'].join('\n'))
  assert.equal(out.A, 'hash # inside')
  assert.equal(out.B, 'also # inside')
  assert.equal(out.C, 'value')
  assert.equal(out.D, 'value#stays', "a '#' with no preceding space is value, not a comment")
  assert.equal(out.E, 'mid')
})

test('parseDotEnv: empty + unterminated-quote edges keep a value, never throw', () => {
  const out = parseDotEnv(['EMPTY=', 'UNTERMINATED="half', "UNTERMINATED_S='half"].join('\n'))
  assert.equal(out.EMPTY, '')
  assert.equal(out.UNTERMINATED, 'half')
  assert.equal(out.UNTERMINATED_S, 'half')
})

test('parseDotEnv: CRLF endings and last-key-wins', () => {
  const out = parseDotEnv('A=1\r\nA=2\r\nB=3')
  assert.deepEqual(out, { A: '2', B: '3' })
})

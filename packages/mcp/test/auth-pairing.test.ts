import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import type { AddressInfo } from 'node:net'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { testWorld } from '@nektar/chain'
import { NektarMemory, createHttpApi } from '../src/index.js'
import { createAuthManager, resolveOrGenerateTokens } from '../src/auth.js'
import { formatStartupBanner } from '../src/banner.js'

const mkTmp = (p: string) => mkdtempSync(join(tmpdir(), p))

void test('resolveOrGenerateTokens: generates persistent 64-hex secrets and respects env overrides', () => {
  const dir = mkTmp('nektar-auth-tok-')
  try {
    const env1: Record<string, string | undefined> = {}
    const r1 = resolveOrGenerateTokens(dir, env1)
    assert.equal(r1.generatedHttp, true)
    assert.equal(r1.generatedOperator, true)
    assert.equal(r1.httpToken.length, 64)
    assert.equal(r1.operatorToken.length, 64)
    assert.equal(env1.NEKTAR_HTTP_TOKEN, r1.httpToken)
    assert.equal(readFileSync(join(dir, 'http-token'), 'utf8').trim(), r1.httpToken)

    // Second boot reuses persisted secrets
    const env2: Record<string, string | undefined> = {}
    const r2 = resolveOrGenerateTokens(dir, env2)
    assert.equal(r2.generatedHttp, false)
    assert.equal(r2.httpToken, r1.httpToken)
    assert.equal(r2.operatorToken, r1.operatorToken)

    // Explicit env wins
    const env3: Record<string, string | undefined> = { NEKTAR_HTTP_TOKEN: 'explicit-http-secret-1234' }
    const r3 = resolveOrGenerateTokens(dir, env3)
    assert.equal(r3.httpToken, 'explicit-http-secret-1234')
    assert.equal(r3.generatedHttp, false)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

void test('Dashboard shell, one-time pairing cookie, and MCP OAuth 2.1 PKCE flow', async (t) => {
  const w = await testWorld(t, 4)
  if (w === undefined) return
  const prevHttp = process.env.NEKTAR_HTTP_TOKEN
  const prevOp = process.env.NEKTAR_OPERATOR_TOKEN
  process.env.NEKTAR_HTTP_TOKEN = 'agent-secret-token-123456'
  process.env.NEKTAR_OPERATOR_TOKEN = 'operator-secret-token-987654'
  const mem = new NektarMemory(w.chain, w.funder, w.funder.keyPair, { batchPolicy: { maxLeaves: 2 }, network: 'testnet' })
  const auth = createAuthManager()
  const srv = createHttpApi(mem, undefined, { auth }).listen(0, '127.0.0.1')
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`

  try {
    // 1. Browser shell / is 200 HTML even under token auth, while /state and /v1/tools are 401
    const homeRes = await fetch(`${base}/`)
    assert.equal(homeRes.status, 200)
    assert.match(homeRes.headers.get('content-type') ?? '', /text\/html/)
    assert.equal((await fetch(`${base}/state`)).status, 401)
    assert.equal((await fetch(`${base}/v1/tools`)).status, 401)
    const stUnauth = (await (await fetch(`${base}/v1/auth/status`)).json()) as { authRequired: boolean; authenticated: boolean }
    assert.deepEqual(stUnauth, { authRequired: true, authenticated: false, role: 'none', operatorConfigured: true })

    // 2. One-time pairing code sets HttpOnly SameSite=Strict session cookie and cannot be replayed
    const code = auth.issuePairingCode('operator')
    const pairRes = await fetch(`${base}/pair?code=${encodeURIComponent(code)}`, { redirect: 'manual' })
    assert.equal(pairRes.status, 302)
    assert.equal(pairRes.headers.get('location'), '/')
    const setCookie = pairRes.headers.get('set-cookie') ?? ''
    assert.match(setCookie, /nektar_sess=/)
    assert.match(setCookie, /HttpOnly/)
    assert.match(setCookie, /SameSite=Strict/)
    const cookieHeader = setCookie.split(';')[0]

    // Replay of one-time code fails
    assert.equal((await fetch(`${base}/pair?code=${encodeURIComponent(code)}`, { redirect: 'manual' })).status, 400)

    // Cookie unlocks /state and /v1/auth/status
    assert.equal((await fetch(`${base}/state`, { headers: { cookie: cookieHeader } })).status, 200)
    const stAuthed = (await (await fetch(`${base}/v1/auth/status`, { headers: { cookie: cookieHeader } })).json()) as {
      authenticated: boolean
      role: string
    }
    assert.equal(stAuthed.authenticated, true)
    assert.equal(stAuthed.role, 'operator')

    // 3. POST /v1/auth/session rejects invalid tokens and accepts valid tokens
    assert.equal(
      (await fetch(`${base}/v1/auth/session`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ token: 'wrong' }) }))
        .status,
      401
    )
    const sessOk = await fetch(`${base}/v1/auth/session`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ token: 'agent-secret-token-123456' })
    })
    assert.equal(sessOk.status, 200)

    // 4. MCP OAuth 2.1 discovery + dynamic client registration + PKCE S256
    const prMeta = (await (await fetch(`${base}/.well-known/oauth-protected-resource`)).json()) as { resource: string; authorization_servers: string[] }
    assert.match(prMeta.resource, /\/mcp$/)
    const asMeta = (await (await fetch(`${base}/.well-known/oauth-authorization-server`)).json()) as { token_endpoint: string; registration_endpoint: string }
    assert.match(asMeta.token_endpoint, /\/oauth\/token$/)

    // Bad redirect URI rejected
    assert.equal(
      (
        await fetch(`${base}/oauth/register`, {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ client_name: 'evil', redirect_uris: ['javascript:alert(1)'] })
        })
      ).status,
      400
    )

    const reg = (await (
      await fetch(`${base}/oauth/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ client_name: 'Test Connector', redirect_uris: ['http://localhost:9999/cb'] })
      })
    ).json()) as { client_id: string }
    assert.ok(reg.client_id.startsWith('nkt_'))

    const verifier = 'verifier-random-string-0123456789abcdef0123456789'
    const challenge = createHash('sha256').update(verifier).digest('base64url')
    const authRes = await fetch(`${base}/oauth/authorize`, {
      method: 'POST',
      redirect: 'manual',
      headers: { 'content-type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({
        client_id: reg.client_id,
        redirect_uri: 'http://localhost:9999/cb',
        state: 'st1',
        code_challenge: challenge,
        code_challenge_method: 'S256',
        token: 'agent-secret-token-123456'
      })
    })
    assert.equal(authRes.status, 302)
    const loc = new URL(authRes.headers.get('location')!)
    const authCode = loc.searchParams.get('code')!
    assert.ok(authCode.length > 10)
    assert.equal(loc.searchParams.get('state'), 'st1')

    // Wrong PKCE verifier rejected (and consumes code) -> re-issue for success test
    const authRes2 = await fetch(`${base}/oauth/authorize`, {
      method: 'POST',
      redirect: 'manual',
      headers: { 'content-type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({
        client_id: reg.client_id,
        redirect_uri: 'http://localhost:9999/cb',
        code_challenge: challenge,
        code_challenge_method: 'S256',
        token: 'agent-secret-token-123456'
      })
    })
    const authCode2 = new URL(authRes2.headers.get('location')!).searchParams.get('code')!

    const badEx = await fetch(`${base}/oauth/token`, {
      method: 'POST',
      headers: { 'content-type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({
        grant_type: 'authorization_code',
        code: authCode,
        client_id: reg.client_id,
        redirect_uri: 'http://localhost:9999/cb',
        code_verifier: 'wrong-verifier'
      })
    })
    assert.equal(badEx.status, 400)

    const goodEx = (await (
      await fetch(`${base}/oauth/token`, {
        method: 'POST',
        headers: { 'content-type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({
          grant_type: 'authorization_code',
          code: authCode2,
          client_id: reg.client_id,
          redirect_uri: 'http://localhost:9999/cb',
          code_verifier: verifier
        })
      })
    ).json()) as { access_token: string; token_type: string }
    assert.equal(goodEx.token_type, 'Bearer')
    assert.equal((await fetch(`${base}/v1/tools`, { headers: { authorization: `Bearer ${goodEx.access_token}` } })).status, 200)

    // 5. Startup banner formatting
    const banner = formatStartupBanner({
      url: base,
      network: 'testnet',
      bootMs: 1200,
      pairCode: 'abcd-1234-ef56',
      feeAddress: 'mtTestAddr',
      balanceSats: 0,
      automationOn: false,
      containerMode: true,
      generatedTokens: true
    })
    assert.match(banner, /\/pair\?code=abcd-1234-ef56/)
    assert.match(banner, /\/v1\/proxy\/openai\/v1/)
    assert.match(banner, /1-click faucet in Dashboard/)
  } finally {
    srv.close()
    if (prevHttp === undefined) delete process.env.NEKTAR_HTTP_TOKEN
    else process.env.NEKTAR_HTTP_TOKEN = prevHttp
    if (prevOp === undefined) delete process.env.NEKTAR_OPERATOR_TOKEN
    else process.env.NEKTAR_OPERATOR_TOKEN = prevOp
  }
})

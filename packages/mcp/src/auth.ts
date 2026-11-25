// auth.ts — persistent auto-generated server tokens, browser pairing cookies
// (HttpOnly, SameSite=Strict), and lightweight MCP OAuth 2.1 (RFC 9728 / 8414 /
// 7591 + PKCE S256) so both local browsers and hosted MCP clients connect
// smoothly without ever exposing an unauthenticated 0.0.0.0 bind.
import { createHash, createHmac, randomBytes, timingSafeEqual } from 'node:crypto'
import { chmodSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import type { IncomingMessage, ServerResponse } from 'node:http'
import { join } from 'node:path'
import { nektarEnv, setNektarEnv, type EnvSource } from '@nektar/core'
import { safeHost } from './pages.js'

const COOKIE_NAME = 'nektar_sess'
const SESSION_TTL_SEC = 7 * 24 * 3600
const PAIR_TTL_MS = 10 * 60_000
const OAUTH_CODE_TTL_MS = 5 * 60_000
const OAUTH_TOKEN_TTL_SEC = 24 * 3600

type AuthRole = 'none' | 'agent' | 'operator'

export interface GeneratedTokens {
  httpToken: string
  operatorToken: string
  generatedHttp: boolean
  generatedOperator: boolean
}

const safeEq = (a: string, b: string): boolean => {
  const ab = Buffer.from(a)
  const bb = Buffer.from(b)
  return ab.length === bb.length && timingSafeEqual(ab, bb)
}

const readOrCreateSecretFile = (file: string): { value: string; created: boolean } => {
  if (existsSync(file)) {
    const existing = readFileSync(file, 'utf8').trim()
    if (existing.length >= 16) return { value: existing, created: false }
  }
  const value = randomBytes(32).toString('hex')
  writeFileSync(file, value + '\n', { mode: 0o600 })
  try {
    chmodSync(file, 0o600)
  } catch {
    /* Windows NTFS inherits user ACL */
  }
  return { value, created: true }
}

/** Ensure NEKTAR_HTTP_TOKEN and NEKTAR_OPERATOR_TOKEN exist for non-loopback or
 *  `serve` boots: explicit env values always win; otherwise persist mode-0600
 *  random secrets under dataDir so restarts keep the same credentials. */
export function resolveOrGenerateTokens(dataDir: string, env: EnvSource = process.env): GeneratedTokens {
  mkdirSync(dataDir, { recursive: true })
  const envHttp = nektarEnv('NEKTAR_HTTP_TOKEN', env)
  const envOp = nektarEnv('NEKTAR_OPERATOR_TOKEN', env)
  let httpToken: string
  let generatedHttp = false
  if (envHttp !== undefined) {
    httpToken = envHttp
  } else {
    const r = readOrCreateSecretFile(join(dataDir, 'http-token'))
    httpToken = r.value
    generatedHttp = r.created
    setNektarEnv('NEKTAR_HTTP_TOKEN', httpToken, env)
  }
  let operatorToken: string
  let generatedOperator = false
  if (envOp !== undefined) {
    operatorToken = envOp
  } else {
    const r = readOrCreateSecretFile(join(dataDir, 'operator-token'))
    operatorToken = r.value
    generatedOperator = r.created
    setNektarEnv('NEKTAR_OPERATOR_TOKEN', operatorToken, env)
  }
  return { httpToken, operatorToken, generatedHttp, generatedOperator }
}

export const isLoopbackPeer = (req: IncomingMessage): boolean => {
  const peer = req.socket.remoteAddress ?? ''
  return peer === '127.0.0.1' || peer === '::1' || peer === '::ffff:127.0.0.1'
}

const parseCookies = (header: string | undefined): Record<string, string> => {
  const out: Record<string, string> = {}
  if (!header) return out
  for (const part of header.split(';')) {
    const eq = part.indexOf('=')
    if (eq <= 0) continue
    out[part.slice(0, eq).trim()] = part.slice(eq + 1).trim()
  }
  return out
}

const escHtml = (s: string): string => s.replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c] ?? c)

const allowedRedirectUri = (raw: string): boolean => {
  try {
    const u = new URL(raw)
    if (u.protocol === 'https:') return true
    return u.protocol === 'http:' && (u.hostname === 'localhost' || u.hostname === '127.0.0.1' || u.hostname === '[::1]')
  } catch {
    return false
  }
}

interface OAuthClient {
  clientId: string
  clientName: string
  redirectUris: string[]
}

interface OAuthCode {
  clientId: string
  redirectUri: string
  codeChallenge: string
  role: 'agent' | 'operator'
  expiresAt: number
}

interface OAuthAccessToken {
  role: 'agent' | 'operator'
  expiresAt: number
}

export interface AuthManager {
  issuePairingCode: (role?: 'agent' | 'operator') => string
  authState: (req: IncomingMessage) => { authed: boolean; operator: boolean; role: 'none' | 'agent' | 'operator'; authRequired: boolean }
  handleRoute: (req: IncomingMessage, res: ServerResponse, url: URL, readBody: () => Promise<unknown>) => Promise<boolean>
}

export function createAuthManager(env: EnvSource = process.env): AuthManager {
  const pairCodes = new Map<string, { role: 'agent' | 'operator'; expiresAt: number }>()
  const oauthClients = new Map<string, OAuthClient>()
  const oauthCodes = new Map<string, OAuthCode>()
  const oauthTokens = new Map<string, OAuthAccessToken>()
  const fallbackSecret = randomBytes(32).toString('hex')

  const signKey = (): string => nektarEnv('NEKTAR_HTTP_TOKEN', env) ?? nektarEnv('NEKTAR_OPERATOR_TOKEN', env) ?? fallbackSecret

  const signCookie = (role: 'agent' | 'operator'): string => {
    const exp = Math.floor(Date.now() / 1000) + SESSION_TTL_SEC
    const payload = `${role}.${exp}`
    const sig = createHmac('sha256', signKey()).update(`nektar-sess-v1:${payload}`).digest('base64url')
    return `${payload}.${sig}`
  }

  const verifyCookie = (raw: string | undefined): 'agent' | 'operator' | undefined => {
    if (!raw) return undefined
    const parts = raw.split('.')
    if (parts.length !== 3) return undefined
    const [role, expStr, sig] = parts
    if (role !== 'agent' && role !== 'operator') return undefined
    const exp = Number(expStr)
    if (!Number.isFinite(exp) || exp < Math.floor(Date.now() / 1000)) return undefined
    const expected = createHmac('sha256', signKey()).update(`nektar-sess-v1:${role}.${expStr}`).digest('base64url')
    return safeEq(sig, expected) ? role : undefined
  }

  const setSessionCookie = (req: IncomingMessage, res: ServerResponse, role: 'agent' | 'operator'): void => {
    const secure = req.headers['x-forwarded-proto'] === 'https'
    const val = signCookie(role)
    res.setHeader('set-cookie', `${COOKIE_NAME}=${val}; HttpOnly; SameSite=Strict; Path=/; Max-Age=${SESSION_TTL_SEC}${secure ? '; Secure' : ''}`)
  }

  const clearSessionCookie = (res: ServerResponse): void => {
    res.setHeader('set-cookie', `${COOKIE_NAME}=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0`)
  }

  const verifyPresentedSecret = (secret: string): 'agent' | 'operator' | undefined => {
    const opTok = nektarEnv('NEKTAR_OPERATOR_TOKEN', env)
    const httpTok = nektarEnv('NEKTAR_HTTP_TOKEN', env)
    if (opTok !== undefined && safeEq(secret, opTok)) return 'operator'
    if (httpTok !== undefined && safeEq(secret, httpTok)) return opTok === undefined ? 'operator' : 'agent'
    return undefined
  }

  const issuePairingCode = (role: 'agent' | 'operator' = 'operator'): string => {
    const now = Date.now()
    for (const [k, v] of pairCodes) if (v.expiresAt < now) pairCodes.delete(k)
    if (pairCodes.size >= 16) {
      const oldest = pairCodes.keys().next().value
      if (oldest !== undefined) pairCodes.delete(oldest)
    }
    const hex = randomBytes(6).toString('hex')
    const code = `${hex.slice(0, 4)}-${hex.slice(4, 8)}-${hex.slice(8, 12)}`
    pairCodes.set(code, { role, expiresAt: now + PAIR_TTL_MS })
    return code
  }

  const authState = (req: IncomingMessage): { authed: boolean; operator: boolean; role: AuthRole; authRequired: boolean } => {
    const httpTok = nektarEnv('NEKTAR_HTTP_TOKEN', env)
    const opTok = nektarEnv('NEKTAR_OPERATOR_TOKEN', env)
    const authRequired = httpTok !== undefined
    const authHdr = typeof req.headers.authorization === 'string' ? req.headers.authorization : ''
    if (authHdr.startsWith('Bearer ')) {
      const tok = authHdr.slice(7).trim()
      if (opTok !== undefined && safeEq(tok, opTok)) return { authed: true, operator: true, role: 'operator', authRequired }
      if (httpTok !== undefined && safeEq(tok, httpTok)) return { authed: true, operator: false, role: 'agent', authRequired }
      const oauth = oauthTokens.get(tok)
      if (oauth !== undefined) {
        if (oauth.expiresAt > Date.now()) {
          return { authed: true, operator: oauth.role === 'operator', role: oauth.role, authRequired }
        }
        oauthTokens.delete(tok)
      }
    }
    const cookieRole = verifyCookie(parseCookies(req.headers.cookie)[COOKIE_NAME])
    if (cookieRole !== undefined) {
      return { authed: true, operator: cookieRole === 'operator', role: cookieRole, authRequired }
    }
    if (!authRequired && isLoopbackPeer(req)) {
      return { authed: true, operator: opTok === undefined, role: opTok === undefined ? 'operator' : 'agent', authRequired: false }
    }
    return { authed: false, operator: false, role: 'none', authRequired }
  }

  const sendJson = (res: ServerResponse, code: number, body: unknown): void => {
    res.writeHead(code, { 'content-type': 'application/json' })
    res.end(JSON.stringify(body))
  }

  const baseFor = (req: IncomingMessage): string => {
    const explicit = nektarEnv('NEKTAR_VERIFY_BASE', env)
    if (explicit) return explicit.replace(/\/+$/, '')
    const proto = req.headers['x-forwarded-proto'] === 'https' ? 'https' : 'http'
    return `${proto}://${safeHost(req.headers.host)}`
  }

  const readFormOrJson = async (req: IncomingMessage, readBody: () => Promise<unknown>): Promise<Record<string, string | undefined>> => {
    const ct = String(req.headers['content-type'] ?? '').toLowerCase()
    if (ct.includes('application/x-www-form-urlencoded')) {
      const chunks: Buffer[] = []
      let total = 0
      for await (const c of req) {
        const b = c as Buffer
        total += b.length
        if (total > 64 * 1024) throw new Error('body too large')
        chunks.push(b)
      }
      const sp = new URLSearchParams(Buffer.concat(chunks).toString('utf8'))
      const out: Record<string, string | undefined> = {}
      for (const [k, v] of sp.entries()) out[k] = v
      return out
    }
    const raw = (await readBody().catch(() => ({}))) as Record<string, unknown>
    const out: Record<string, string | undefined> = {}
    for (const [k, v] of Object.entries(raw)) if (typeof v === 'string') out[k] = v
    return out
  }

  const handleRoute = async (req: IncomingMessage, res: ServerResponse, url: URL, readBody: () => Promise<unknown>): Promise<boolean> => {
    // 1. Browser auth status check (public, leaks zero secrets)
    if (req.method === 'GET' && url.pathname === '/v1/auth/status') {
      const st = authState(req)
      sendJson(res, 200, {
        authRequired: st.authRequired,
        authenticated: st.authed,
        role: st.role,
        operatorConfigured: nektarEnv('NEKTAR_OPERATOR_TOKEN', env) !== undefined
      })
      return true
    }

    // 2. Exchange bearer token -> HttpOnly session cookie
    if (req.method === 'POST' && url.pathname === '/v1/auth/session') {
      const b = (await readBody().catch(() => ({}))) as { token?: unknown }
      const token = typeof b.token === 'string' ? b.token.trim() : ''
      if (!token) {
        sendJson(res, 400, { error: 'token required' })
        return true
      }
      const role = verifyPresentedSecret(token)
      if (!role) {
        sendJson(res, 401, { error: 'invalid token' })
        return true
      }
      setSessionCookie(req, res, role)
      sendJson(res, 200, { ok: true, role })
      return true
    }

    if (req.method === 'DELETE' && url.pathname === '/v1/auth/session') {
      clearSessionCookie(res)
      sendJson(res, 200, { ok: true })
      return true
    }

    // 3. One-time pairing link from Startup Card -> HttpOnly cookie + redirect /
    if (req.method === 'GET' && url.pathname === '/pair') {
      const code = (url.searchParams.get('code') ?? '').trim()
      const entry = pairCodes.get(code)
      if (!entry || entry.expiresAt < Date.now()) {
        pairCodes.delete(code)
        res.writeHead(400, { 'content-type': 'text/html; charset=utf-8' })
        res.end(
          '<!doctype html><html><body style="background:#120e0c;color:#ece7e4;font:15px system-ui;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0"><div style="max-width:420px;text-align:center"><h2>Pairing link expired or already used</h2><p style="color:#a89a8e">Open <a href="/" style="color:#f7991b">the dashboard</a> and paste your token, or run <code>nektar open</code>.</p></div></body></html>'
        )
        return true
      }
      pairCodes.delete(code)
      setSessionCookie(req, res, entry.role)
      res.writeHead(302, { location: '/' })
      res.end()
      return true
    }

    // 4. MCP OAuth 2.1 Discovery (RFC 9728 + RFC 8414)
    if (req.method === 'GET' && url.pathname === '/.well-known/oauth-protected-resource') {
      const base = baseFor(req)
      sendJson(res, 200, {
        resource: `${base}/mcp`,
        authorization_servers: [base],
        bearer_methods_supported: ['header']
      })
      return true
    }

    if (req.method === 'GET' && url.pathname === '/.well-known/oauth-authorization-server') {
      const base = baseFor(req)
      sendJson(res, 200, {
        issuer: base,
        authorization_endpoint: `${base}/oauth/authorize`,
        token_endpoint: `${base}/oauth/token`,
        registration_endpoint: `${base}/oauth/register`,
        response_types_supported: ['code'],
        grant_types_supported: ['authorization_code'],
        code_challenge_methods_supported: ['S256'],
        token_endpoint_auth_methods_supported: ['none']
      })
      return true
    }

    // 5. Dynamic Client Registration (RFC 7591)
    if (req.method === 'POST' && url.pathname === '/oauth/register') {
      const b = (await readBody().catch(() => ({}))) as { client_name?: unknown; redirect_uris?: unknown }
      const rawUris = Array.isArray(b.redirect_uris) ? b.redirect_uris.filter((u): u is string => typeof u === 'string') : []
      if (rawUris.length === 0 || !rawUris.every(allowedRedirectUri)) {
        sendJson(res, 400, { error: 'invalid_redirect_uri', error_description: 'redirect_uris must be https or localhost' })
        return true
      }
      if (oauthClients.size >= 64) {
        const oldest = oauthClients.keys().next().value
        if (oldest !== undefined) oauthClients.delete(oldest)
      }
      const clientId = `nkt_${randomBytes(12).toString('hex')}`
      const clientName = typeof b.client_name === 'string' && b.client_name.trim() ? b.client_name.trim().slice(0, 80) : 'MCP Client'
      oauthClients.set(clientId, { clientId, clientName, redirectUris: rawUris })
      sendJson(res, 201, {
        client_id: clientId,
        client_name: clientName,
        redirect_uris: rawUris,
        token_endpoint_auth_method: 'none',
        grant_types: ['authorization_code'],
        response_types: ['code']
      })
      return true
    }

    // 6. OAuth Authorize UI + POST approval
    if (url.pathname === '/oauth/authorize' && (req.method === 'GET' || req.method === 'POST')) {
      const params: Record<string, string | undefined> =
        req.method === 'POST' ? await readFormOrJson(req, readBody) : Object.fromEntries(url.searchParams.entries())
      const clientId = params.client_id ?? ''
      const redirectUri = params.redirect_uri ?? ''
      const state = params.state ?? ''
      const challenge = params.code_challenge ?? ''
      const method = params.code_challenge_method ?? 'S256'
      const client = oauthClients.get(clientId)
      if (!client || !client.redirectUris.includes(redirectUri) || !challenge || method !== 'S256') {
        sendJson(res, 400, { error: 'invalid_request', error_description: 'unknown client_id, redirect_uri, or missing PKCE S256' })
        return true
      }

      if (req.method === 'GET') {
        const st = authState(req)
        res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' })
        res.end(`<!doctype html><html><head><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1"><title>Authorize ${escHtml(client.clientName)} — nektar</title>
<style>body{background:#120e0c;color:#ece7e4;font:15px/1.5 system-ui,sans-serif;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0;padding:16px}
.box{background:#211b17;border:1px solid #3a2f28;border-radius:14px;padding:24px;max-width:420px;width:100%}
h1{font-size:18px;margin:0 0 8px;color:#f7991b}p{color:#a89a8e;font-size:13.5px}
input{width:100%;box-sizing:border-box;padding:10px 12px;border-radius:8px;border:1px solid #3a2f28;background:#120e0c;color:#ece7e4;margin:10px 0 14px;font-family:ui-monospace,monospace}
button{width:100%;padding:11px;border:0;border-radius:8px;background:#f7991b;color:#221e1d;font-weight:700;cursor:pointer}</style></head>
<body><form class=box method=POST action="/oauth/authorize">
<h1>Connect ${escHtml(client.clientName)}</h1>
<p>Allow this client to access your nektar MCP memory tools at <code>${escHtml(redirectUri)}</code>.</p>
<input type=hidden name=client_id value="${escHtml(clientId)}">
<input type=hidden name=redirect_uri value="${escHtml(redirectUri)}">
<input type=hidden name=state value="${escHtml(state)}">
<input type=hidden name=code_challenge value="${escHtml(challenge)}">
<input type=hidden name=code_challenge_method value="S256">
${st.authed ? '<p style="color:#4cc26b">✓ Authenticated via active nektar session</p>' : '<label style="font-size:12.5px;color:#a89a8e">Nektar access token (NEKTAR_HTTP_TOKEN)</label><input type=password name=token required placeholder="paste token from startup banner">'}
<button type=submit>Approve Connection</button></form></body></html>`)
        return true
      }

      const st = authState(req)
      const role: 'agent' | 'operator' | undefined = st.authed
        ? st.operator
          ? 'operator'
          : 'agent'
        : params.token
          ? verifyPresentedSecret(params.token.trim())
          : undefined
      if (!role) {
        sendJson(res, 401, { error: 'access_denied', error_description: 'invalid nektar token' })
        return true
      }
      const code = randomBytes(24).toString('base64url')
      oauthCodes.set(code, { clientId, redirectUri, codeChallenge: challenge, role, expiresAt: Date.now() + OAUTH_CODE_TTL_MS })
      const dest = new URL(redirectUri)
      dest.searchParams.set('code', code)
      if (state) dest.searchParams.set('state', state)
      res.writeHead(302, { location: dest.toString() })
      res.end()
      return true
    }

    // 7. OAuth Token Exchange (PKCE S256 verification)
    if (req.method === 'POST' && url.pathname === '/oauth/token') {
      const params = await readFormOrJson(req, readBody)
      if (params.grant_type !== 'authorization_code') {
        sendJson(res, 400, { error: 'unsupported_grant_type' })
        return true
      }
      const code = params.code ?? ''
      const entry = oauthCodes.get(code)
      oauthCodes.delete(code)
      if (!entry || entry.expiresAt < Date.now()) {
        sendJson(res, 400, { error: 'invalid_grant', error_description: 'expired or unknown code' })
        return true
      }
      if (entry.clientId !== params.client_id || entry.redirectUri !== params.redirect_uri) {
        sendJson(res, 400, { error: 'invalid_grant', error_description: 'client or redirect mismatch' })
        return true
      }
      const verifier = params.code_verifier ?? ''
      const computed = createHash('sha256').update(verifier).digest('base64url')
      if (!verifier || !safeEq(computed, entry.codeChallenge)) {
        sendJson(res, 400, { error: 'invalid_grant', error_description: 'PKCE code_verifier mismatch' })
        return true
      }
      const accessToken = `nkt_at_${randomBytes(32).toString('base64url')}`
      oauthTokens.set(accessToken, { role: entry.role, expiresAt: Date.now() + OAUTH_TOKEN_TTL_SEC * 1000 })
      sendJson(res, 200, { access_token: accessToken, token_type: 'Bearer', expires_in: OAUTH_TOKEN_TTL_SEC })
      return true
    }

    return false
  }

  return { issuePairingCode, authState, handleRoute }
}

// openapi.ts — OpenAPI 3.1 route specification for the nektar HTTP surface.
// Kept in its own module so http.ts stays well below the god-file LOC cap.
import { TOOLS } from './server.js'

export const OPENAPI = {
  openapi: '3.1.0',
  info: {
    title: 'nektar API',
    version: '1.0.0',
    description: 'Verifiable encrypted agent memory on BSV. Proofs establish inclusion, integrity and existence-by-time — never truth.'
  },
  paths: {
    '/mcp': {
      post: {
        summary: 'MCP Streamable HTTP (JSON-RPC; initialize creates a session, mcp-session-id header thereafter)',
        responses: { 200: { description: 'JSON or SSE stream' } }
      },
      get: { summary: 'MCP server-to-client SSE stream for a session', responses: { 200: { description: 'SSE' } } },
      delete: { summary: 'end an MCP session', responses: { 200: { description: 'closed' } } }
    },
    '/a2a': {
      post: { summary: 'A2A JSON-RPC task endpoint (tasks/send, message/send)', responses: { 200: { description: 'A2A Task' } } }
    },
    '/pair': {
      get: { summary: 'redeem one-time browser pairing code into HttpOnly session cookie', responses: { 302: { description: 'redirect to /' } } }
    },
    '/v1/auth/status': { get: { summary: 'browser auth state (no secrets)', responses: { 200: { description: 'auth state' } } } },
    '/v1/auth/session': {
      post: { summary: 'exchange bearer token for HttpOnly session cookie', responses: { 200: { description: 'paired' } } },
      delete: { summary: 'clear session cookie', responses: { 200: { description: 'cleared' } } }
    },
    '/.well-known/oauth-protected-resource': { get: { summary: 'MCP OAuth 2.1 protected resource metadata', responses: { 200: { description: 'RFC 9728' } } } },
    '/.well-known/oauth-authorization-server': { get: { summary: 'MCP OAuth 2.1 server metadata', responses: { 200: { description: 'RFC 8414' } } } },
    '/oauth/register': { post: { summary: 'MCP OAuth 2.1 dynamic client registration', responses: { 201: { description: 'registered' } } } },
    '/oauth/authorize': { get: { summary: 'MCP OAuth 2.1 approval page', responses: { 200: { description: 'HTML' } } } },
    '/oauth/token': { post: { summary: 'MCP OAuth 2.1 PKCE token exchange', responses: { 200: { description: 'access token' } } } },
    '/v1/proxy/{provider}': {
      post: { summary: 'L0 transparent LLM memory proxy (openai | anthropic)', responses: { 200: { description: 'proxied LLM response' } } }
    },
    '/v1/onboard/faucet': { post: { summary: '1-click testnet faucet drip + auto fundingImport', responses: { 200: { description: 'dripped' } } } },
    '/v1/onboard/policy-preset': { post: { summary: 'operator-gated starter policy preset approval', responses: { 200: { description: 'approved' } } } },
    '/v1/onboard/hub-bundle': { get: { summary: 'pre-filled Hub CLI + Cloud-Init + Coolify templates', responses: { 200: { description: 'bundle' } } } },
    '/v1/adapters/{kind}': {
      get: {
        summary: 'bundled native adapter downloads (manifest, skill.md, bundle/hermes, bundle/openclaw)',
        responses: { 200: { description: 'adapter data' } }
      }
    },
    '/v1/health': { get: { summary: 'liveness', responses: { 200: { description: 'ok' } } } },
    '/v1/hosts': {
      post: {
        summary: 'join a market host — probes /.well-known/nektar, persists to hosts.json and hot-adds running lanes. Operator-gated.',
        requestBody: { content: { 'application/json': { schema: { type: 'object', properties: { url: { type: 'string' } }, required: ['url'] } } } },
        responses: { 200: { description: 'joined host + discovered peers' }, 400: { description: 'bad url' }, 502: { description: 'no nektar there' } }
      },
      delete: {
        summary: 'leave a market host — drops saved join or blocks env/seed host persistently. Operator-gated.',
        parameters: [{ name: 'url', in: 'query', required: true, schema: { type: 'string' } }],
        responses: { 200: { description: 'removed' }, 400: { description: 'url required' }, 404: { description: 'not a market host' } }
      }
    },
    '/v1/faucet': {
      post: {
        summary: 'self-serve testnet drip — {address} → {txid, sats} (opt-in NEKTAR_FAUCET=1, never on main)',
        requestBody: { content: { 'application/json': { schema: { type: 'object', properties: { address: { type: 'string' } }, required: ['address'] } } } },
        responses: { 200: { description: 'dripped' }, 400: { description: 'bad address' }, 404: { description: 'disabled' }, 429: { description: 'capped' } }
      }
    },
    '/v1/openapi.json': { get: { summary: 'this document', responses: { 200: { description: 'OpenAPI 3.1' } } } },
    '/v1/metrics': { get: { summary: 'Prometheus metrics (text/plain 0.0.4)', responses: { 200: { description: 'metrics' } } } },
    '/v1/tools': { get: { summary: 'list tools (same as MCP tools/list)', responses: { 200: { description: 'tool schemas' } } } },
    '/v1/tools/{name}': {
      post: {
        summary: 'call a tool (same semantics as MCP tools/call)',
        parameters: [{ name: 'name', in: 'path', required: true, schema: { type: 'string', enum: TOOLS.map((t) => t.name) } }],
        requestBody: { content: { 'application/json': { schema: { type: 'object' } } } },
        responses: { 200: { description: 'tool result' }, 400: { description: 'tool error with reason' } }
      }
    },
    '/v1/proof/{shard}/{seq}': {
      get: {
        summary: 'public proof bundle + verification result for one record',
        parameters: [
          { name: 'shard', in: 'path', required: true, schema: { type: 'string' } },
          { name: 'seq', in: 'path', required: true, schema: { type: 'integer' } }
        ],
        responses: { 200: { description: 'bundle + result' } }
      }
    },
    '/v1/zkproof/{commit}': {
      get: {
        summary: 'a committed ZK proof artifact by sha256',
        parameters: [{ name: 'commit', in: 'path', required: true, schema: { type: 'string' } }],
        responses: { 200: { description: 'proof bytes' }, 404: { description: 'unknown/mismatched commit' } }
      }
    },
    '/v1/checkpoint/{shard}': {
      get: {
        summary: 'latest signed checkpoint of the shard (LOG-007)',
        parameters: [{ name: 'shard', in: 'path', required: true, schema: { type: 'string' } }],
        responses: { 200: { description: 'checkpoint + log pubkey' }, 404: { description: 'no anchor yet' } }
      }
    },
    '/.well-known/nektar': {
      get: {
        summary: 'capability/pack discovery doc (NEKTAR_PACK_SHARE=0 disables)',
        responses: { 200: { description: 'discovery doc' }, 404: { description: 'disabled' } }
      }
    },
    '/v1/pack/{shard}': {
      get: {
        summary: 'serve a public shard as a portable pack',
        parameters: [{ name: 'shard', in: 'path', required: true, schema: { type: 'string' } }],
        responses: { 200: { description: 'ShardPack JSON' }, 403: { description: 'private/gated' }, 404: { description: 'unknown' } }
      }
    },
    '/v1/verify': {
      get: {
        summary: 'scan a receipt token — traffic-light JSON or HTML verification page with QR',
        parameters: [{ name: 'r', in: 'query', required: true, schema: { type: 'string' } }],
        responses: { 200: { description: 'check list + ok flag' }, 400: { description: 'missing/malformed token' } }
      }
    },
    '/v1/badge': {
      get: {
        summary: 'verification label as an SVG badge',
        parameters: [{ name: 'r', in: 'query', required: true, schema: { type: 'string' } }],
        responses: { 200: { description: 'image/svg+xml badge' }, 400: { description: 'missing token' } }
      }
    },
    '/.well-known/x402': {
      get: { summary: 'payment catalog — priced tools, scheme and network', responses: { 200: { description: 'catalog' }, 404: { description: 'disabled' } } }
    },
    '/.well-known/agent.json': {
      get: { summary: 'A2A-compatible agent card — MCP, proxy, A2A, REST endpoints and capabilities', responses: { 200: { description: 'agent card' } } }
    },
    '/card': {
      get: { summary: 'public HTML storefront rendering the agent discovery doc', responses: { 200: { description: 'text/html agent card' } } }
    },
    '/': { get: { summary: 'operator dashboard shell (data endpoints bearer/session-gated)', responses: { 200: { description: 'text/html dashboard' } } } },
    '/state': { get: { summary: 'dashboard live state JSON', responses: { 200: { description: 'state JSON' } } } },
    '/assets/{file}': {
      get: {
        summary: 'compiled dashboard bundle assets',
        parameters: [{ name: 'file', in: 'path', required: true, schema: { type: 'string' } }],
        responses: { 200: { description: 'asset bytes' }, 404: { description: 'not found' } }
      }
    },
    '/v1/pay/quote': {
      get: {
        summary: 'payment invoice for a paid tool (NEKTAR_PAYMENTS=1)',
        parameters: [{ name: 'tool', in: 'query', required: true, schema: { type: 'string', enum: ['anchor', 'groundedAnswer', 'commitReceipt'] } }],
        responses: { 402: { description: 'PaymentRequired invoice' }, 404: { description: 'disabled' } }
      }
    },
    '/v1/automation/health': {
      get: { summary: 'public automation preflight pass boolean', responses: { 200: { description: '{pass}' }, 404: { description: 'off' } } }
    },
    '/v1/automation/preflight': {
      get: { summary: 'full preflight report (token-gated)', responses: { 200: { description: 'report' }, 404: { description: 'off' } } }
    },
    '/v1/automation/state': {
      get: { summary: 'operation + pending-approval snapshot', responses: { 200: { description: 'snapshot' }, 404: { description: 'off' } } }
    },
    '/v1/automation/approvals/{id}': {
      post: {
        summary: 'decide a pending approval — operator-gated',
        parameters: [{ name: 'id', in: 'path', required: true, schema: { type: 'string' } }],
        responses: { 200: { description: 'decision recorded' }, 403: { description: 'operator required' } }
      }
    },
    '/v1/wallet': {
      get: { summary: 'fee wallet: address, balance, spend caps, runway', responses: { 200: { description: 'wallet ops' }, 404: { description: 'disabled' } } }
    },
    '/v1/costs': { get: { summary: 'broadcast cost ledger entries', responses: { 200: { description: 'cost records' }, 404: { description: 'disabled' } } } }
  }
} as const

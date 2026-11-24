// mcp-bridge.ts — transparent MCP stdio ↔ Streamable HTTP bridge ("one brain,
// many mouths"). When a local nektar daemon is already live on loopback, a
// second AI app spawning nektar over stdio attaches here instead of opening
// NEKTAR_DATA_DIR and colliding on instance.lock or cold-starting Restate.
import type { Readable, Writable } from 'node:stream'
import type { JSONRPCMessage } from '@modelcontextprotocol/sdk/types.js'
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js'
import { StreamableHTTPClientTransport } from '@modelcontextprotocol/sdk/client/streamableHttp.js'
import { deadline } from '@nektar/core'

export interface BridgeOptions {
  /** Streamable HTTP MCP endpoint, e.g. http://127.0.0.1:18420/mcp */
  mcpUrl: string
  token?: string
  stdin?: Readable
  stdout?: Writable
  log?: (line: string) => void
  onClose?: () => void
}

export async function serveStdioBridge(opts: BridgeOptions): Promise<{ close: () => Promise<void> }> {
  const log = opts.log ?? (() => {})
  const stdio = new StdioServerTransport(opts.stdin, opts.stdout)
  const headers: Record<string, string> = {}
  if (opts.token) headers.authorization = `Bearer ${opts.token}`
  const remote = new StreamableHTTPClientTransport(new URL(opts.mcpUrl), {
    requestInit: Object.keys(headers).length > 0 ? { headers } : undefined
  })

  let closed = false
  const close = async (): Promise<void> => {
    if (closed) return
    closed = true
    await deadline(() => remote.terminateSession().catch(() => {}), 2_000)
    await Promise.allSettled([remote.close(), stdio.close()])
    opts.onClose?.()
  }

  stdio.onmessage = (msg: JSONRPCMessage) => {
    void remote.send(msg).catch((e: unknown) => {
      const errMsg = (e as Error).message
      log(`bridge forward error: ${errMsg}`)
      if ('id' in msg && msg.id !== undefined) {
        void stdio
          .send({
            jsonrpc: '2.0',
            id: msg.id,
            error: { code: -32000, message: `nektar bridge error: ${errMsg}` }
          } as JSONRPCMessage)
          .catch(() => {})
      }
    })
  }
  remote.onmessage = (msg: JSONRPCMessage) => {
    void stdio.send(msg).catch((e: unknown) => {
      log(`bridge reply error: ${(e as Error).message}`)
    })
  }
  stdio.onerror = (e: Error) => log(`bridge stdio error: ${e.message}`)
  remote.onerror = (e: Error) => log(`bridge http error: ${e.message}`)
  stdio.onclose = () => {
    void close()
  }
  remote.onclose = () => {
    void close()
  }

  await remote.start()
  await stdio.start()
  log(`bridged stdio → ${opts.mcpUrl}`)
  return { close }
}

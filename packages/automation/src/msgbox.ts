// msgbox.ts — the standard MessageBox delivery lane (@bsv/message-box-client).
//
// What this is: a second transport for market messages, the reach plane.
// The lightweb box lane is the fast path on our own hosts — the standard
// box is how a deal note reaches ANY BRC-100 identity with zero nektar
// infrastructure on their side: store-and-forward for absent peers, the
// body encrypted end-to-end by the client, the recipient's own host found
// by overlay lookup (networkPreset) rather than configured.
//
// What it is NOT: a replacement for the carrier plane. Messages ride an
// authenticated POST to the host (no on-chain tx, no carrier funding) —
// wants/offers still publish through lanes. In a composite it fans the
// send across both and merges the reads; `collect` dedups cross-lane
// delivery on a canonical key (engine ingest is idempotent on deal ids,
// so a rare miss costs one redundant pass, never a double transition).
import { MessageBoxClient } from '@bsv/message-box-client'
import type { WalletInterface } from '@bsv/sdk'
import { nektarEnv, nektarEnvFlag } from '@nektar/core'
import type { MarketMessage } from './market.js'
import type { BoxRef } from '@nektar/bstack'

export interface MsgsShape {
  send: (toPub: string, msg: MarketMessage, refs?: BoxRef[]) => Promise<void>
  collect: () => Promise<Array<{ from: string; msg: MarketMessage; refs?: BoxRef[] }>>
  ack?: () => Promise<void>
}

/** the surface the adapter uses — the real MessageBoxClient satisfies it;
 *  tests inject a stub (the package client needs a live wallet + host). */
export interface MsgBoxClientLike {
  sendMessage: (args: { recipient: string; messageBox: string; body: unknown }) => Promise<unknown>
  listMessages: (args: { messageBox: string }) => Promise<Array<{ messageId: string; sender: string; body: unknown }>>
  acknowledgeMessage: (args: { messageIds: string[] }) => Promise<unknown>
}

export const MSGBOX = 'nektar-market'

/** the market box as a Msgs surface. `send` wraps the note in {market,refs}
 *  (the same body the lightweb envelope carries); `collect` holds the ids
 *  it delivered until `ack` consumes them — acknowledging deletes the rows
 *  host-side, the protocol's own receipt. */
export function msgboxMsgs(args: { client: MsgBoxClientLike; box?: string }): MsgsShape {
  const box = args.box ?? MSGBOX
  let held: string[] = []
  return {
    send: async (to, msg, refs) => {
      const body: { market: MarketMessage; refs?: BoxRef[] } = { market: msg }
      if (refs !== undefined) body.refs = refs
      await args.client.sendMessage({ recipient: to, messageBox: box, body })
    },
    collect: async () => {
      const rows = await args.client.listMessages({ messageBox: box })
      const out: Array<{ from: string; msg: MarketMessage; refs?: BoxRef[] }> = []
      const ids: string[] = []
      for (const m of rows) {
        const body = m.body as { market?: MarketMessage; refs?: BoxRef[] }
        if (body === null || typeof body !== 'object' || body.market === undefined || typeof body.market !== 'object') continue
        ids.push(m.messageId)
        out.push({ from: m.sender, msg: body.market, refs: body.refs })
      }
      held = ids
      return out
    },
    ack: async () => {
      const ids = held
      held = []
      if (ids.length > 0) await args.client.acknowledgeMessage({ messageIds: ids })
    }
  }
}

/** canonical key for cross-lane dedup — the same note serialized by JCS on
 *  one lane and plain JSON on the other can arrive with different member
 *  order; sort keys so both forms hash alike. */
const canon = (v: unknown): string => {
  if (v === null || typeof v !== 'object') return JSON.stringify(v)
  if (Array.isArray(v)) return `[${v.map(canon).join(',')}]`
  return `{${Object.keys(v as Record<string, unknown>)
    .sort()
    .map((k) => `${JSON.stringify(k)}:${canon((v as Record<string, unknown>)[k])}`)
    .join(',')}}`
}

/** two lanes as one: a send fires on BOTH (one delivered lane is a
 *  delivery — both must fail before the row retries), collect merges and
 *  dedups on canonical message identity, ack covers both. Primary-first
 *  ordering keeps our fast lane's items ahead. */
export function compositeMsgs(primary: MsgsShape, secondary: MsgsShape): MsgsShape {
  return {
    send: async (to, msg, refs) => {
      const r = await Promise.allSettled([primary.send(to, msg, refs), secondary.send(to, msg, refs)])
      if (r.every((s) => s.status === 'rejected')) throw (r[0] as PromiseRejectedResult).reason
    },
    collect: async () => {
      const [a, b] = await Promise.all([primary.collect(), secondary.collect()])
      const seen = new Set(a.map((i) => `${i.from}:${canon(i.msg)}`))
      return [...a, ...b.filter((i) => !seen.has(`${i.from}:${canon(i.msg)}`))]
    },
    ack: async () => {
      await Promise.all([primary.ack?.(), secondary.ack?.()])
    }
  }
}

/** open the standard lane or stay inert. Enabled by cfg.msgbox or
 *  NEKTAR_MSGBOX=1; the real client needs a reachable host — networkPreset
 *  covers main/testnet, a regtest/custom net must name NEKTAR_MSGBOX_HOST
 *  explicitly (no standard boxes exist there). Returns undefined when the
 *  lane is disabled or has nowhere to point. */
export function msgboxLane(args: {
  wallet: Pick<WalletInterface, 'getPublicKey' | 'createSignature' | 'encrypt' | 'decrypt' | 'createHmac'>
  network: string
  enabled?: boolean
  host?: string
  box?: string
  client?: MsgBoxClientLike
  env?: NodeJS.ProcessEnv
}): MsgsShape | undefined {
  const enabled = args.enabled ?? nektarEnvFlag('NEKTAR_MSGBOX', args.env)
  if (!enabled) return undefined
  const host = args.host ?? nektarEnv('NEKTAR_MSGBOX_HOST', args.env)
  const preset = args.network === 'main' ? 'mainnet' : args.network === 'testnet' ? 'testnet' : undefined
  const client =
    args.client ??
    (preset !== undefined || host !== undefined
      ? (new MessageBoxClient({
          walletClient: args.wallet as WalletInterface,
          ...(preset !== undefined ? { networkPreset: preset } : {}),
          ...(host !== undefined ? { host } : {})
        }) as unknown as MsgBoxClientLike)
      : undefined)
  if (client === undefined) return undefined
  return msgboxMsgs({ client, box: args.box ?? nektarEnv('NEKTAR_MSGBOX_NAME', args.env) ?? MSGBOX })
}

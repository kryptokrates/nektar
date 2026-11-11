// p2p.ts — minimal BSV p2p listener: your own mempool eyes, no node, no index.
//
// Connects to public nodes, completes the version handshake, then watches inv
// announcements (the network's native push channel). Matched items are fetched
// via getdata and emitted — watched txids, watched outpoints (spend events),
// watched locking scripts (payments to us), and block announcements.
// Everything else is dropped: we carry a watched SET, never a global index.
//
// Scale honesty: at ordinary volume the getdata flood is feasible; at Teranode-
// scale bursts it falls behind — swap this EventSource for Kafka/Teranode then.
import { createConnection, type Socket } from 'node:net'
import { EventEmitter } from 'node:events'
import { Hash, Transaction } from '@bsv/sdk'

interface P2PEvents {
  'txid': (txid: string) => void // every announced txid (cheap)
  'tx': (tx: Transaction) => void // a fetched tx that matched a watch set
  'spend': (txid: string, vout: number, spentBy: string) => void // watched outpoint spent
  'pay': (txid: string, vout: number, satoshis: number) => void // output to watched script
  'block': (hash: string) => void
  'peer': (addr: string, up: boolean) => void
  'error': (e: Error) => void
}

export interface P2POptions {
  peers: string[] // 'host:port'
  magic?: number // mainnet default; regtest/teranode differs
  fetchAll?: boolean // getdata every announced tx (the full-mempool mirror)
  txids?: Set<string> // txids to fetch+emit on announce
  outpoints?: Set<string> // 'txid:vout' — emit 'spend' when an input matches
  scripts?: Set<string> // lockingScript hex — emit 'pay' when an output matches
  maxTxFetchPerSec?: number // safety cap on getdata volume
}

const BSV_MAGIC = 0xe8f3e1e3
const dsha = (b: Buffer) => Buffer.from(Hash.hash256([...b]))
const pad12 = (s: string) => Buffer.concat([Buffer.from(s, 'ascii'), Buffer.alloc(12)]).subarray(0, 12)

function varint (n: number): Buffer {
  if (n < 0xfd) return Buffer.from([n])
  if (n <= 0xffff) { const b = Buffer.alloc(3); b[0] = 0xfd; b.writeUInt16LE(n, 1); return b }
  const b = Buffer.alloc(5); b[0] = 0xfe; b.writeUInt32LE(n, 1); return b
}
function readVarint (b: Buffer, o: number): [number, number] {
  const f = b[o]!
  if (f < 0xfd) return [f, o + 1]
  if (f === 0xfd) return [b.readUInt16LE(o + 1), o + 3]
  if (f === 0xfe) return [b.readUInt32LE(o + 1), o + 5]
  return [Number(b.readBigUInt64LE(o + 1)), o + 9]
}

function msg (command: string, payload: Buffer, magic: number): Buffer {
  const h = Buffer.alloc(24)
  h.writeUInt32LE(magic, 0)
  pad12(command).copy(h, 4)
  h.writeUInt32LE(payload.length, 16)
  dsha(payload).subarray(0, 4).copy(h, 20)
  return Buffer.concat([h, payload])
}

function netaddr (ip = '0.0.0.0', port = 0): Buffer {
  const b = Buffer.alloc(26)
  const v4 = ip.split('.').map(Number)
  Buffer.from([0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, ...v4]).copy(b, 8)
  b.writeUInt16BE(port, 24)
  return b
}

function versionPayload (selfPort = 0): Buffer {
  const ua = Buffer.from('/nektar:0.1.0/')
  return Buffer.concat([
    (() => { const b = Buffer.alloc(4); b.writeInt32LE(70015); return b })(), // version
    (() => { const b = Buffer.alloc(8); b.writeBigUInt64LE(1n); return b })(), // services: NODE_NETWORK (peers deprioritize getdata for leaf clients)
    (() => { const b = Buffer.alloc(8); b.writeBigInt64LE(BigInt(Math.floor(Date.now() / 1000))); return b })(),
    netaddr(), netaddr('0.0.0.0', selfPort),
    (() => { const b = Buffer.alloc(8); b.writeBigUInt64LE(BigInt(Math.floor(Math.random() * 2 ** 32))); return b })(),
    varint(ua.length), ua,
    (() => { const b = Buffer.alloc(4); b.writeInt32LE(0); return b })(), // start_height
    Buffer.from([1]) // relay: true — inv announcements are the whole point
  ])
}

export class P2PListener extends EventEmitter {
  private sock?: Socket
  private buf = Buffer.alloc(0)
  private peerIdx = 0
  private closed = false
  private fetchWindow = 0
  private fetchCount = 0
  readonly opts: Required<Omit<P2POptions, 'peers' | 'txids' | 'outpoints' | 'scripts'>> & Pick<P2POptions, 'txids' | 'outpoints' | 'scripts'>
  private peers: string[]

  constructor (opts: P2POptions) {
    super()
    this.peers = opts.peers
    this.opts = {
      magic: opts.magic ?? BSV_MAGIC,
      fetchAll: opts.fetchAll ?? false,
      maxTxFetchPerSec: opts.maxTxFetchPerSec ?? 50,
      txids: opts.txids, outpoints: opts.outpoints, scripts: opts.scripts
    }
    setInterval(() => { this.fetchWindow = Date.now(); this.fetchCount = 0 }, 1000).unref()
  }

  connect (): void {
    if (this.closed) return
    const [host, port] = this.peers[this.peerIdx % this.peers.length]!.split(':')
    this.peerIdx++
    const sock = createConnection({ host: host!, port: Number(port ?? 8333), timeout: 15_000 })
    this.sock = sock
    let handshook = false
    sock.on('connect', () => {
      sock.write(msg('version', versionPayload(), this.opts.magic))
    })
    sock.on('data', (d) => {
      this.buf = Buffer.concat([this.buf, d])
      for (;;) {
        if (this.buf.length < 24) return
        if (this.buf.readUInt32LE(0) !== this.opts.magic) { sock.destroy(); return }
        const len = this.buf.readUInt32LE(16)
        if (this.buf.length < 24 + len) return
        const cmd = this.buf.subarray(4, 16).toString('ascii').replace(/\0+$/, '')
        const payload = this.buf.subarray(24, 24 + len)
        const cksum = this.buf.subarray(20, 24)
        if (!dsha(payload).subarray(0, 4).equals(cksum)) { sock.destroy(); return }
        this.buf = this.buf.subarray(24 + len)
        try { this.onMsg(sock, cmd, payload) } catch { /* keep going */ }
      }
    })
    sock.on('timeout', () => sock.destroy())
    sock.on('close', () => {
      this.emit('peer', `${host}:${port}`, false)
      if (!this.closed) setTimeout(() => this.connect(), 5_000)
    })
    sock.on('error', (e) => this.emit('error', e))
    const onHandshake = () => { if (!handshook) { handshook = true; this.emit('peer', `${host}:${port}`, true) } }
    this.once('_verack', onHandshake)
  }

  close (): void { this.closed = true; this.sock?.destroy() }

  private onMsg (sock: Socket, cmd: string, p: Buffer): void {
    switch (cmd) {
      case 'verack': this.emit('_verack'); return
      case 'version': sock.write(msg('verack', Buffer.alloc(0), this.opts.magic)); return
      case 'ping': sock.write(msg('pong', p, this.opts.magic)); return
      case 'inv': {
        let [count, o] = readVarint(p, 0)
        while (count-- > 0 && o + 36 <= p.length) {
          const type = p.readUInt32LE(o)
          const hashLE = Buffer.from(p.subarray(o + 4, o + 36))
          const id = Buffer.from(hashLE).reverse().toString('hex')
          o += 36
          if (type === 2) this.emit('block', id)
          else if (type === 1) {
            this.emit('txid', id)
            const want = this.opts.fetchAll || this.opts.txids?.has(id)
            if (want && this.fetchCount < this.opts.maxTxFetchPerSec) {
              this.fetchCount++
              const gd = Buffer.concat([varint(1), (() => { const b = Buffer.alloc(4); b.writeUInt32LE(1); return b })(), hashLE])
              sock.write(msg('getdata', gd, this.opts.magic))
            }
          }
        }
        return
      }
      case 'tx': {
        let tx: Transaction
        try { tx = Transaction.fromHex(p.toString('hex')) } catch (pe) { this.emit('parsefail', String((pe as Error).message ?? pe).slice(0, 120)); return }
        this.emit('fetched', tx.id('hex'))
        const txid = tx.id('hex')
        let matched = this.opts.txids?.has(txid) === true
        for (const i of tx.inputs) {
          const st = (i as any).sourceTXID ?? (i as any).sourceTxid
          const sv = i.sourceOutputIndex
          if (st && this.opts.outpoints?.has(`${st}:${sv}`)) {
            this.opts.outpoints.delete(`${st}:${sv}`)
            this.emit('spend', st, sv, txid); matched = true
          }
        }
        for (const [v, out] of tx.outputs.entries()) {
          if (this.opts.scripts?.has(out.lockingScript.toHex())) {
            this.emit('pay', txid, v, out.satoshis ?? 0); matched = true
          }
        }
        if (matched) this.emit('tx', tx)
        return
      }
      case 'notfound': case 'reject': case 'addr': case 'sendheaders': case 'sendcmpct':
      case 'getheaders': case 'headers': case 'feefilter': return
      default: return
    }
  }
}

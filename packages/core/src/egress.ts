// egress.ts — the shared outbound-host screen. Every fetch() whose URL is
// caller-, peer- or artifact-controlled goes through here: the codebase grew
// one screen per surface and several fetches shipped with none. Loopback,
// private, link-local (cloud metadata), multicast/reserved and non-https
// targets are refused; local rigs opt in via allowLocal (regtest endpoints
// ARE the rig's own services).

/** True when a remote URL is safe to contact: https (or http under
 *  allowHttp) and not a loopback/private/link-local/reserved target.
 *  allowLocal bypasses the address checks for dev rigs only — never pass it
 *  on a live network. */
export function egressAllowed(url: string, opts?: { allowHttp?: boolean; allowLocal?: boolean }): boolean {
  let u: URL
  try {
    u = new URL(url)
  } catch {
    return false
  }
  if (u.protocol !== 'https:' && !(opts?.allowHttp === true && u.protocol === 'http:')) return false
  if (opts?.allowLocal === true) return true
  const h = u.hostname.toLowerCase().replace(/^\[|\]$/g, '')
  if (h === 'localhost' || h.endsWith('.localhost')) return false
  // IPv6 forms: ::1 loopback, fe80::/10 link-local, fc00::/7 unique-local,
  // ::ffff:a.b.c.d v4-mapped
  if (h.includes(':')) {
    if (h === '::1' || h === '0:0:0:0:0:0:0:1') return false
    if (/^fe[89ab]/.test(h)) return false // fe80::/10 link-local
    if (/^f[cd]/.test(h)) return false // fc00::/7 unique-local
    const mapped = /^::ffff:(\d{1,3}(?:\.\d{1,3}){3})$/.exec(h)?.[1]
    if (mapped === undefined) return true // other global v6 — allowed
    return v4Allowed(mapped.split('.').map(Number))
  }
  const ip = /^\d{1,3}(\.\d{1,3}){3}$/.test(h) ? h.split('.').map(Number) : undefined
  return ip === undefined ? true : v4Allowed(ip)
}

function v4Allowed(ip: number[]): boolean {
  const [a, b] = ip
  if (a === 127 || a === 0) return false // loopback / this-host
  if (a === 10 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168)) return false // private
  if (a === 169 && b === 254) return false // link-local (metadata endpoint)
  if (a >= 224) return false // multicast/reserved
  if (ip.some((o) => o > 255)) return false
  return true
}

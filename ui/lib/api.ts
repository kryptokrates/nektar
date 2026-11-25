export type OperatorPrompter = (reason: string) => Promise<string | null>

let prompter: OperatorPrompter | null = null

export function registerOperatorPrompter(fn: OperatorPrompter) {
  prompter = fn
}

export function getStoredOperatorToken(): string | undefined {
  try {
    return sessionStorage.getItem('nektar-operator-token') ?? undefined
  } catch {
    return undefined
  }
}

export function setStoredOperatorToken(tok: string) {
  try {
    sessionStorage.setItem('nektar-operator-token', tok)
  } catch {}
}

export async function operatorFetch(
  method: string,
  url: string,
  payload?: Record<string, unknown>,
  reason = 'Operator authorization required'
): Promise<Response | null> {
  const exec = (tok?: string) =>
    fetch(url, {
      method,
      headers: {
        ...(payload !== undefined ? { 'content-type': 'application/json' } : {}),
        ...(tok ? { authorization: `Bearer ${tok}` } : {})
      },
      ...(payload !== undefined ? { body: JSON.stringify(payload) } : {})
    })

  let res = await exec(getStoredOperatorToken())
  if (res.status === 401 || res.status === 403) {
    if (!prompter) return res
    const tok = await prompter(reason)
    if (!tok) return null
    setStoredOperatorToken(tok)
    res = await exec(tok)
  }
  return res
}

export async function postJSON<T = Record<string, unknown>>(
  url: string,
  payload?: Record<string, unknown>
): Promise<{ ok: boolean; status: number; data: T & { error?: string } }> {
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(payload ?? {})
  })
  const data = (await res.json().catch(() => ({}))) as T & { error?: string }
  return { ok: res.ok, status: res.status, data }
}

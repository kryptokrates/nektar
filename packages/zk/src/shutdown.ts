// shutdownZk — terminates the snarkjs/ffjavascript BN128 worker threads
// (cached on globalThis as curve_bn128). Call at the end of test files, the
// MCP server shutdown path and e2e scripts so the process can exit cleanly.
export async function shutdownZk (): Promise<void> {
  const c = (globalThis as any).curve_bn128
  if (c && typeof c.terminate === 'function') await c.terminate()
}

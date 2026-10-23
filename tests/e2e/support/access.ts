/** Supply optional outer-gateway credentials without changing Helmr API auth. */
export function accessFetch(environment: Record<string, string | undefined>, base: typeof fetch = fetch): typeof fetch {
  const origin = environment.HELMR_ACCESS_ORIGIN
  const id = environment.HELMR_ACCESS_CLIENT_ID
  const secret = environment.HELMR_ACCESS_CLIENT_SECRET
  if (!origin && !id && !secret) return base
  if (!origin || !id || !secret) throw new Error("Access origin, client ID and secret must be supplied together")
  const target = new URL(origin)
  if (target.protocol !== "https:" || target.username || target.password || target.pathname !== "/" || target.search || target.hash) {
    throw new Error("Access credentials require an HTTPS origin")
  }
  if (![id, secret].every((value) => /^[A-Za-z0-9_.-]+$/.test(value))) throw new Error("Invalid Access credentials")
  return (async (input, init) => {
    const request = new Request(input, init)
    if (new URL(request.url).origin !== target.origin) throw new Error("Refusing Access request outside its configured origin")
    request.headers.set("CF-Access-Client-Id", id)
    request.headers.set("CF-Access-Client-Secret", secret)
    return base(new Request(request, { redirect: "error" }))
  }) as typeof fetch
}

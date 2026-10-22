import assert from "node:assert/strict"
import { createServer } from "node:http"
import { setTimeout as delay } from "node:timers/promises"
import test from "node:test"
import { Agent, request } from "undici"
import { installSessionHttp } from "./agent-http"


test("managed loopback waits retain cancellation and explicit dispatchers", async () => {
  const server = createServer(async (req, res) => {
    if (req.url === "/body") { res.writeHead(200); res.write("first"); await delay(1200); res.end("last") }
    else { await delay(1200); res.end("done") }
  })
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve))
  const url = `http://127.0.0.1:${(server.address() as { port: number }).port}`
  const uninstall = installSessionHttp()
  const explicit = new Agent({ headersTimeout: 100, bodyTimeout: 100 })
  try {
    assert.equal(await (await request(url, { headersTimeout: 100 })).body.text(), "done")
    assert.equal(await (await request(`${url}/body`, { bodyTimeout: 100 })).body.text(), "firstlast")
    await assert.rejects(fetch(url, { signal: AbortSignal.timeout(20) }), { name: "TimeoutError" })
    await assert.rejects(request(url, { dispatcher: explicit }), { code: "UND_ERR_HEADERS_TIMEOUT" })
  } finally {
    uninstall()
    await explicit.destroy()
    server.closeAllConnections()
    await new Promise<void>(resolve => server.close(() => resolve()))
  }
})

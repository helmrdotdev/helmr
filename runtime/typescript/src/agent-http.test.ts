import assert from "node:assert/strict"
import { createServer } from "node:http"
import { setTimeout as delay } from "node:timers/promises"
import test from "node:test"
import { Agent, request } from "undici"
import { installSessionHttp } from "./agent-http"
import { SessionActivity } from "./agent-activity"

const tick = () => new Promise<void>(resolve => setImmediate(resolve))

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

test("pending HTTP and callback work pin capture; expired idle transport does not", { timeout: 12_000 }, async () => {
  let release!: () => void
  const waiting = new Promise<void>(resolve => { release = resolve })
  const server = createServer(async (_req, res) => { await waiting; res.end("done") })
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve))
  const url = `http://127.0.0.1:${(server.address() as { port: number }).port}`
  const uninstall = installSessionHttp()
  const activity = new SessionActivity()
  let timer: ReturnType<typeof setInterval> | undefined
  try {
    const pending = activity.authored(async () => {
      const response = await fetch(url)
      await response.text()
      timer = setInterval(() => {}, 1000)
    })
    await tick()
    assert.throws(() => activity.assertIdle())
    release()
    await pending
    await tick()
    assert.throws(() => activity.assertIdle(), /Timeout/)
    clearInterval(timer)
    // Keep-alive sockets retain their normal expiry. They are not hidden from
    // activity tracking, nor is an in-flight request declared safe for capture.
    await delay(6500)
    await tick()
    activity.assertIdle()
  } finally {
    release()
    clearInterval(timer)
    activity.close()
    uninstall()
    server.closeAllConnections()
    await new Promise<void>(resolve => server.close(() => resolve()))
  }
})

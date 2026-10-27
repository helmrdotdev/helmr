import assert from "node:assert/strict"
import { execFileSync } from "node:child_process"
import { mkdtempSync, readFileSync, rmSync } from "node:fs"
import { createServer } from "node:https"
import { tmpdir } from "node:os"
import { join } from "node:path"
import test from "node:test"
import { denied, https, probe, socket } from "./probe.mjs"

test("blocked probe rejects absent CA trust before making network observations", async () => {
  await assert.rejects(probe({ testCA: "placeholder" }, false, true), /public test CA/)
})

test("a reachable TLS endpoint or HTTP failure cannot count as policy denial", async () => {
  const dir = mkdtempSync(join(tmpdir(), "helmr-private-probe-test-"))
  let server
  try {
    execFileSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=localhost",
      "-addext", "subjectAltName=DNS:localhost", "-keyout", join(dir, "key.pem"), "-out", join(dir, "cert.pem")], { stdio: "ignore" })
    const cert = readFileSync(join(dir, "cert.pem"), "utf8")
    server = createServer({ cert, key: readFileSync(join(dir, "key.pem")) }, (_req, res) => { res.writeHead(503); res.end("wrong-body") })
    await new Promise(resolve => server.listen(0, "127.0.0.1", resolve))
    const port = server.address().port
    await assert.rejects(denied("numeric-tls", () => socket("127.0.0.1", port, { tls: true })), /bypassed/)
    const config = { testCA: cert, business: { domain: "localhost", path: "/probe" } }
    assert.deepEqual(await https(config, port), { status: 503, body: "wrong-body" })
    await assert.rejects(denied("http-error", () => https(config, port)), /bypassed/)
    await assert.rejects(https({ ...config, testCA: "" }, port), { code: "DEPTH_ZERO_SELF_SIGNED_CERT" })
    const result = await denied("refused", async () => { const error = new Error("refused"); error.code = "ECONNREFUSED"; throw error })
    assert.equal(result.reason, "ECONNREFUSED")
  } finally {
    if (server) await new Promise(resolve => server.close(resolve))
    rmSync(dir, { recursive: true })
  }
})

// Real pinned native process, loopback MCP, synthetic credentials only.
import assert from "node:assert/strict"
import { test } from "node:test"
import { createServer } from "node:http"
import { mkdtemp, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { CodexHarness } from "../tasks/issue-fixer/codex-harness"
import { conversation } from "../tasks/issue-fixer/conversation"
import { localNativeRuntime } from "./native-runtime"

test("Codex setup passes the managed MCP endpoint and header to the native client", { timeout: 25_000 }, async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-mcp-config-"))
  const stop = new AbortController()
  const runtime = localNativeRuntime(stop.signal)
  const methods: string[] = []
  let connected!: () => void
  const listed = new Promise<void>(resolve => { connected = resolve })
  const server = createServer(async (request, response) => {
    if (request.headers.authorization !== "Bearer synthetic-local-fixture") {
      response.writeHead(401); response.end(); return
    }
    if (request.method !== "POST") { response.writeHead(405); response.end(); return }
    const chunks: Buffer[] = []
    for await (const chunk of request) chunks.push(Buffer.from(chunk))
    const body = JSON.parse(Buffer.concat(chunks).toString())
    methods.push(body.method)
    if (body.id === undefined) { response.writeHead(202); response.end(); return }
    const result = body.method === "initialize"
      ? { protocolVersion: body.params.protocolVersion, capabilities: { tools: {} }, serverInfo: { name: "fixture", version: "1" } }
      : { tools: [] }
    response.writeHead(200, { "content-type": "application/json" })
    response.end(JSON.stringify({ jsonrpc: "2.0", id: body.id, result }))
    if (body.method === "tools/list") connected()
  })
  let harness: CodexHarness | undefined
  let timer: ReturnType<typeof setTimeout> | undefined
  try {
    await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve))
    const address = server.address() as { port: number }
    const saved = await conversation(directory, "mcp-session", "codex")
    await writeFile(join(saved.directory, "config.toml"), 'model_provider = "fixture"\nmodel = "gpt-5.1-codex"\n[model_providers.fixture]\nname = "local fixture"\nbase_url = "http://127.0.0.1:1/v1"\nwire_api = "responses"\nrequires_openai_auth = false\n')
    const timeout = new Promise<never>((_, reject) => { timer = setTimeout(() => { stop.abort(); reject(new Error("Native MCP configuration timed out")) }, 15_000) })
    harness = await Promise.race([runtime.registry.setup(() => CodexHarness.open(directory, "mcp-session", { PATH: process.env.PATH, HOME: directory }, {
      url: `http://127.0.0.1:${address.port}/mcp`, headers: { Authorization: "Bearer synthetic-local-fixture" },
    })), timeout])
    await Promise.race([listed, timeout])
    assert.ok(methods.includes("initialize"))
    assert.ok(methods.includes("tools/list"))
  } finally {
    clearTimeout(timer)
    stop.abort()
    await harness?.close()
    runtime.uninstall()
    server.closeAllConnections()
    await new Promise<void>(resolve => server.close(() => resolve()))
    await rm(directory, { recursive: true, force: true })
  }
})

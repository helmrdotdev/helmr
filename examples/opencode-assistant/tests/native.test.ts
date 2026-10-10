import assert from "node:assert/strict"
import { test } from "node:test"
import { createServer } from "node:http"
import { execFileSync } from "node:child_process"
import { mkdtemp, rm, mkdir, writeFile, stat, realpath } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { SessionRuntime, type SessionDriver } from "../../../runtime/typescript/src/agent-session"
import { NativeContinuationLost } from "../../../runtime/typescript/src/agent-native"
import { localNativeRuntime } from "../../issue-fixer/probes/native-runtime"
import { OpenCodeHarness } from "../tasks/opencode"

test("native binary matches the application pin", () => {
  assert.equal(execFileSync("opencode", ["--version"], { encoding: "utf8" }).trim(), "1.18.30")
})

for (const scenario of ["question", "permission", "deny", "large-edit", "project-config", "interrupt", "error"] as const) test(`real OpenCode: ${scenario}`, { timeout: 90_000 }, async () => {
  const cwd = await realpath(await mkdtemp(join(tmpdir(), "helmr-opencode-")))
  const requests: any[] = []
  const responses: unknown[] = []
  let tools = 0, questions = 0, holds = 0, outputs = 0
  const server = createServer(async (req, res) => {
    let body = ""
    for await (const chunk of req) body += chunk
    const input = JSON.parse(body)
    requests.push(input)
    if (scenario === "error" && requests.length === 1) { tools++; res.writeHead(400, { "content-type": "application/json" }); res.end(JSON.stringify({ error: { message: "fixture model failure", type: "invalid_request_error" } })); return }
    res.writeHead(200, { "content-type": "text/event-stream" })
    const event = (delta: unknown, finish_reason: string | null = null) => res.write(`data: ${JSON.stringify({ id: "fixture", object: "chat.completion.chunk", created: 1, model: "fixture", choices: [{ index: 0, delta, finish_reason }] })}\n\n`)
    const toolName = scenario === "large-edit" ? "write" : scenario === "permission" || scenario === "deny" || scenario === "project-config" ? "bash" : "question"
    if (tools++ === 0) {
      assert.ok(input.tools.some((tool: any) => tool.function.name === toolName), `missing ${toolName} tool`)
      event({ role: "assistant", tool_calls: [{ index: 0, id: "call_fixture", type: "function", function: { name: toolName, arguments: JSON.stringify(toolName === "question" ? { questions: [{ header: "Color", question: "Which color?", options: [{ label: "Blue", description: "Use blue" }, { label: "Green", description: "Use green" }] }] } : toolName === "write" ? { filePath: join(cwd, "large.txt"), content: "A".repeat(50 * 1024) } : { command: "printf fixture-approved", description: "Print fixture text" }) } }] })
      event({}, "tool_calls")
    } else { event({ role: "assistant", content: "fixture response" }); event({}, "stop") }
    res.end("data: [DONE]\n\n")
  })
  const cancel = new AbortController()
  const native = localNativeRuntime(cancel.signal)
  let harness: OpenCodeHarness | undefined
  try {
    await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve))
    const baseURL = `http://127.0.0.1:${(server.address() as { port: number }).port}/v1`
    if (scenario === "project-config") {
      // A project plugin and agent override must not run or bypass the question.
      await mkdir(join(cwd, ".opencode", "plugins"), { recursive: true })
      await mkdir(join(cwd, ".opencode", "agents"), { recursive: true })
      await writeFile(join(cwd, ".opencode", "plugins", "fixture.ts"), `import { writeFileSync } from 'node:fs'; export const Fixture = async () => { writeFileSync(${JSON.stringify(join(cwd, "plugin-ran"))}, 'ran'); return {} }`)
      await writeFile(join(cwd, ".opencode", "agents", "build.md"), "---\nmode: primary\npermission:\n  bash: allow\n---\nFixture agent\n")
      await writeFile(join(cwd, "opencode.json"), JSON.stringify({ permission: { bash: "allow" } }))
    }
    const driver: SessionDriver = {
      agents: () => ({ session: () => { throw new Error("unused") }, list: async () => { throw new Error("unused") }, spawn: async () => { throw new Error("unused") }, start: async () => { throw new Error("unused") } }),
      registerMessages: async () => {}, closeProcessing: async () => {},
      ask: async () => {
        questions++
        if (scenario === "interrupt") {
          queueMicrotask(() => runtime.interrupt("first", new Error("fixture interrupt")))
          return { answer: new Promise<any>(() => {}), withdraw: async () => {} }
        }
        const permission = scenario === "permission" || scenario === "project-config" || scenario === "deny"
        return { answer: Promise.resolve({ answer: { selected: [{ id: permission ? scenario === "deny" ? "deny" : "allow" : "option-1", value: permission ? scenario !== "deny" : "Blue" }] }, respondedBy: { kind: "user" as const, id: "fixture" } }), withdraw: async () => {} }
      },
      output: async () => { outputs++; return { sequence: outputs } },
      respond: async (_turnId, _id, value) => { responses.push(value) },
      convergeNative: async (id, disposition, signal) => {
        await native.registry.join(id, disposition, signal.reason)
        if (!(await native.registry.evidence()).reusable) throw new NativeContinuationLost()
      },
      finalize: async (_id, result) => ({ status: "completed", result }),
      fail: async (_id, error) => ({ status: error.code === "interrupted" ? "interrupted" : "failed", error }),
      hold: async () => { holds++ },
    }
    const runtime = new SessionRuntime({
      kind: "agent", id: "fixture", computer: undefined!,
      setup: async () => {
        harness = await OpenCodeHarness.open(cwd, "same-session", {
          model: "fixture/fixture", small_model: "fixture/fixture", enabled_providers: ["fixture"],
          provider: { fixture: { npm: "@ai-sdk/openai-compatible", name: "Local fixture", options: { baseURL, apiKey: "not-a-real-key" }, models: { fixture: { name: "Fixture", limit: { context: 32000, output: 2048 } } } } },
        }, { PATH: process.env.PATH, HOME: cwd })
        return harness
      },
      turn: (turn, context) => (context.setupResult as OpenCodeHarness).run(turn, turn.input.map(part => part.text).join("")),
    }, { session: { id: "same-session" }, computer: { id: "computer" }, deployment: { id: "deployment" }, recovery: { kind: "initial" }, signal: cancel.signal }, driver, { failureConvergenceMs: 10_000, native: native.registry })
    const first = await runtime.dispatch({ id: "first", sequence: 1, createdAt: "2026-10-10T00:00:00Z", input: [{ type: "text", text: "first unique input" }], source: { kind: "api" } })
    const pid = harness!.processId, id = harness!.conversationId
    if (scenario === "interrupt" || scenario === "error") {
      assert.equal(first.status, scenario === "interrupt" ? "interrupted" : "failed", JSON.stringify(first))
      assert.equal(responses.length, 0)
      assert.equal(harness!.reusable, true)
      assert.equal(holds, 0)
      if (scenario === "interrupt") runtime.resume()
    } else assert.equal(first.status, "completed", JSON.stringify(first))
    const second = await runtime.dispatch({ id: "second", sequence: 2, createdAt: "2026-10-10T00:00:01Z", input: [{ type: "text", text: "second unique input" }], source: { kind: "api" } })
    assert.equal(second.status, "completed", JSON.stringify(second))
    assert.equal(harness!.processId, pid)
    assert.equal(harness!.conversationId, id)
    assert.equal(questions, scenario === "error" || scenario === "large-edit" ? 0 : 1)
    assert.equal(holds, 0)
    assert.equal(responses.length, scenario === "interrupt" || scenario === "error" ? 1 : 2)
    assert.ok(JSON.stringify(requests.at(-1).messages).includes("first unique input"))
    assert.ok(JSON.stringify(requests.at(-1).messages).includes("second unique input"))
    if (scenario === "permission" || scenario === "question") assert.ok(JSON.stringify(requests.at(-1).messages).includes(scenario === "permission" ? "fixture-approved" : "Blue"))
    if (scenario === "deny") assert.ok(JSON.stringify(requests.at(-1).messages).includes("rejected"))
    if (scenario === "large-edit") {
      assert.ok(JSON.stringify(requests.at(-1).messages).includes("Too large to review"))
      await assert.rejects(stat(join(cwd, "large.txt")), { code: "ENOENT" })
    }
    if (scenario === "project-config") await assert.rejects(stat(join(cwd, "plugin-ran")), { code: "ENOENT" })
    assert.equal(outputs, 0, "tool starts must not become conversation progress")
    assert.equal(runtime.idle, true)
  } finally {
    await harness?.close()
    cancel.abort()
    native.uninstall()
    server.closeAllConnections()
    await new Promise<void>(resolve => server.close(() => resolve()))
    await rm(cwd, { recursive: true, force: true })
  }
})

import assert from "node:assert/strict"
import { test } from "node:test"
import { createServer, type ServerResponse } from "node:http"
import { once } from "node:events"
import { mkdtemp, mkdir, rm, readFile, stat } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { OpenCode } from "@opencode/sdk/effect"
import { Effect, Exit, Schema, Scope } from "effect"
import type { SessionExecution } from "@opencode/core/session/execution"
import type { Session } from "@opencode/core/session"
import { Form } from "@opencode/schema/form"
import { askForm } from "../agents/questions"
import { OpenCodeSession, type OpenCodeOptions } from "../agents/opencode-bridge"
import { SessionRuntime, type SessionDriver } from "../../../runtime/typescript/src/agent-session"
import type { InitContext, Turn } from "../../../sdk/typescript/src/agent"

const delay = (ms: number) => new Promise(resolve => setTimeout(resolve, ms))
async function until(check: () => boolean) {
  for (let n = 0; n < 1000; n++) { if (check()) return; await delay(20) }
  throw new Error("Expected native activity did not occur")
}

test("OpenCode starter retains native context, joins interruption and reconstructs without old input", { timeout: 120_000 }, async () => {
  const root = await mkdtemp(join(tmpdir(), "helmr-opencode-")), cwd = join(root, "workspace")
  await mkdir(cwd)
  const calls: any[] = [], held: Array<{ response: ServerResponse; body: any }> = [], output: unknown[] = []
  const actions: Array<{ name: string; input: unknown }> = [], questions: unknown[] = []
  let hold = false, allow = true, answerQuestions = true, childMarker: string | undefined
  let questionFailure: Error | undefined
  const childHeld: Array<{ response: ServerResponse; body: any }> = []
  function reply(response: ServerResponse, body: any) {
    response.writeHead(200, { "content-type": "text/event-stream" })
    const send = (type: string, data: object) => response.write(`event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`)
    send("message_start", { message: { id: `msg_${crypto.randomUUID()}`, type: "message", role: "assistant", model: body.model, content: [], stop_reason: null, usage: { input_tokens: 10, output_tokens: 1 } } })
    const action = body.tools?.length ? actions.shift() : undefined
    if (action) {
      assert.ok(body.tools.some((tool: any) => tool.name === action.name), "native tool exists")
      send("content_block_start", { index: 0, content_block: { type: "tool_use", id: `tool_${crypto.randomUUID()}`, name: action.name, input: {} } })
      send("content_block_delta", { index: 0, delta: { type: "input_json_delta", partial_json: JSON.stringify(action.input) } })
    } else {
      send("content_block_start", { index: 0, content_block: { type: "text", text: "" } })
      send("content_block_delta", { index: 0, delta: { type: "text_delta", text: "offline reply" } })
    }
    send("content_block_stop", { index: 0 })
    send("message_delta", { delta: { stop_reason: action ? "tool_use" : "end_turn", stop_sequence: null }, usage: { output_tokens: 2 } })
    send("message_stop", {})
    response.end()
  }
  const server = createServer(async (request, response) => {
    let raw = ""
    for await (const chunk of request) raw += chunk
    const body = JSON.parse(raw || "{}")
    if (request.url?.includes("count_tokens")) { response.end(JSON.stringify({ input_tokens: 10 })); return }
    if (!request.url?.endsWith("/messages")) { response.writeHead(404); response.end(); return }
    calls.push(body)
    if (childMarker && body.messages?.some((message: any) => message.role === "user" && Array.isArray(message.content) && message.content.some((part: any) => part.type === "text" && part.text.includes(childMarker)))) { childHeld.push({ response, body }); return }
    if (hold) held.push({ response, body })
    else reply(response, body)
  })
  server.listen(0, "127.0.0.1")
  await once(server, "listening")
  const port = (server.address() as import("node:net").AddressInfo).port
  const options: OpenCodeOptions = { cwd, stateDirectory: join(root, "state"), sessionId: "test", model: "fixture/probe", introduction: "You are a fixture assistant.", recovery: "initial",
    native: { config: { directory: join(root, "config"), project: false, claude: false, content: JSON.stringify({
      agents: { title: { model: "fixture/small" } },
      permissions: [{ action: "*", resource: "*", effect: "ask" }], model: "fixture/probe", providers: { fixture: { name: "Offline model", package: "@opencode/ai/providers/anthropic", settings: { baseURL: `http://127.0.0.1:${port}/v1`, apiKey: "local-fixture" }, models: { small: { name: "Small", limit: { context: 100000, output: 1000 }, capabilities: { tools: true, input: ["text"], output: ["text"] } }, probe: { name: "Probe", limit: { context: 100000, output: 1000 }, capabilities: { tools: true, input: ["text"], output: ["text"] } } } } },
    }) }, models: { fetch: false, snapshot: false }, fs: { filewatcher: false, fff: false }, plugins: [] },
  }
  let bridge: OpenCodeSession | undefined, sequence = 0, initCount = 0
  const context: InitContext = { session: { id: "test" }, computer: { id: "computer" }, deployment: { id: "deployment" }, recovery: { kind: "initial" }, signal: new AbortController().signal, mcp: { getConnectionOptions: async () => { throw Error("unused") } } }
  const driver: SessionDriver = {
    registerMessages: async () => {}, closeProcessing: async () => {},
    ask: async (_turn, _id, question, signal) => {
      questions.push(question)
      if (questionFailure) throw questionFailure
      const answer = answerQuestions ? Promise.resolve({ answer: question.type === "text" ? "fixture answer" : { selected: [question.type === "choice" ? question.options.find(option => option.id === (allow ? "once" : "reject")) ?? question.options[0]! : undefined!] }, answeredBy: { kind: "user" as const, id: "tester" } }) : new Promise<never>((_, reject) => signal.addEventListener("abort", () => reject(signal.reason), { once: true }))
      return { answer, withdraw: async () => {} }
    },
    respond: async (_turn, _id, content) => { output.push(content) },
    output: async (_turn, _id, content) => { output.push(content); return { sequence: output.length } },
    confirmDrain: async () => {}, finalize: async (_id, result) => ({ status: "completed", result }),
    fail: async (_id, error) => { console.error("TURN_FAILED", error); return error.code === "interrupted" ? { status: "interrupted" } : { status: "failed", error } },
    hold: async () => {},
  }
  const createRuntime = () => new SessionRuntime({ kind: "agent", id: "test", template: undefined!, init: async () => { initCount++; return bridge = await OpenCodeSession.open(options) }, turn: async (turn: Turn, { initResult }) => {
    const { response, ...result } = await (initResult as OpenCodeSession).run(turn)
    if (response !== undefined) await turn.respond(response)
    return result
  } }, context, driver)
  let runtime = createRuntime()
  const dispatch = (text: string) => { const n = ++sequence; return { id: `turn-${n}`, result: runtime.dispatch({ id: `turn-${n}`, sequence: n, createdAt: new Date().toISOString(), input: [{ type: "text", text }], source: { kind: "api_key" } }) } }
  try {
    assert.equal((await dispatch("remember first input").result).status, "completed")
    await until(() => calls.some(call => call.model === "small"))
    assert.ok(calls.some(call => call.model === "probe"), "conversation uses the main model")
    assert.equal((await dispatch("second input").result).status, "completed")
    assert.equal(initCount, 1)
    assert.ok(JSON.stringify(calls.at(-1)).includes("remember first input"))
    assert.ok(JSON.stringify(calls.at(-1)).includes(options.introduction))
    // Native background requests can appear without an active Helmr Turn.
    // Exercise the native API without changing the production bridge's surface.
    const native = (bridge as unknown as { sdk: OpenCode.Interface }).sdk
    const beforePending = questions.length
    await Effect.runPromise(native.sessions.form.create({ sessionID: bridge!.nativeSessionId, title: "Between Turns", fields: Schema.decodeUnknownSync(Form.Fields)([{ type: "string", key: "answer", title: "Background question" }]) }))
    const permission = Effect.runPromise(native.permission.create({ sessionID: bridge!.nativeSessionId, action: "shell", resources: ["between-turns"] }))
    await delay(20)
    assert.equal(questions.length, beforePending)
    assert.equal((await dispatch("answer pending native requests").result).status, "completed")
    await permission
    assert.equal(questions.length, beforePending + 2)
    hold = true
    const steering = dispatch("wait for steering")
    await until(() => held.length === 1)
    await runtime.deliverMessage(steering.id, "message-1", [{ type: "text", text: "steered marker" }], { kind: "api_key" })
    hold = false
    reply(held[0]!.response, held[0]!.body)
    assert.equal((await steering.result).status, "completed")
    assert.ok(JSON.stringify(calls.at(-1)).includes("steered marker"))
    hold = true
    const cancelled = dispatch("interrupt held model")
    await until(() => held.length === 2)
    await runtime.deliverMessage(cancelled.id, "message-2", [{ type: "text", text: "cancelled pending marker" }], { kind: "api_key" })
    await runtime.interrupt(cancelled.id, new Error("stop"))
    assert.equal((await cancelled.result).status, "interrupted")
    hold = false
    assert.equal((await dispatch("fresh after cancellation").result).status, "completed")
    assert.ok(!JSON.stringify(calls.at(-1)).includes("cancelled pending marker"))
    for (const permitted of [false, true]) {
      allow = permitted
      const marker = join(cwd, `allowed-${permitted}`), before = questions.length
      actions.push({ name: "shell", input: { command: `echo approved > '${marker}'`, description: "Write fixture marker" } })
      assert.equal((await dispatch("exercise native approval").result).status, "completed")
      assert.ok(questions.length > before)
      assert.equal(await stat(marker).then(() => true, () => false), permitted)
    }
    allow = true
    const question = { name: "question", input: { questions: [{ question: "Which fixture?", header: "Fixture", options: [{ label: "A", description: "First" }, { label: "B", description: "Second" }] }] } }
    let before = questions.length
    actions.push(question)
    assert.equal((await dispatch("ask native question").result).status, "completed")
    assert.ok(questions.length > before)
    assert.ok(JSON.stringify(calls.at(-1)).includes("A"))
    questionFailure = new Error("injected question transport failure")
    actions.push(question)
    assert.deepEqual(await dispatch("fail a native question").result, { status: "failed", error: { code: "handler_failed", message: questionFailure.message } })
    questionFailure = undefined
    assert.equal((await dispatch("after question failure").result).status, "completed")
    answerQuestions = false
    before = questions.length
    actions.push(question)
    const waitingQuestion = dispatch("cancel native question")
    await until(() => questions.length > before)
    await runtime.interrupt(waitingQuestion.id, new Error("stop question"))
    assert.equal((await waitingQuestion.result).status, "interrupted")
    answerQuestions = true
    assert.equal((await dispatch("after question cancellation").result).status, "completed")
    const pidFile = join(cwd, "tool-pid"), completion = join(cwd, "tool-done")
    actions.push({ name: "shell", input: { command: `echo $$ > '${pidFile}'; sleep 30; echo done > '${completion}'`, description: "Hold fixture tool" } })
    const tool = dispatch("cancel native shell")
    let pid = 0
    for (let n = 0; n < 1000; n++) { const value = await readFile(pidFile, "utf8").catch(() => ""); if (value) { pid = Number(value.trim()); break } await delay(20) }
    assert.ok(pid > 0)
    await runtime.interrupt(tool.id, new Error("stop tool"))
    assert.equal((await tool.result).status, "interrupted")
    await until(() => { try { process.kill(pid, 0); return false } catch (error) { if ((error as NodeJS.ErrnoException).code === "ESRCH") return true; throw error } })
    assert.equal(await stat(completion).then(() => true, () => false), false)
    assert.equal((await dispatch("after tool cancellation").result).status, "completed")
    const currentNative = (bridge as unknown as { sdk: OpenCode.Interface }).sdk
    const agents = await Effect.runPromise(currentNative.agent.list({ location: { directory: cwd } }))
    const worker = agents.data.find(agent => agent.mode !== "primary")
    assert.ok(worker, "native subagent is available")
    childMarker = "hold this unique background fixture"
    actions.push({ name: "subagent", input: { agent: worker.id, description: "Background fixture", prompt: childMarker, background: true } })
    assert.equal((await dispatch("start native background work").result).status, "completed")
    await until(() => childHeld.length > 0)
    const heldBefore = held.length
    hold = true
    const parent = dispatch("cancel parent while background work remains")
    await until(() => held.length > heldBefore)
    // A background completion may admit a synthetic notification after the
    // parent accepted interruption. It must not keep cancellation waiting on
    // a fresh model call before the native host can close.
    const lifecycle = bridge as unknown as { execution: SessionExecution.Interface; sessions: Session.Interface; scope: Scope.Closeable }
    const execution = lifecycle.execution
    let notified = false
    lifecycle.execution = { ...execution, interrupt: (id, options) => execution.interrupt(id, options).pipe(Effect.tap(interrupted => {
      if (!interrupted || id !== bridge!.nativeSessionId || notified) return Effect.void
      notified = true
      return lifecycle.sessions.synthetic({ sessionID: id, text: "background completion during cancellation", resume: true }).pipe(Effect.orDie)
    })) }
    let deadline: ReturnType<typeof setTimeout> | undefined
    try {
      await Promise.race([
        runtime.interrupt(parent.id, new Error("stop background work")),
        new Promise<never>((_, reject) => { deadline = setTimeout(() => reject(new Error("cancellation waited for a restarted native run")), 5000) }),
      ])
      assert.equal(notified, true, "the native notification race was exercised")
    } catch (error) {
      // Retire the host on failure without masking the bridge's success path.
      await Effect.runPromise(Scope.close(lifecycle.scope, Exit.succeed(undefined)))
      throw error
    } finally { clearTimeout(deadline) }
    assert.equal((await parent.result).status, "interrupted")
    const afterCancel = calls.length
    await delay(100)
    assert.equal(calls.length, afterCancel, "no background notification restarts a cancelled parent")
    hold = false
    childMarker = undefined
    assert.equal((await dispatch("fresh after background cancellation").result).status, "completed")
    assert.notEqual((bridge as unknown as { sdk: OpenCode.Interface }).sdk, currentNative, "cancel reconstructs the native host")
    const beforeInvalid = calls.length
    const invalidNative = (bridge as unknown as { sdk: OpenCode.Interface }).sdk
    await Effect.runPromise(invalidNative.sessions.form.create({ sessionID: bridge!.nativeSessionId, title: "External native form", fields: Schema.decodeUnknownSync(Form.Fields)([{ type: "external", key: "outside", url: "https://invalid.example" }]) }))
    assert.equal((await dispatch("must not admit after replay failure").result).status, "failed")
    assert.equal(calls.length, beforeInvalid, "replay failure closes admission before prompting")
    assert.equal((await dispatch("fresh after rejected native form").result).status, "completed")
    await bridge!.close()
    const count = calls.length
    options.recovery = "reconstructed"
    runtime = createRuntime()
    await runtime.initialize()
    await delay(100)
    assert.equal(calls.length, count)
    assert.equal((await dispatch("fresh after reopen").result).status, "completed")
    assert.ok(JSON.stringify(calls.at(-1)).includes("remember first input"))
    assert.equal(initCount, 2)
  } finally {
    await bridge?.close()
    server.closeAllConnections()
    await new Promise<void>(resolve => server.close(() => resolve()))
    await rm(root, { recursive: true, force: true })
  }
})


test("required native form text and numbers reject blank responses", async () => {
  for (const type of ["string", "number", "integer"]) {
    const form = { fields: Schema.decodeUnknownSync(Form.Fields)([{ type, key: "required", required: true }]) } as Form.Info
    const turn = { ask: async () => ({ answer: "   " }) } as unknown as Turn
    await assert.rejects(askForm(turn, form, new AbortController().signal), /requires an answer/)
  }
})

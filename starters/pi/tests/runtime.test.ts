import assert from "node:assert/strict"
import { test } from "node:test"
import { mkdtemp, mkdir, rm, readFile, writeFile } from "node:fs/promises"
import { existsSync } from "node:fs"
import { join } from "node:path"
import { tmpdir } from "node:os"
import { createAssistantMessageEventStream, type AssistantMessage } from "@earendil-works/pi-ai"
import { DefaultResourceLoader, ModelRuntime, SettingsManager, type AgentSession } from "@earendil-works/pi-coding-agent"
import { PiSession, type PiOptions } from "../agents/pi-bridge"
import { SessionRuntime, type SessionDriver } from "../../../runtime/typescript/src/agent-session"
import type { InitContext, Turn } from "../../../sdk/typescript/src/agent"

const delay = (ms: number) => new Promise(resolve => setTimeout(resolve, ms))
async function until(check: () => boolean) { for (let n = 0; n < 500; n++) { if (check()) return; await delay(10) }; throw new Error("Expected native activity did not occur") }

test("Pi starter retains native history, joins cancellation and restores only saved context", { timeout: 60_000 }, async () => {
  const root = await mkdtemp(join(tmpdir(), "helmr-pi-")), cwd = join(root, "workspace"), agentDir = join(root, "config")
  await mkdir(cwd)
  const responses: unknown[] = []
  const calls: string[] = [], replies: Array<() => void> = [], questions: unknown[] = []
  let diagnosticFailure = false
  let toolCommand: string | undefined, replacementGate: (() => Promise<void>) | undefined
  let hold = false, answerQuestions = true, inputGate: (() => Promise<void>) | undefined
  const modelRuntime = await ModelRuntime.create({ authPath: join(root, "auth.json"), modelsPath: null, modelsStorePath: join(root, "models.json"), allowModelNetwork: false, refreshOnCreate: false })
  modelRuntime.registerProvider("fixture", { baseUrl: "https://invalid.example", api: "fixture-api", apiKey: "offline", models: [{ id: "probe", name: "Probe", reasoning: false, input: ["text"], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 100000, maxTokens: 1000 }], streamSimple: (model, context, options) => {
    calls.push(JSON.stringify(context))
    const stream = createAssistantMessageEventStream()
    const message: AssistantMessage = { role: "assistant", content: [{ type: "text", text: "offline reply" }], api: model.api, provider: model.provider, model: model.id, usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } }, stopReason: "stop", timestamp: Date.now() }
    if (toolCommand) { message.content = [{ type: "toolCall", id: crypto.randomUUID(), name: "bash", arguments: { command: toolCommand } }]; message.stopReason = "toolUse"; toolCommand = undefined }
    let ended = false
    const finish = (aborted = false) => {
      if (ended) return
      ended = true
      if (aborted) { message.stopReason = "aborted"; stream.push({ type: "error", reason: "aborted", error: message }) }
      else stream.push({ type: "done", reason: message.stopReason as "stop" | "toolUse", message })
      stream.end(message)
    }
    if (options?.signal?.aborted) finish(true)
    else { options?.signal?.addEventListener("abort", () => finish(true), { once: true }); if (hold) replies.push(() => finish()); else queueMicrotask(() => finish()) }
    return stream
  } })
  const settingsManager = SettingsManager.inMemory({ compaction: { enabled: false }, retry: { enabled: false }, cacheWarming: "off" })
  const resourceLoader = new DefaultResourceLoader({ cwd, agentDir, settingsManager, noExtensions: true, noSkills: true, noThemes: true, noContextFiles: true, appendSystemPrompt: ["You are a fixture assistant."], extensionFactories: [pi => {
    pi.on("message_end", async () => { if (diagnosticFailure) throw new Error("fixture telemetry failure") })
    pi.on("input", async () => { await inputGate?.() })
    pi.registerCommand("rewind", { description: "Navigate history", handler: async (_, ctx) => {
      const target = ctx.sessionManager.getEntries().find(entry => entry.type === "message" && entry.message.role === "assistant")
      assert.ok(target)
      await ctx.navigateTree(target.id, { summarize: false })
      await (bridge as unknown as { session: AgentSession }).session.prompt("reply after history navigation")
    } })
    pi.registerCommand("fresh", { description: "Replace session", handler: async (_, ctx) => { await ctx.newSession({ setup: async () => { await replacementGate?.() } }) } })
    pi.registerCommand("question", { description: "Ask the user", handler: async (_, ctx) => { await ctx.ui.confirm("Continue?", "Native extension request") } })
  }] })
  await resourceLoader.reload()
  const options: PiOptions = { cwd, stateDirectory: join(root, "state"), sessionId: "test", model: "fixture/probe", introduction: "You are a fixture assistant.", recovery: "initial", native: { agentDir, modelRuntime, settingsManager, resourceLoader } }
  let bridge: PiSession | undefined, sequence = 0
  const context: InitContext = { session: { id: "test" }, computer: { id: "computer" }, deployment: { id: "deployment" }, recovery: { kind: "initial" }, signal: new AbortController().signal, mcp: { getConnectionOptions: async () => { throw Error("unused") } } }
  const driver: SessionDriver = {
    registerMessages: async () => {}, closeProcessing: async () => {},
    ask: async (_turn, _id, question, signal) => {
      questions.push(question)
      const answer = answerQuestions ? Promise.resolve({ answer: question.type === "text" ? "fixture answer" : { selected: [question.type === "choice" ? question.options[0]! : undefined!] }, answeredBy: { kind: "user" as const, id: "tester" } }) : new Promise<never>((_, reject) => signal.addEventListener("abort", () => reject(signal.reason), { once: true }))
      return { answer, withdraw: async () => {} }
    },
    respond: async (_turn, _id, content) => { responses.push(content) }, output: async () => ({ sequence: 1 }), confirmDrain: async () => {}, finalize: async (_id, result) => ({ status: "completed", result }),
    fail: async (_id, error) => { console.error("TURN_FAILED", error); return error.code === "interrupted" ? { status: "interrupted" } : { status: "failed", error } }, hold: async () => {},
  }
  const createRuntime = () => new SessionRuntime({ kind: "agent", id: "test", template: undefined!, init: async () => bridge = await PiSession.open(options), turn: async (turn: Turn, { initResult }) => { const { response, ...result } = await (initResult as PiSession).run(turn); if (response) await turn.respond(response); return result } }, context, driver)
  let runtime = createRuntime()
  const dispatch = (text: string) => { const n = ++sequence; return { id: `turn-${n}`, result: runtime.dispatch({ id: `turn-${n}`, sequence: n, createdAt: new Date().toISOString(), input: [{ type: "text", text }], source: { kind: "api_key" } }) } }
  try {
    assert.equal((await dispatch("remember first input").result).status, "completed")
    assert.equal((await dispatch("second input").result).status, "completed")
    assert.ok(calls.at(-1)?.includes("remember first input"))
    assert.ok(calls.at(-1)?.includes(options.introduction))
    assert.equal((await dispatch("/rewind").result).status, "completed")
    assert.ok(JSON.stringify(responses.at(-1)).includes("offline reply"))
    diagnosticFailure = true
    assert.equal((await dispatch("recoverable diagnostic").result).status, "completed")
    diagnosticFailure = false
    hold = true
    const steering = dispatch("wait for steering")
    await until(() => replies.length === 1)
    await runtime.deliverMessage(steering.id, "message-1", [{ type: "text", text: "steered marker" }], { kind: "api_key" })
    hold = false; replies[0]!()
    assert.equal((await steering.result).status, "completed")
    assert.ok(calls.at(-1)?.includes("steered marker"))
    hold = true
    const cancelled = dispatch("interrupt held model")
    await until(() => replies.length === 2)
    await runtime.deliverMessage(cancelled.id, "message-2", [{ type: "text", text: "cancelled pending marker" }], { kind: "api_key" })
    const native = (bridge as unknown as { session: AgentSession }).session
    await native.followUp("cancelled followup marker")
    await runtime.interrupt(cancelled.id, new Error("stop"))
    assert.equal((await cancelled.result).status, "interrupted")
    hold = false
    assert.equal((await dispatch("fresh after cancellation").result).status, "completed")
    assert.ok(!calls.at(-1)?.includes("cancelled pending marker"))
    assert.ok(!calls.at(-1)?.includes("cancelled followup marker"))
    assert.equal((await dispatch("/question").result).status, "completed")
    assert.equal(questions.length, 1)
    answerQuestions = false
    const question = dispatch("/question")
    await until(() => questions.length === 2)
    await runtime.interrupt(question.id, new Error("stop question"))
    assert.equal((await question.result).status, "interrupted")
    answerQuestions = true
    assert.equal((await dispatch("after question").result).status, "completed")
    const marker = join(cwd, "tool.pid"), finished = join(cwd, "tool.finished")
    toolCommand = `echo $$ > '${marker}'; sleep 30; echo done > '${finished}'`
    const tool = dispatch("run native bash")
    await until(() => existsSync(marker))
    const pid = Number(await readFile(marker, "utf8"))
    await runtime.interrupt(tool.id, new Error("stop tool"))
    assert.equal((await tool.result).status, "interrupted")
    await until(() => { try { process.kill(pid, 0); return false } catch { return true } })
    assert.equal(existsSync(finished), false)
    assert.equal((await dispatch("after native tool").result).status, "completed")
    // A steering callback cannot overtake the initial native input preflight.
    let orderingRelease!: () => void, orderingEntered = false
    inputGate = () => new Promise<void>(resolve => { orderingEntered = true; orderingRelease = resolve })
    const ordered = dispatch("ordered original marker")
    await until(() => orderingEntered)
    const beforeOrdering = calls.length
    const delivery = runtime.deliverMessage(ordered.id, "ordered-message", [{ type: "text", text: "ordered steer marker" }], { kind: "api_key" })
    await delay(20); assert.equal(calls.length, beforeOrdering)
    inputGate = undefined; orderingRelease()
    await delivery
    assert.equal((await ordered.result).status, "completed")
    assert.ok(calls[beforeOrdering]?.includes("ordered original marker"))
    // Cancellation while a native input extension is awaiting must remain joined.
    let release!: () => void, entered = false
    inputGate = () => new Promise<void>(resolve => { entered = true; release = resolve })
    const gated = dispatch("preflight cancellation")
    await until(() => entered)
    let stopped = false
    const stopping = runtime.interrupt(gated.id, new Error("stop preflight")).then(() => { stopped = true })
    await delay(20); assert.equal(stopped, false)
    inputGate = undefined; release()
    await stopping
    assert.equal((await gated.result).status, "interrupted")
    const count = calls.length
    await bridge!.close()
    options.recovery = "reconstructed"
    runtime = createRuntime()
    assert.equal((await dispatch("new input after reconstruction").result).status, "completed")
    assert.equal(calls.length, count + 1)
    assert.ok(calls.at(-1)?.includes("remember first input"))
    const beforeReplacement = (bridge as unknown as { session: AgentSession }).session.sessionManager.getSessionId()
    assert.equal((await dispatch("/fresh").result).status, "completed")
    assert.notEqual((bridge as unknown as { session: AgentSession }).session.sessionManager.getSessionId(), beforeReplacement)
    assert.equal((await dispatch("replacement remembered input").result).status, "completed")
    assert.ok(!calls.at(-1)?.includes("remember first input"))
    await bridge!.close(); runtime = createRuntime()
    assert.equal((await dispatch("replacement reopened").result).status, "completed")
    assert.ok(calls.at(-1)?.includes("replacement remembered input"))
    let replacementEntered = false, replacementRelease!: () => void
    replacementGate = () => new Promise<void>(resolve => { replacementEntered = true; replacementRelease = resolve })
    const replacing = dispatch("/fresh")
    await until(() => replacementEntered)
    let replacementStopped = false
    const replacementStopping = runtime.interrupt(replacing.id, new Error("stop replacement")).then(() => { replacementStopped = true })
    await delay(20); assert.equal(replacementStopped, false)
    replacementGate = undefined; replacementRelease(); await replacementStopping
    assert.equal((await replacing.result).status, "interrupted")
    assert.equal((await dispatch("after cancelled replacement").result).status, "completed")
    await bridge!.close(); bridge = undefined
    const manifest = join(options.stateDirectory, "test", "session.json")
    const saved = JSON.parse(await readFile(manifest, "utf8"))
    await writeFile(manifest, JSON.stringify({ ...saved, id: "wrong" }))
    await assert.rejects(PiSession.open(options), /identity/)
    await writeFile(manifest, JSON.stringify(saved)); await rm(saved.file)
    await assert.rejects(PiSession.open(options), /ENOENT/)
  } finally { await bridge?.close(); await rm(root, { recursive: true, force: true }) }
})

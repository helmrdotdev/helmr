import type { AskAnswer } from "../../../sdk/typescript/src/question"
// Real pinned native SDK/process, local model and in-memory control-plane driver.
// This exercises warm process identity, not VM or process-containment guarantees.
import assert from "node:assert/strict"
import { test } from "node:test"
import { mkdtemp, rm, stat } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { SessionRuntime, type SessionDriver } from "../../../runtime/typescript/src/agent-session"
import { conversation } from "../agents/issue-fixer/conversation"
import { ClaudeHarness } from "../agents/issue-fixer/claude-harness"
import { claudeModel } from "./native-model"

for (const scenario of ["answer", "background", "malformed", "handler-failure", "interrupt"]) test(`Claude warm Session: ${scenario}`, { timeout: 30_000 }, async () => {
  const blocked = scenario === "background"
  const directory = await mkdtemp(join(tmpdir(), "helmr-claude-session-"))
  const requests: any[] = []
  const responses: { turnId: string; value: unknown }[] = []
  let recoveryApprovalSent = false
  const recovers = ["malformed", "handler-failure", "interrupt"].includes(scenario)
  const http = claudeModel(requests, false, blocked ? [
    { id: "toolu_background", name: "Bash", input: { command: "printf forbidden > background-marker", run_in_background: true, description: "Must be denied" } },
    { id: "toolu_agent", name: "Agent", input: { description: "Must be denied", prompt: "Write background-marker", subagent_type: "general-purpose", run_in_background: true } },
  ] : recovers ? (body, index) => {
    if (index === 1) return { id: "toolu_question", name: "AskUserQuestion", input: { questions: [{ question: "Which option?", header: "Choice", multiSelect: false, options: [{ label: "A", description: "First" }, { label: "B", description: "Second" }] }] } }
    // Cancellation can consume a model response without admitting its tool.
    // Exercise approval on the successor Turn, whose input proves recovery.
    if (!recoveryApprovalSent && JSON.stringify(body.messages).includes("second unique input")) {
      recoveryApprovalSent = true
      return { id: "toolu_command", name: "Bash", input: { command: "printf fixture > denied-command-marker", description: "Write a disposable test marker" } }
    }
    return undefined
  } : undefined)
  const cancel = new AbortController()
  let questionReached!: () => void
  const questionReady = new Promise<void>(resolve => { questionReached = resolve })
  let harness: ClaudeHarness | undefined
  let holds = 0
  let setups = 0, questions = 0, approvals = 0, outputs = 0
  const deadline = setTimeout(() => cancel.abort(new Error("Fixture deadline")), 20000)
  try {
    await new Promise<void>(resolve => http.listen(0, "127.0.0.1", resolve))
    const port = (http.address() as { port: number }).port
    const environment = {
      PATH: process.env.PATH, HOME: directory,
      ANTHROPIC_BASE_URL: `http://127.0.0.1:${port}`, ANTHROPIC_API_KEY: "local-fixture",
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1", DISABLE_TELEMETRY: "1",
    }
    if (scenario === "answer") {
      const unstarted = await ClaudeHarness.open(directory, "same-session", environment)
      await unstarted.close()
      const reserved = await conversation(directory, "same-session", "claude")
      assert.notEqual(reserved.candidateId, undefined)
      assert.equal(reserved.id, undefined)
    }
    const driver: SessionDriver = {
      registerMessages: async () => {}, closeProcessing: async () => {},
      ask: async (_turn, _id, question) => {
        const kind = question.type === "choice" && question.options[0]?.id === "allow" ? "approval" : "answer"
        if (kind === "answer") questions++; else approvals++
        if (scenario === "handler-failure" && _turn === "first") {
          questionReached()
          return { answer: new Promise<AskAnswer>(() => {}), withdraw: async () => {} }
        }
        if (scenario === "interrupt" && _turn === "first") {
          void runtime.interrupt("first", new Error("Fixture interruption")).catch(() => {})
          return { answer: new Promise<AskAnswer>(() => {}), withdraw: async () => {} }
        }
        await assert.rejects(runtime.deliverMessage(_turn, `bad-steer-${questions}-${approvals}`, { text: "" } as never, { kind: "user" }), /Content must be an array/)
        return { answer: Promise.resolve<AskAnswer>({ answer: kind === "answer" ? (scenario === "malformed" ? { wrong: ["A"] } : { selected: [{ id: "option-1", value: "A" }] }) : { selected: [{ id: "deny", value: false }] }, answeredBy: { kind: "user", id: "fixture" } }), withdraw: async () => {} }
      },
      output: async () => ({ sequence: ++outputs }),
      respond: async (turnId, _id, value) => { responses.push({ turnId, value }) },
      confirmDrain: async () => {},
      finalize: async (_id, result) => ({ status: "completed", result }),
      fail: async (_id, error) => (error.code === "interrupted" ? { status: "interrupted" } : { status: "failed", error }), hold: async () => { holds++ },
    }
    const runtime = new SessionRuntime({
      kind: "agent", id: "native-probe", template: undefined!,
      init: async () => {
        setups++
        harness = await ClaudeHarness.open(directory, "same-session", environment)
        return harness
      },
      turn: async (turn: import("../../../sdk/typescript/src/agent").Turn, context) => {
        const running = (context.initResult as ClaudeHarness).run(turn, turn.input.map(part => part.text).join(""))
        if (scenario === "handler-failure" && turn.id === "first") {
          void running.catch(() => {})
          await questionReady
          throw new Error("Ordinary application failure")
        }
        const { response, ...result } = await running
        if (response !== undefined) await turn.respond(response)
        return result
      },
    }, { session: { id: "same-session" }, computer: { id: "computer" }, deployment: { id: "deployment" }, recovery: { kind: "initial" }, mcp: { getConnectionOptions: async () => { throw new Error("MCP is not configured in this fixture") } }, signal: cancel.signal }, driver, { failureConvergenceMs: 10_000 })
    const firstPending = runtime.dispatch({ id: "first", sequence: 1, createdAt: "2026-10-04T00:00:00Z", input: [{type:"text",text:"first unique input"}], source: { kind: "api_key" } })
    const first = await firstPending
    const processId = harness!.processId
    assert.equal(first.status, scenario === "interrupt" ? "interrupted" : ["handler-failure", "malformed"].includes(scenario) ? "failed" : "completed")
    assert.equal(runtime.idle, true)
    assert.ok(processId !== undefined && processId > 0)
    const second = await runtime.dispatch({ id: "second", sequence: 2, createdAt: "2026-10-04T00:00:01Z", input: [{type:"text",text:"second unique input"}], source: { kind: "api_key" } })
    assert.equal(second.status, "completed")
    assert.deepEqual(responses.find(value => value.turnId === "second")?.value, [{ type: "text", text: "native fixture response" }])
    if (["handler-failure", "malformed"].includes(scenario)) assert.equal(responses.some(value => value.turnId === "first"), false)
    assert.equal(harness!.processId, processId)
    assert.equal(setups, 1)
    assert.equal(holds, 0)
    assert.equal(questions, blocked ? 0 : 1)
    // The second model request may occur in the next Turn after interruption.
    assert.equal(approvals, blocked ? 0 : 1)
    await assert.rejects(stat(join(directory, "background-marker")), { code: "ENOENT" })
    await assert.rejects(stat(join(directory, "denied-command-marker")), { code: "ENOENT" })
    assert.equal(runtime.idle, true)
    const history = JSON.stringify(requests.at(-1).messages)
    assert.ok(history.includes("first unique input"))
    assert.ok(history.includes("second unique input"))
  } finally {
    clearTimeout(deadline)
    cancel.abort()
    await harness?.close()
    http.closeAllConnections()
    await new Promise<void>(resolve => http.close(() => resolve()))
    await rm(directory, { recursive: true, force: true })
  }
})

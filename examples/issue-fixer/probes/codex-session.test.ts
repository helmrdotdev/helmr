import type { AskResponse } from "../../../sdk/typescript/src/question"
// Uses the real pinned native app-server and SessionRuntime with a local model.
// Guest containment, durable control-plane state and VM restore are separate gates.
import assert from "node:assert/strict"
import { test } from "node:test"
import { mkdtemp, rm, writeFile, readFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { SessionRuntime, type SessionDriver } from "../../../runtime/typescript/src/agent-session"
import { CodexHarness } from "../agents/issue-fixer/codex-harness"
import { conversation } from "../agents/issue-fixer/conversation"
import { codexModel } from "./native-model"

for (const scenario of ["answer", "malformed", "handler-failure", "background-exec", "interrupt"]) test(`Codex warm Session: ${scenario}`, { timeout: 30_000 }, async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-session-"))
  const requests: any[] = []
  const responses: { turnId: string; value: unknown }[] = []
  const http = codexModel(requests, false, false, scenario === "background-exec" ? (_body,index) => index === 1
    ? { type:"function_call",id:"fc_exec",call_id:"call_exec",name:"exec_command",arguments:JSON.stringify({cmd:"sh -c 'echo $$ > owned-command.pid; exec sleep 60'",yield_time_ms:100}) }
    : index === 2 ? { type:"function_call",id:"fc_question",call_id:"call_question",name:"request_user_input",arguments:JSON.stringify({questions:[{id:"choice",header:"Choice",question:"Continue?",options:[{label:"A",description:"First"},{label:"B",description:"Second"}]}]}) } : undefined : undefined)
  const cancel = new AbortController()
  let questionReached!: () => void
  const questionReady = new Promise<void>(resolve => { questionReached = resolve })
  let harness: CodexHarness | undefined
  let holds = 0
  let setups = 0, questions = 0, outputs = 0
  try {
    await new Promise<void>(resolve => http.listen(0, "127.0.0.1", resolve))
    const port = (http.address() as { port: number }).port
    const saved = await conversation(directory, "same-session", "codex")
    await writeFile(join(saved.directory, "config.toml"), `model_provider = "fixture"\nmodel = "gpt-5.1-codex"\n[model_providers.fixture]\nname = "local fixture"\nbase_url = "http://127.0.0.1:${port}/v1"\nwire_api = "responses"\nrequires_openai_auth = false\n`)
    const driver: SessionDriver = {
      registerMessages: async () => {}, closeProcessing: async () => {},
      ask: async (turnId) => {
        questions++
        if (scenario === "handler-failure" && turnId === "first") {
          questionReached()
          return { answer: new Promise<AskResponse>(() => {}), withdraw: async () => {} }
        }
        if ((scenario === "background-exec" || scenario === "interrupt") && turnId === "first") {
          void runtime.interrupt("first", new Error("Fixture interruption")).catch(() => {})
          return { answer: new Promise<AskResponse>(() => {}), withdraw: async () => {} }
        }
        await assert.rejects(runtime.deliverMessage(turnId, "bad-steer", { text: "" } as never, { kind: "user" }), /Content must be an array/)
        return { answer: Promise.resolve<AskResponse>({ answer: scenario === "malformed" ? { wrong: ["A"] } : { selected: [{ id: "option-1", value: "A" }] }, respondedBy: { kind: "user", id: "fixture" } }), withdraw: async () => {} }
      },
      output: async () => ({ sequence: ++outputs }),
      respond: async (turnId, _id, value) => { responses.push({ turnId, value }) },
      confirmDrain: async () => {},
      finalize: async (_id, result) => ({ status: "completed", result }),
      fail: async (_id, error) => ({ status: error.code === "interrupted" ? "interrupted" : "failed", error }),
      hold: async () => { holds++ },
    }
    const runtime = new SessionRuntime({
      kind: "agent", id: "native-probe", template: undefined!,
      init: async () => { setups++; harness = await CodexHarness.open(directory, "same-session", { PATH: process.env.PATH, HOME: directory }); return harness },
      turn: async (turn, context) => {
        const running = (context.initResult as CodexHarness).run(turn, turn.input.map(part => part.text).join(""))
        if (scenario === "handler-failure" && turn.id === "first") {
          void running.catch(() => {})
          await questionReady
          throw new Error("Ordinary application failure")
        }
        return running
      },
    }, { session: { id: "same-session" }, computer: { id: "computer" }, deployment: { id: "deployment" }, recovery: { kind: "initial" }, mcp: { getConnectionOptions: async () => { throw new Error("MCP is not configured in this fixture") } }, signal: cancel.signal }, driver, { failureConvergenceMs: 10_000 })
    const firstPending = runtime.dispatch({ id: "first", sequence: 1, createdAt: "2026-10-04T00:00:00Z", input: [{type:"text",text:"first unique input"}], source: { kind: "api" } })
    const first = await firstPending
    if (scenario === "background-exec") {
      const commandPid = Number((await readFile(join(directory,"owned-command.pid"),"utf8")).trim())
      assert.ok(Number.isSafeInteger(commandPid) && commandPid>0)
      assert.throws(() => process.kill(commandPid,0))
    }
    const processId = harness!.processId
    assert.equal(first.status, (scenario === "interrupt" || scenario === "background-exec") ? "interrupted" : ["handler-failure", "malformed"].includes(scenario) ? "failed" : "completed")
    assert.equal(runtime.idle, true)
    assert.ok(processId !== undefined && processId > 0)
    const second = await runtime.dispatch({ id: "second", sequence: 2, createdAt: "2026-10-04T00:00:01Z", input: [{type:"text",text:"second unique input"}], source: { kind: "api" } })
    assert.equal(second.status, "completed")
    assert.deepEqual(responses.find(value => value.turnId === "second")?.value, [{ type: "text", text: "fixture response" }])
    if (["handler-failure", "malformed"].includes(scenario)) assert.equal(responses.some(value => value.turnId === "first"), false)
    assert.equal(harness!.processId, processId)
    assert.equal(setups, 1)
    assert.equal(holds, 0)
    assert.equal(questions, 1)
    assert.equal(runtime.idle, true)
    const history = JSON.stringify(requests.at(-1).input)
    assert.ok(history.includes("first unique input"))
    assert.ok(history.includes("second unique input"))
  } finally {
    cancel.abort()
    await harness?.close()
    http.closeAllConnections()
    await new Promise<void>(resolve => http.close(() => resolve()))
    // Native plugin-cache helpers can finish filesystem cleanup after app-server exit.
    await rm(directory, { recursive: true, force: true, maxRetries: 3, retryDelay: 100 })
  }
})

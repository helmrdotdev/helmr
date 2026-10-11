import type { AskResponse } from "../../../sdk/typescript/src/question"
// Uses the real pinned native app-server and SessionRuntime with a local model.
// Guest containment, durable control-plane state and VM restore are separate gates.
import { NativeContinuationLost } from "../../../runtime/typescript/src/agent-native"
import { localNativeRuntime } from "./native-runtime"
import assert from "node:assert/strict"
import { test } from "node:test"
import { mkdtemp, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { SessionRuntime, type SessionDriver } from "../../../runtime/typescript/src/agent-session"
import { CodexHarness } from "../tasks/issue-fixer/codex-harness"
import { conversation } from "../tasks/issue-fixer/conversation"
import { codexModel } from "./native-model"

for (const scenario of ["answer", "malformed", "handler-failure", "stopped-process"]) test(`Codex warm Session: ${scenario}`, { timeout: 30_000 }, async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-native-session-"))
  const requests: any[] = []
  const responses: { turnId: string; value: unknown }[] = []
  const http = codexModel(requests, false)
  const cancel = new AbortController()
  let native: ReturnType<typeof localNativeRuntime> | undefined
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
    native = localNativeRuntime(cancel.signal)
    const driver: SessionDriver = {
      registerMessages: async () => {}, closeProcessing: async () => {},
      ask: async (turnId) => {
        questions++
        if (scenario === "handler-failure" && turnId === "first") {
          questionReached()
          return { answer: new Promise<AskResponse>(() => {}), withdraw: async () => {} }
        }
        if (scenario === "stopped-process") {
          process.kill(harness!.processId!, "SIGSTOP")
          runtime.interrupt("first", new Error("Fixture interruption"))
          return { answer: new Promise<AskResponse>(() => {}), withdraw: async () => {} }
        }
        await assert.rejects(runtime.deliverMessage(turnId, "bad-steer", { text: "" } as never), /Content must be an array/)
        return { answer: Promise.resolve<AskResponse>({ answer: scenario === "malformed" ? { wrong: ["A"] } : { selected: [{ id: "option-1", value: "A" }] }, respondedBy: { kind: "user", id: "fixture" } }), withdraw: async () => {} }
      },
      output: async () => ({ sequence: ++outputs }),
      respond: async (turnId, _id, value) => { responses.push({ turnId, value }) },
      convergeNative: async (turnId, disposition, signal) => {
        await native!.registry.join(turnId, disposition, signal.reason)
        const evidence = await native!.registry.evidence()
        if (!evidence.reusable) throw new NativeContinuationLost()
      },
      finalize: async (_id, result) => ({ status: "completed", result }),
      fail: async (_id, error) => ({ status: error.code === "interrupted" ? "interrupted" : "failed", error }),
      hold: async () => { holds++ },
    }
    const runtime = new SessionRuntime({
      kind: "agent", id: "native-probe", computer: undefined!,
      setup: async () => { setups++; harness = await CodexHarness.open(directory, "same-session", { PATH: process.env.PATH, HOME: directory }); return harness },
      turn: async (turn, context) => {
        const running = (context.setupResult as CodexHarness).run(turn, turn.input.map(part => part.text).join(""))
        if (scenario === "handler-failure" && turn.id === "first") {
          void running.catch(() => {})
          await questionReady
          throw new Error("Ordinary application failure")
        }
        return running
      },
    }, { session: { id: "same-session" }, computer: { id: "computer" }, deployment: { id: "deployment" }, recovery: { kind: "initial" }, signal: cancel.signal }, driver, { failureConvergenceMs: 10_000, native: native.registry })
    const firstPending = runtime.dispatch({ id: "first", sequence: 1, createdAt: "2026-10-04T00:00:00Z", input: [{type:"text",text:"first unique input"}], source: { kind: "api" } })
    if (scenario === "stopped-process") {
      assert.equal((await firstPending).status, "interrupted")
      assert.equal(holds, 1)
      assert.equal(runtime.idle, false)
      assert.throws(() => process.kill(harness!.processId!, 0))
      return
    }
    const first = await firstPending
    const processId = harness!.processId
    assert.equal(first.status, ["handler-failure", "malformed"].includes(scenario) ? "failed" : "completed")
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
    native?.uninstall()
    http.closeAllConnections()
    await new Promise<void>(resolve => http.close(() => resolve()))
    await rm(directory, { recursive: true, force: true })
  }
})
